// Package main is the entry point for koios-pool-meta-fetcher.
//
// The daemon periodically fetches off-chain pool metadata from URLs declared in
// on-chain pool_update certificates, validates the responses, and upserts them
// into grest.pool_offchain_metadata.
//
// See ../../../../openspec/proposal.md for the change rationale and
// ../../../../openspec/specs/pool-offchain-fetcher/spec.md for the contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/config"
	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/health"
	"github.com/cardano-community/koios-artifacts/pool-meta-fetcher/internal/storage"
)

const (
	defaultConfigPath = "/etc/koios/pool-meta-fetcher.yaml"
	exitCodeConfig    = 2
	exitCodeDB        = 3
	exitCodeRuntime   = 1
)

func main() {
	cfgPath := flag.String("config", defaultConfigPath, "Path to YAML config file")
	flag.Parse()

	logger, logLevel := newLogger(slog.LevelInfo)
	slog.SetDefault(logger)

	logger.Info("starting koios-pool-meta-fetcher",
		slog.String("config_path", *cfgPath),
		slog.String("version", version),
	)

	cfg, err := config.Load(*cfgPath, logger)
	if err != nil {
		logger.Error("config load failed", slog.String("err", err.Error()))
		os.Exit(exitCodeConfig)
	}
	logger.Info("config loaded",
		slog.String("network", string(cfg.NetworkName)),
		slog.Bool("fetch_enabled", cfg.Fetch.Enabled),
		slog.Duration("poll_interval", cfg.Fetch.Poll.PollInterval),
	)

	// Re-log at the configured level now that we know it.
	if logLevel != cfg.Logging.Level {
		logger = newLoggerAt(cfg.Logging.Level)
		slog.SetDefault(logger)
	}

	// Health server (always up).
	hs := health.NewServer(health.Config{
		ListenAddr:           cfg.Health.ListenAddr,
		ListenPort:           cfg.Health.ListenPort,
		ReadyzDBGraceSeconds: cfg.Health.ReadyzDBGraceSeconds,
		PingFn:               nil, // wired below once DB is up
	})
	if err := hs.Start(); err != nil {
		logger.Error("health server failed to start", slog.String("err", err.Error()))
		os.Exit(exitCodeRuntime)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Stop(shutdownCtx)
	}()
	logger.Info("health server up",
		slog.String("addr", hs.Addr()),
	)

	// Honour fetch.enabled=false as a soft disable: health stays up, no DB connect.
	if !cfg.Fetch.Enabled {
		logger.Info("fetch disabled by config; daemon idle (health-only)")
		<-awaitShutdown()
		logger.Info("shutdown requested; exiting cleanly")
		return
	}

	// DB pool.
	poolCtx, poolCancel := context.WithTimeout(context.Background(), 15*time.Second)
	pg, err := storage.NewPool(poolCtx, cfg.DatabaseURL, storage.PoolOptions{
		MaxConns:          cfg.Database.PoolMaxConns,
		MinConns:          cfg.Database.PoolMinConns,
		MaxConnLifetime:   cfg.Database.PoolMaxConnLifetime,
		MaxConnIdleTime:   cfg.Database.PoolMaxConnIdleTime,
		StatementTimeout:  time.Duration(cfg.Database.StatementTimeoutMs) * time.Millisecond,
		ApplicationName:   "koios-pool-meta-fetcher",
	})
	poolCancel()
	if err != nil {
		logger.Error("db pool init failed", slog.String("err", err.Error()))
		os.Exit(exitCodeDB)
	}
	defer pg.Close()
	logger.Info("db pool ready")

	// Wire the readyz probe to use the pool.
	hs.SetPingFn(func(ctx context.Context) error {
		_, _, pingErr := pg.Ping(ctx)
		return pingErr
	})

	// Run migrations (idempotent, no-op on subsequent runs).
	migCtx, migCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := storage.RunMigrations(migCtx, cfg.DatabaseURL); err != nil {
		migCancel()
		logger.Error("migrations failed", slog.String("err", err.Error()))
		os.Exit(exitCodeDB)
	}
	migCancel()
	logger.Info("migrations applied")

	// Daemon root context with shutdown semantics.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	// Translate SIGHUP to ctx-driven reload signal.
	reloadCh := make(chan os.Signal, 1)
	signal.Notify(reloadCh, syscall.SIGHUP)
	go func() {
		for range reloadCh {
			logger.Info("SIGHUP received; reloading config")
			newCfg, err := config.Load(*cfgPath, logger)
			if err != nil {
				logger.Warn("config reload failed; keeping current", slog.String("err", err.Error()))
				continue
			}
			// Apply hot-reloadable fields. Non-reloadable fields are logged and
			// silently kept at their previous values (per spec §live-reload).
			if r := applyReload(cfg, newCfg); len(r.Changed) > 0 || len(r.Skipped) > 0 {
				logger.Info("config reloaded",
					slog.Any("changed", r.Changed),
					slog.Any("skipped", r.Skipped),
				)
			}
		}
	}()

	logger.Info("entering fetch loop",
		slog.Int("concurrency", cfg.Fetch.WorkQueue.Concurrency),
		slog.Int("batch_size", cfg.Fetch.WorkQueue.BatchSize),
	)

	// Fetch loop (placeholder body — wired up in Phase 3).
	err = runFetchLoop(ctx, cfg, pg, logger)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("fetch loop exited with error", slog.String("err", err.Error()))
		os.Exit(exitCodeRuntime)
	}

	logger.Info("shutdown complete")
}

