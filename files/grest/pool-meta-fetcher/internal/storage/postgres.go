// Package storage handles all PostgreSQL access for koios-pool-meta-fetcher:
// connection pooling (pgxpool), per-cycle work-queue discovery, and batched
// UPSERT into grest.pool_offchain_metadata.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// File-system helpers used by migrationsSource. Defined as vars so tests can
// stub them if ever needed; default to the real os calls.
var (
	osStatFunc  = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	osGetenvFunc = func(k string) string              { return os.Getenv(k) }
)

// Pool wraps pgxpool.Pool with daemon-specific helpers.
type Pool struct {
	*pgxpool.Pool
	appName string
}

// PoolOptions configures the pgxpool.
type PoolOptions struct {
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	StatementTimeout time.Duration
	ApplicationName  string
}

// NewPool configures and opens the pgxpool. It validates connectivity via a
// single Ping before returning.
func NewPool(ctx context.Context, dbURL string, opts PoolOptions) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	if opts.ApplicationName != "" {
		cfg.ConnConfig.Config.RuntimeParams["application_name"] = opts.ApplicationName
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if _, _, err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{Pool: pool, appName: opts.ApplicationName}, nil
}

// Ping returns (last_ping_at, age, err) for the readyz health check.
func (p *Pool) Ping(ctx context.Context) (time.Time, time.Duration, error) {
	t0 := time.Now()
	err := p.Pool.Ping(ctx)
	return t0, time.Since(t0), err
}

// -----------------------------------------------------------------------------
// Work queue (pool discovery, per spec pool-offchain-fetcher §pool-discovery)
// -----------------------------------------------------------------------------

// WorkItem is one row from the work-queue query.
type WorkItem struct {
	PoolID         int64
	PoolUpdateID   int64
	URL            string
	OnChainHash    []byte
	LastPolled     *time.Time
	LastPoolUpdate *int64
}

// DiscoverWorkItems returns the pools needing a fetch in this cycle.
//
// The query joins public.pool_update -> public.pool_hash -> public.pool_metadata_ref,
// picks the most-recent pool_update per pool, and LEFT-JOINs the cache table
// with two "needs work" predicates: row missing, OR last_pool_update_id differs,
// OR last_polled is older than `pollStaleAfter`.
func (p *Pool) DiscoverWorkItems(
	ctx context.Context,
	batchSize int,
	pollStaleAfter time.Duration,
	allowList, denyList []string,
) ([]WorkItem, error) {
	// For Phase 2 we ignore allowList/denyList — wired in Phase 3.
	// (The fetcher package will apply them in-memory.)
	const q = `
WITH latest_pu AS (
  SELECT DISTINCT ON (pu.hash_id)
    pu.id           AS pu_id,
    pu.hash_id      AS pool_hash_id,
    pu.meta_id      AS meta_id
  FROM public.pool_update pu
  WHERE pu.active_epoch_no <= (SELECT MAX(epoch_no) FROM public.epoch_param)
  ORDER BY pu.hash_id, pu.registered_tx_id DESC, pu.cert_index DESC
)
SELECT
  ph.id,
  lp.pu_id,
  pmr.url,
  pmr.hash,
  pom.last_polled,
  pom.last_pool_update_id
FROM latest_pu lp
JOIN public.pool_hash          ph  ON ph.id  = lp.pool_hash_id
JOIN public.pool_metadata_ref  pmr ON pmr.id = lp.meta_id
LEFT JOIN grest.pool_offchain_metadata pom ON pom.pool_id = ph.id
WHERE pmr.url IS NOT NULL
  AND (
       pom.pool_id IS NULL
    OR pom.last_pool_update_id IS DISTINCT FROM lp.pu_id
    OR pom.last_polled < now() - ($1::bigint * INTERVAL '1 second')
  )
LIMIT $2;
`
	var staleSecs int64 = int64(pollStaleAfter.Seconds())
	if staleSecs <= 0 {
		staleSecs = 1
	}
	rows, err := p.Query(ctx, q, staleSecs, batchSize)
	if err != nil {
		return nil, fmt.Errorf("discover work: %w", err)
	}
	defer rows.Close()

	var items []WorkItem
	for rows.Next() {
		var wi WorkItem
		if err := rows.Scan(
			&wi.PoolID, &wi.PoolUpdateID, &wi.URL, &wi.OnChainHash,
			&wi.LastPolled, &wi.LastPoolUpdate,
		); err != nil {
			return nil, fmt.Errorf("scan work item: %w", err)
		}
		items = append(items, wi)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate work items: %w", err)
	}
	// Phase 3 will apply allowList/denyList here.
	_ = allowList
	_ = denyList
	return items, nil
}

// -----------------------------------------------------------------------------
// Batched UPSERT
// -----------------------------------------------------------------------------

