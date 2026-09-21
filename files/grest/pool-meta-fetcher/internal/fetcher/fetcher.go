// Package fetcher (fetcher.go) implements the cycle loop: time.Ticker,
// bounded work channel, worker pool, batched storage UPSERT, deduped
// hash-mismatch logging, and per-cycle summary log.
//
// Spec: openspec/specs/pool-offchain-fetcher/spec.md §poll-cycle.
package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/config"
	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/storage"
)

// DBStore is the storage surface the fetcher needs. It is implemented by
// *storage.Pool in production and by fakes in tests.
type DBStore interface {
	DiscoverWorkItems(ctx context.Context, batchSize int, pollStaleAfter, failedRetryAfter time.Duration, allowList, denyList []string) ([]storage.WorkItem, error)
	UpsertBatch(ctx context.Context, batch []storage.PoolMetadata) error
	MarkPoolAttempted(ctx context.Context, poolID, lastPoolUpdateID int64, succeeded bool) error
}

// Fetcher is the cycle driver.
type Fetcher struct {
	cfg  *config.Config
	db   DBStore
	pool *PoolFetcher
	log  *slog.Logger

	// Per-cycle summaries (exposed at /debug/pools when enabled).
	statsLk     sync.Mutex
	lastCycleAt time.Time
	lastSucc    atomic.Uint64
	lastFail    atomic.Uint64
	lastSkip    atomic.Uint64
	lastDurMS   atomic.Int64
}

// New constructs a Fetcher. The PoolFetcher is built from the same config so
// that per-cycle settings (timeouts, content-type allowlist, body caps,
// user-agent, hash algorithm) all stay in lockstep.
func New(cfg *config.Config, db DBStore, pf *PoolFetcher, logger *slog.Logger) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Fetcher{
		cfg:  cfg,
		db:   db,
		pool: pf,
		log:  logger.With(slog.String("subsystem", "fetcher")),
	}
}

// Run blocks until ctx is cancelled, ticking on cfg.Fetch.Poll.PollInterval.
// On each tick it triggers one cycle and then waits for the next tick.
// Returns nil on graceful shutdown (ctx.Err()).
func (f *Fetcher) Run(ctx context.Context) error {
	interval := f.cfg.PollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run one cycle immediately at startup (matching db-sync behaviour).
	f.runCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			f.runCycle(ctx)
		}
	}
}

// runCycle performs one discovery-then-fetch-then-store iteration. The body
// is split into small helpers so each phase is testable in isolation.
func (f *Fetcher) runCycle(ctx context.Context) {
	start := time.Now()
	cycleCtx, cancel := context.WithTimeout(ctx, f.intervalTimeout())
	defer cancel()

	items, err := f.db.DiscoverWorkItems(
		cycleCtx,
		f.cfg.Fetch.WorkQueue.BatchSize,
		f.cfg.PollInterval(),
		f.cfg.FailedPoolRetry(),
		f.cfg.Fetch.WorkQueue.PoolIDAllowList,
		f.cfg.Fetch.WorkQueue.PoolIDDenyList,
	)
	if err != nil {
		f.log.Error("discover_work_items failed", slog.String("err", err.Error()))
		return
	}

	// Honour per-pool allow/deny lists + pools_overrides (Phase 4 work: glob).
	work := f.applyFilterLists(items)
	skipped := len(items) - len(work)

	results := f.dispatch(cycleCtx, work)
	upsertBatch := f.collectSuccesses(results)

	if len(upsertBatch) > 0 {
		if err := f.db.UpsertBatch(cycleCtx, upsertBatch); err != nil {
			f.log.Error("upsert_failed",
				slog.Int("rows", len(upsertBatch)),
				slog.String("err", err.Error()),
			)
		}
	}

	// Persist per-pool failure marker so the next cycle knows which pools
	// to re-attempt at the short retry cadence.
	failedCount := len(work) - len(upsertBatch)
	if failedCount > 0 {
		f.markFailedAttempts(cycleCtx, results)
	}

	f.recordStats(start, skipped, len(upsertBatch), failedCount)
}

// markFailedAttempts calls MarkPoolAttempted(false) for each result that
// did not succeed; best-effort — failures here are logged but don't abort
// the cycle.
func (f *Fetcher) markFailedAttempts(ctx context.Context, results []Result) {
	for _, r := range results {
		if r.Succeeded() {
			continue
		}
		if err := f.db.MarkPoolAttempted(ctx, r.PoolID, r.PoolUpdateID, false); err != nil {
			f.log.Warn("mark_failed_attempt_failed",
				slog.Int64("pool_id", r.PoolID),
				slog.String("err", err.Error()),
			)
		}
	}
}

// intervalTimeout bounds a single cycle to roughly one poll interval.
func (f *Fetcher) intervalTimeout() time.Duration {
	return f.cfg.PollInterval()
}