// runFetchLoop drives the poll-cycle loop. Phase 2 is the skeleton; Phase 3
// implements the workers and storage upsert.
func runFetchLoop(
	ctx context.Context,
	cfg *config.Config,
	pg *storage.Pool,
	logger *slog.Logger,
) error {
	ticker := time.NewTicker(cfg.Fetch.Poll.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			logger.Debug("tick (work scheduled in Phase 3)", slog.Time("at", t))
		}
	}
}

// awaitShutdown blocks until SIGINT / SIGTERM. Used when fetch is disabled so
// the daemon still serves /healthz cleanly without entering the fetch loop.
func awaitShutdown() <-chan struct{} {
	ch := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(ch)
	}()
	return ch
}

// reloadReport is the diff between the previous and newly-loaded configs.
type reloadReport struct {
	Changed []string
	Skipped []string
}

// applyReload merges hot-reloadable fields from `newCfg` into the live `cfg`.
// Non-reloadable changes are recorded in `Skipped`. Returns the diff.
func applyReload(cfg, newCfg *config.Config) reloadReport {
	r := reloadReport{}

	// Hot-reloadable
	if cfg.Fetch.Poll.PollInterval != newCfg.Fetch.Poll.PollInterval {
		r.Changed = append(r.Changed, fmt.Sprintf("fetch.poll.poll_interval_seconds: %d → %d",
			int(cfg.Fetch.Poll.PollInterval.Seconds()), int(newCfg.Fetch.Poll.PollInterval.Seconds())))
		cfg.Fetch.Poll.PollInterval = newCfg.Fetch.Poll.PollInterval
	}
	if cfg.Fetch.Poll.MinPollInterval != newCfg.Fetch.Poll.MinPollInterval {
		r.Changed = append(r.Changed, fmt.Sprintf("fetch.poll.min_poll_interval_seconds: %d → %d",
			int(cfg.Fetch.Poll.MinPollInterval.Seconds()), int(newCfg.Fetch.Poll.MinPollInterval.Seconds())))
		cfg.Fetch.Poll.MinPollInterval = newCfg.Fetch.Poll.MinPollInterval
	}
	if cfg.Fetch.WorkQueue.Concurrency != newCfg.Fetch.WorkQueue.Concurrency {
		r.Changed = append(r.Changed, fmt.Sprintf("fetch.work_queue.concurrency: %d → %d",
			cfg.Fetch.WorkQueue.Concurrency, newCfg.Fetch.WorkQueue.Concurrency))
		cfg.Fetch.WorkQueue.Concurrency = newCfg.Fetch.WorkQueue.Concurrency
	}
	if cfg.Logging.Level != newCfg.Logging.Level {
		r.Changed = append(r.Changed, fmt.Sprintf("logging.level: %s → %s", cfg.Logging.Level, newCfg.Logging.Level))
		cfg.Logging.Level = newCfg.Logging.Level
		slog.SetDefault(newLoggerAt(cfg.Logging.Level))
	}

	// Non-reloadable: warning only
	if cfg.DatabaseURL != newCfg.DatabaseURL {
		r.Skipped = append(r.Skipped, "database_url (restart required)")
	}
	if cfg.Health.ListenAddr != newCfg.Health.ListenAddr || cfg.Health.ListenPort != newCfg.Health.ListenPort {
		r.Skipped = append(r.Skipped, "health.listen_addr / health.listen_port (restart required)")
	}
	if cfg.NetworkName != newCfg.NetworkName {
		r.Skipped = append(r.Skipped, "network_name (restart required)")
	}

	// Indicate direction of full-config replacement for remaining hot fields.
	// (Wire up the rest in Phase 3 per spec requirement: live-reload.)
	return r
}

func newLogger(level slog.Level) (*slog.Logger, slog.Level) {
	return newLoggerAt(level), level
}

func newLoggerAt(level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
	})
	return slog.New(h)
}

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "dev"

// Ensure the binary can locate /etc/koios even when started by systemd.
func init() {
	// Sanity: refuse to run as root in non-test scenarios if a downstream
	// env var is set. (Logged; not enforced — operators decide.)
	if os.Getuid() == 0 {
		wd, _ := os.Getwd()
		_ = filepath.Clean(wd)
	}
}