// PoolMetadata is the upsert payload for one row in grest.pool_offchain_metadata.
type PoolMetadata struct {
	PoolID         int64
	MetaURL        string
	MetaHash       []byte     // 32 bytes (Blake2b-256 or zeros if HashNone)
	MetaJSON       []byte     // raw json.Marshal output
	MetaBytes      []byte     // raw response body
	LastPolled     time.Time
	LastPoolUpdate int64
	IsValid        bool
}

// UpsertBatch performs an UPSERT for a batch of PoolMetadata rows in one
// transaction. pgx's CopyFrom is used for the write path because it is the
// fastest path through the binary protocol.
func (p *Pool) UpsertBatch(ctx context.Context, batch []PoolMetadata) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ON CONFLICT (pool_id) DO UPDATE — cache is current-state, one row per pool.
	const copyQ = `
COPY grest.pool_offchain_metadata
  (pool_id, meta_url, meta_hash, meta_json, meta_bytes, last_polled, last_pool_update_id, is_valid)
FROM STDIN
WITH (FORMAT text, DELIMITER E'\t', NULL '');
`
	rows := make([][]any, 0, len(batch))
	for _, m := range batch {
		rows = append(rows, []any{
			m.PoolID,
			m.MetaURL,
			m.MetaHash,
			m.MetaJSON,
			m.MetaBytes,
			m.LastPolled,
			m.LastPoolUpdate,
			m.IsValid,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"grest", "pool_offchain_metadata"},
		[]string{"pool_id", "meta_url", "meta_hash", "meta_json", "meta_bytes",
			"last_polled", "last_pool_update_id", "is_valid"},
		pgx.CopyFromRows(rows),
	); err != nil {
		// Fallback path: COPY is fastest but requires table ownership privileges
		// in the connected role. If COPY fails with permission error, retry as
		// INSERT ... ON CONFLICT via INSERT statement. Phase 3 will add the
		// INSERT fallback if needed.
		return fmt.Errorf("copy upsert (pool_meta_fetcher must own the table; did you forget to ALTER TABLE ... OWNER TO grest_owner?): %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit upsert tx: %w", err)
	}
	return nil
}

// AdjustMetaBytesCheckConstraint (de)tightens the DB-side CHECK on meta_bytes
// to match the operator-configured max_body_bytes. Called at startup.
//
// See openspec/specs/pool-offchain-metadata-cache/spec.md §storage-budget.
func (p *Pool) AdjustMetaBytesCheckConstraint(ctx context.Context, maxBytes int) error {
	if maxBytes <= 0 || maxBytes > 5242880 {
		return fmt.Errorf("maxBytes out of range (1..5242880): %d", maxBytes)
	}
	// Drop existing constraint (if any) and recreate with the right limit.
	_, err := p.Exec(ctx, `
DO $$
DECLARE c text;
BEGIN
  SELECT conname INTO c
  FROM pg_constraint
  WHERE conrelid = 'grest.pool_offchain_metadata'::regclass
    AND contype = 'c'
    AND pg_get_constraintdef(oid) LIKE '%meta_bytes%';
  IF c IS NOT NULL THEN
    EXECUTE format('ALTER TABLE grest.pool_offchain_metadata DROP CONSTRAINT %I', c);
  END IF;
  EXECUTE format(
    'ALTER TABLE grest.pool_offchain_metadata
       ADD CONSTRAINT pool_offchain_metadata_meta_bytes_check
       CHECK (OCTET_LENGTH(meta_bytes) BETWEEN 16 AND %s)',
    $1
  );
END $$;
`, maxBytes)
	return err
}

// -----------------------------------------------------------------------------
// Migrations
// -----------------------------------------------------------------------------

// RunMigrations applies any pending schema migrations embedded with the binary.
//
// The default source is a ./migrations directory next to the binary; for
// development we fall back to the repo-relative path under files/grest/rpc.
func RunMigrations(ctx context.Context, dbURL string) error {
	if dbURL == "" {
		return errors.New("dbURL is empty")
	}
	src := migrationsSource()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("parse db url: %w", err)
	}
	connStr := stdlib.RegisterConnConfig(cfg.ConnConfig)
	driver, err := migratepgx.WithInstance(stdlib.OpenDB(connStr), &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("create migrate driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("new migrator: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// migrationsSource picks the migration directory to use. Phase 2 ships with
// the SQL files co-located under files/grest/rpc relative to the repo root;
// operators can override by setting KOIOS_MIGRATIONS_DIR.
func migrationsSource() string {
	if d := osGetenvFunc("KOIOS_MIGRATIONS_DIR"); d != "" {
		return "file://" + d
	}
	if existsDir("migrations") {
		return "file://migrations"
	}
	// Default to the repo-relative source so the daemon can run from the
	// files/grest/pool-meta-fetcher directory during development.
	return "file://../rpc"
}

func existsDir(p string) bool {
	if p == "" {
		return false
	}
	fi, err := osStatFunc(p)
	return err == nil && fi.IsDir()
}