// applyFilterLists filters out pool IDs present in the deny list (and any
// not in the allow list, when the allow list is non-empty). Returns only the
// pools the cycle should actually attempt.
func (f *Fetcher) applyFilterLists(items []storage.WorkItem) []WorkItem {
	allowed := map[string]bool{}
	for _, p := range f.cfg.Fetch.WorkQueue.PoolIDAllowList {
		allowed[p] = true
	}
	denied := map[string]bool{}
	for _, p := range f.cfg.Fetch.WorkQueue.PoolIDDenyList {
		denied[p] = true
	}

	out := make([]WorkItem, 0, len(items))
	for _, wi := range items {
		// NB: storage.WorkItem doesn't carry the bech32 yet (Phase 4). Filter
		// is effectively a no-op for now; will be wired with the bech32 once
		// DiscoverWorkItems returns it.
		_ = allowed
		_ = denied
		out = append(out, WorkItem{
			PoolID:         wi.PoolID,
			PoolUpdateID:   wi.PoolUpdateID,
			URL:            wi.URL,
			OnChainHash:    wi.OnChainHash,
			LastPolled:     wi.LastPolled,
			LastPoolUpdate: wi.LastPoolUpdate,
		})
	}
	return out
}

// dispatch runs fetch workers in parallel against the bounded channel.
func (f *Fetcher) dispatch(ctx context.Context, work []WorkItem) []Result {
	if len(work) == 0 {
		return nil
	}
	concurrency := f.cfg.Fetch.WorkQueue.Concurrency
	maxInflight := f.cfg.Fetch.WorkQueue.MaxInflight
	if concurrency < 1 {
		concurrency = 1
	}
	if maxInflight < concurrency {
		maxInflight = concurrency
	}
	queue := make(chan WorkItem, maxInflight)
	results := make(chan Result, maxInflight)

	// Feed the queue.
	go func() {
		defer close(queue)
		for _, w := range work {
			select {
			case <-ctx.Done():
				return
			case queue <- w:
			}
		}
	}()

	// Workers.
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range queue {
				if ctx.Err() != nil {
					return
				}
				if f.pool.ShouldSkip(w) {
					f.log.Debug("pool_skipped_max_retries",
						slog.Int64("pool_id", w.PoolID),
						slog.Uint64("retries", uint64(f.pool.RetryCount(w.PoolID))),
					)
					results <- Result{PoolID: w.PoolID, Err: fmt.Errorf("max retries: %d", f.pool.RetryCount(w.PoolID))}
					continue
				}
				results <- *f.pool.Fetch(ctx, w)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var out []Result
	for r := range results {
		out = append(out, r)
	}
	return out
}

// collectSuccesses returns the rows ready for batched UPSERT.
func (f *Fetcher) collectSuccesses(results []Result) []storage.PoolMetadata {
	now := time.Now().UTC()
	out := make([]storage.PoolMetadata, 0, len(results))
	for _, r := range results {
		if !r.Succeeded() {
			continue
		}
		meta := storage.PoolMetadata{
			PoolID:         r.PoolID,
			MetaURL:        r.URL,
			MetaHash:       r.Validated.MetaHash,
			MetaJSON:       r.Validated.JSON,
			MetaBytes:      r.Validated.Body,
			LastPolled:     now,
			LastPoolUpdate: r.PoolUpdateID,
			IsValid:        true,
		}
		out = append(out, meta)
	}
	return out
}

// recordStats updates the /debug/pools snapshot and emits the per-cycle summary.
func (f *Fetcher) recordStats(start time.Time, skipped, succeeded, failed int) {
	durMS := time.Since(start).Milliseconds()
	f.statsLk.Lock()
	f.lastCycleAt = start
	f.statsLk.Unlock()
	f.lastSucc.Store(uint64(succeeded))
	f.lastFail.Store(uint64(failed))
	f.lastSkip.Store(uint64(skipped))
	f.lastDurMS.Store(durMS)

	f.log.Info("cycle_summary",
		slog.Int("succeeded", succeeded),
		slog.Int("failed", failed),
		slog.Int("skipped", skipped),
		slog.Int64("duration_ms", durMS),
	)
}

// Snapshot returns a JSON-serialisable summary of the most recent cycle for
// /debug/pools. Safe to call concurrently.
func (f *Fetcher) Snapshot() map[string]any {
	f.statsLk.Lock()
	last := f.lastCycleAt
	f.statsLk.Unlock()
	return map[string]any{
		"last_cycle_at": last.Format(time.RFC3339Nano),
		"succeeded":     f.lastSucc.Load(),
		"failed":        f.lastFail.Load(),
		"skipped":       f.lastSkip.Load(),
		"duration_ms":   f.lastDurMS.Load(),
	}
}

// SnapshotJSON returns the snapshot encoded as JSON (handy for health.SnapshotDebugPools).
func (f *Fetcher) SnapshotJSON() any {
	b, _ := json.Marshal(f.Snapshot())
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
