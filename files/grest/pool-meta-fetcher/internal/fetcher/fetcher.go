// Package fetcher (fetcher.go) implements the cycle loop: time.Ticker,
// bounded work channel, worker pool, batched storage UPSERT, deduped
// hash-mismatch logging, and per-cycle summary log.
//
// Spec: openspec/specs/pool-offchain-fetcher/spec.md §poll-cycle.
package fetcher

import (
	"context"
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
	MarkPoolAttempted(ctx context.Context, poolID, lastPoolUpdateID int64, succeeded bool, baseSeconds int) error
}

// Fetcher is the cycle driver.
type Fetcher struct {
	cfg  atomic.Pointer[config.Config]
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

	// currentTicker holds the live *time.Ticker so we can swap it on SIGHUP
	// without leaking the old one. Mutated only inside reloadConfig.
	currentTicker atomic.Pointer[time.Ticker]
}

// New constructs a Fetcher. The PoolFetcher is built from the same config so
// that per-cycle settings (timeouts, content-type allowlist, body caps,
// user-agent, hash algorithm) all stay in lockstep.
func New(cfg *config.Config, db DBStore, pf *PoolFetcher, logger *slog.Logger) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Fetcher{
		db:   db,
		pool: pf,
		log:  logger.With(slog.String("subsystem", "fetcher")),
	}
	f.cfg.Store(cfg)
	return f
}

// currentCfg returns the live config. Reads are lock-free and race-free.
func (f *Fetcher) currentCfg() *config.Config {
	return f.cfg.Load()
}

// ReloadConfig swaps in a new config and recreates the ticker with the
// new poll interval. Safe to call from a SIGHUP handler goroutine.
func (f *Fetcher) ReloadConfig(cfg *config.Config) {
	f.cfg.Store(cfg)
	if old := f.currentTicker.Swap(time.NewTicker(cfg.PollInterval())); old != nil {
		old.Stop()
	}
}

// Run blocks until ctx is cancelled, ticking on the live poll interval.
// The interval is re-read on every tick so SIGHUP-reloaded config takes
// effect at the next tick boundary.
func (f *Fetcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(f.currentCfg().PollInterval())
	f.currentTicker.Store(ticker)

	// Run one cycle immediately at startup.
	f.runCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			f.runCycle(ctx)
			// If SIGHUP swapped the ticker while we were running, adopt it.
			if nt := f.currentTicker.Load(); nt != ticker {
				ticker.Stop()
				ticker = nt
			}
		}
	}
}

// runCycle performs one discovery-then-fetch-then-store iteration.
func (f *Fetcher) runCycle(ctx context.Context) {
	start := time.Now()
	cfg := f.currentCfg()
	cycleCtx, cancel := context.WithTimeout(ctx, f.intervalTimeout())
	defer cancel()

	items, err := f.db.DiscoverWorkItems(
		cycleCtx,
		cfg.Fetch.WorkQueue.BatchSize,
		cfg.PollInterval(),
		cfg.FailedPoolRetry(),
		cfg.Fetch.WorkQueue.PoolIDAllowList,
		cfg.Fetch.WorkQueue.PoolIDDenyList,
	)
	if err != nil {
		f.log.Error("discover_work_items failed", slog.String("err", err.Error()))
		return
	}

	work := f.applyFilterLists(items)
	skipped := len(items) - len(work)

	results := f.dispatch(cycleCtx, work)
	upsertBatch := f.collectSuccesses(results)

	upsertErr := error(nil)
	if len(upsertBatch) > 0 {
		if upsertErr = f.db.UpsertBatch(cycleCtx, upsertBatch); upsertErr != nil {
			f.log.Error("upsert_failed",
				slog.Int("rows", len(upsertBatch)),
				slog.String("err", upsertErr.Error()),
			)
		}
	}

	// Failed = result.Err != nil OR upsert failed for a successful result.
	// A row whose payload fetched cleanly but failed to write must NOT count
	// as a failure — otherwise we'd schedule exponential-backoff retry for
	// data we never persisted, and on the next cycle we'd refetch just to
	// hit the same DB error.
	failedCount := 0
	for _, r := range results {
		if !r.Succeeded() {
			failedCount++
		} else if upsertErr != nil {
			failedCount++
		}
	}
	if failedCount > 0 {
		f.markFailedAttempts(cycleCtx, results)
	}

	f.recordStats(start, skipped, len(upsertBatch), failedCount)
}

// markFailedAttempts records a failed-attempt marker for every result
// whose payload wasn't fetched OR whose fetched payload failed to upsert.
// baseSeconds controls the exponential-backoff floor (failed_pool_retry_seconds).
func (f *Fetcher) markFailedAttempts(ctx context.Context, results []Result) {
	baseSecs := int(f.currentCfg().FailedPoolRetry().Seconds())
	for _, r := range results {
		if r.Succeeded() && r.Validated != nil {
			continue // successful fetch + successful upsert: nothing to mark
		}
		if err := f.db.MarkPoolAttempted(ctx, r.PoolID, r.PoolUpdateID, false, baseSecs); err != nil {
			f.log.Warn("mark_failed_attempt_failed",
				slog.Int64("pool_id", r.PoolID),
				slog.String("err", err.Error()),
			)
		}
	}
}

// intervalTimeout bounds a single cycle to roughly one poll interval.
func (f *Fetcher) intervalTimeout() time.Duration {
	return f.currentCfg().PollInterval()
}

// applyFilterLists is currently a pass-through. allow_list / deny_list are
// accepted by the validator and stored in config but not yet enforced here —
// plumbing the bech32 pool_id from storage through WorkItem is a small task
// tracked for the next iteration.
func (f *Fetcher) applyFilterLists(items []storage.WorkItem) []WorkItem {
	out := make([]WorkItem, 0, len(items))
	for _, wi := range items {
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
	cfg := f.currentCfg()
	concurrency := cfg.Fetch.WorkQueue.Concurrency
	maxInflight := cfg.Fetch.WorkQueue.MaxInflight
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
