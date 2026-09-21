// Package fetcher (pool.go) implements per-pool HTTP fetching + validation
// + in-memory retry. One PoolFetcher represents the work done for a single pool
// in a single cycle.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/ipfilter"
)

// WorkItem is the canonical input to a fetch attempt. It is the same as
// storage.WorkItem so we don't import-cycle. We mirror it here to keep the
// fetcher package self-contained for testing.
type WorkItem struct {
	PoolID         int64
	PoolUpdateID   int64
	URL            string
	OnChainHash    []byte
	LastPolled     *time.Time
	LastPoolUpdate *int64
}

// Result is the outcome of one fetch attempt.
type Result struct {
	PoolID       int64
	PoolUpdateID int64
	URL          string

	Validated *ValidationResult // nil on validation failure
	Err       error             // non-nil on HTTP / validate failure; never both non-nil

	HTTPStatusCode int
	ContentType    string
	BytesRead      int
	Duration       time.Duration
}

// Succeeded returns true when the fetch + validation succeeded.
func (r *Result) Succeeded() bool { return r.Validated != nil && r.Err == nil }

// PoolFetcher executes one WorkItem (or many, in series) and tracks per-pool
// retry counters.
type PoolFetcher struct {
	httpClient *http.Client
	cfg        ValidationConfig
	logger     *slog.Logger
	ua         string

	mu       sync.Mutex
	retries  map[int64]uint // pool_id -> consecutive failure count
	mismatch map[int64]bool // pool_id -> hash_mismatch already logged this cycle
}

// SetUserAgent overrides the default "koios-pool-meta-fetcher/dev" User-Agent.
func (p *PoolFetcher) SetUserAgent(s string) {
	if s != "" {
		p.ua = s
	}
}

// NewPoolFetcher constructs a fetcher with the given HTTP client and validation
// configuration.
func NewPoolFetcher(client *http.Client, cfg ValidationConfig, logger *slog.Logger) *PoolFetcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PoolFetcher{
		httpClient: client,
		cfg:        cfg,
		logger:     logger.With(slog.String("subsystem", "fetcher.pool")),
		ua:         "koios-pool-meta-fetcher/dev",
		retries:    map[int64]uint{},
		mismatch:   map[int64]bool{},
	}
}

// RetryCount returns the current consecutive-failure count for the pool.
func (p *PoolFetcher) RetryCount(poolID int64) uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retries[poolID]
}

// MaxAttempts returns the configured per-pool retry cap.
func (p *PoolFetcher) MaxAttempts() uint { return uint(p.cfgHashMaxAttempts()) }

func (p *PoolFetcher) cfgHashMaxAttempts() int {
	// Mirrors config.Config.Retry.MaxAttempts — the fetcher only needs the int.
	// Kept here because validators.ValidationConfig does not carry retry info.
	return defaultRetryMax
}

const defaultRetryMax = 10

// ShouldSkip returns true when the pool has been retried cfg.MaxAttempts times
// without success and should be excluded this cycle.
func (p *PoolFetcher) ShouldSkip(w WorkItem) bool {
	return p.RetryCount(w.PoolID) >= p.MaxAttempts()
}

// Fetch performs one HTTP GET, applies the validation pipeline, records the
// outcome, and returns a *Result. Context is honoured for cancellation.
//
// The HTTP client MUST already be configured with the IP filter (see
// internal/ipfilter.WrapTransport).
func (p *PoolFetcher) Fetch(ctx context.Context, w WorkItem) *Result {
	start := time.Now()

	// Attach per-pool context for IP-filter exemption check.
	ctx = context.WithValue(ctx, ipfilter.PoolIDKey{}, w.bech32())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.URL, nil)
	if err != nil {
		return p.record(w, &Result{Err: fmt.Errorf("invalid url: %w", err), Duration: time.Since(start)})
	}
	req.Header.Set("User-Agent", p.UserAgent())
	req.Header.Set("Accept", "application/json, application/ld+json, text/plain;q=0.9, */*;q=0.1")

	httpResult := &Result{
		PoolID:       w.PoolID,
		PoolUpdateID: w.PoolUpdateID,
		URL:          w.URL,
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		httpResult.Err = fmt.Errorf("http do: %w", err)
		httpResult.Duration = time.Since(start)
		return p.record(w, httpResult)
	}
	defer resp.Body.Close()

	httpResult.HTTPStatusCode = resp.StatusCode
	httpResult.ContentType = resp.Header.Get("Content-Type")

	// Read body up to max+1 to detect overflow.
	maxBytes := p.cfg.MaxBodyBytes
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		httpResult.Err = fmt.Errorf("read body: %w", err)
		httpResult.Duration = time.Since(start)
		return p.record(w, httpResult)
	}
	httpResult.BytesRead = len(body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpResult.Err = fmt.Errorf("status %d", resp.StatusCode)
		httpResult.Duration = time.Since(start)
		return p.record(w, httpResult)
	}

	validated, err := Validate(body, httpResult.ContentType, w.OnChainHash, p.cfg)
	if err != nil {
		httpResult.Err = err
		httpResult.Duration = time.Since(start)
		return p.record(w, httpResult)
	}

	httpResult.Validated = validated
	httpResult.Duration = time.Since(start)

	// Informational: log hash mismatch once per cycle. We can do it here.
	if !validated.HashMatched && p.shouldLogMismatch(w) {
		p.logger.Info("hash_mismatch",
			slog.Int64("pool_id", w.PoolID),
			slog.String("url", w.URL),
			slog.String("declared_hash", fmt.Sprintf("%x", w.OnChainHash)),
			slog.String("actual_hash", fmt.Sprintf("%x", validated.MetaHash)),
		)
	}

	return p.record(w, httpResult)
}

// record centralises retry-counter bookkeeping. On success the counter resets;
// on failure it increments up to MaxAttempts.
func (p *PoolFetcher) record(w WorkItem, r *Result) *Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Succeeded() {
		delete(p.retries, w.PoolID)
	} else {
		p.retries[w.PoolID]++
	}
	return r
}

// ShouldLogMismatch returns true when a hash_mismatch log should be emitted
// for the pool this cycle. Deduplicated in-memory: at most once per pool until
// the daemon restarts. This avoids log spam for pools with mismatched hashes.
func (p *PoolFetcher) shouldLogMismatch(w WorkItem) bool {
	// We piggyback on the retries map: presence means "already logged this cycle".
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.retries[w.PoolID]; seen {
		return false
	}
	p.retries[w.PoolID] = 0 // mark as seen; will be overwritten if a real retry happens
	return true
}

// UserAgent returns the configured User-Agent string.
func (p *PoolFetcher) UserAgent() string {
	if p.ua == "" {
		return "koios-pool-meta-fetcher/dev"
	}
	return p.ua
}

// bech32 returns the pool's bech32 ID for ipfilter context. Currently empty;
// callers should enrich WorkItem with the bech32 string upstream (Phase 4).
func (w WorkItem) bech32() string { return "" }

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// ErrValidation is exported for external matchers that prefer errors.As.
var ErrValidation = errors.New("validation failed")
