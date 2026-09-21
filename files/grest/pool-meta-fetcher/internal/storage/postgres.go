// Package storage handles all PostgreSQL access for koios-pool-meta-fetcher:
// connection pooling (pgxpool), per-cycle work-queue discovery, and batched
// UPSERT into grest.pool_offchain_metadata.
package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	migrateiofs "github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

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
	schema  string // namespace for grest.* tables; defaults to "grest"
}

// PoolOptions configures the pgxpool.
type PoolOptions struct {
	Schema           string
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
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{Pool: pool, appName: opts.ApplicationName, schema: opts.Schema}, nil
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
	schemaIdent := pgx.Identifier{p.Schema()}.Sanitize()
	q := fmt.Sprintf(`
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
LEFT JOIN %s.pool_offchain_metadata pom ON pom.pool_id = ph.id
WHERE pmr.url IS NOT NULL
  AND (
       pom.pool_id IS NULL
    OR pom.last_pool_update_id IS DISTINCT FROM lp.pu_id
    OR pom.last_polled < now() - ($1::bigint * INTERVAL '1 second')
  )
LIMIT $2;
`, schemaIdent)
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

// NewPoolWithSchema is like NewPool but takes an explicit schema name. The
// schema must already exist in the database (created by migrations or by hand);
// the daemon does not CREATE SCHEMA on startup.
func NewPoolWithSchema(ctx context.Context, dbURL, schema string, opts PoolOptions) (*Pool, error) {
	if schema != "" {
		opts.Schema = schema
	}
	return NewPool(ctx, dbURL, opts)
}

// Schema returns the configured schema name (defaults to "grest").
func (p *Pool) Schema() string {
	if p.schema == "" {
		return "grest"
	}
	return p.schema
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
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{p.Schema(), "pool_offchain_metadata"},
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
	schemaIdent := pgx.Identifier{p.Schema()}.Sanitize()
	// Two-statement approach (no DO block — avoids pgx parameter binding edge
	// cases with dollar-quoted bodies). DROP CONSTRAINT IF EXISTS is a no-op
	// the first time around.
	dropSQL := fmt.Sprintf(
		`ALTER TABLE %s.pool_offchain_metadata DROP CONSTRAINT IF EXISTS pool_offchain_metadata_meta_bytes_check;`,
		schemaIdent,
	)
	if _, err := p.Exec(ctx, dropSQL); err != nil {
		return fmt.Errorf("drop check constraint: %w", err)
	}
	// We bake maxBytes into the SQL string directly (validated above) to
	// avoid the pgx "mismatched param and argument count" error that triggers
	// when the parameterised form is sent via the simple protocol with no
	// explicit type cast. The query becomes a one-shot ALTER.
	addSQL := fmt.Sprintf(
		`ALTER TABLE %s.pool_offchain_metadata ADD CONSTRAINT pool_offchain_metadata_meta_bytes_check CHECK (OCTET_LENGTH(meta_bytes) BETWEEN 16 AND %d);`,
		schemaIdent, maxBytes,
	)
	if _, err := p.Exec(ctx, addSQL); err != nil {
		return fmt.Errorf("add check constraint: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Migrations
// -----------------------------------------------------------------------------

// RunMigrations applies any pending schema migrations. The daemon ships with
// the SQL files embedded under migrations/*.sql; the literal token "{schema}"
// in those files is replaced with the configured schema name before execution.
//
// For dev overrides, set KOIOS_MIGRATIONS_DIR to a directory of plain SQL
// files; the same {schema} substitution applies.
func RunMigrations(ctx context.Context, dbURL, schema string) error {
	if dbURL == "" {
		return errors.New("dbURL is empty")
	}
	if schema == "" {
		schema = "grest"
	}
	if err := validateSchemaIdent(schema); err != nil {
		return err
	}

	// Bootstrap: ensure the schema exists BEFORE constructing the migrator.
	// Without this, golang-migrate's CREATE TABLE schema_migrations fails
	// because the target schema does not yet exist (the schema is normally
	// created by migration 0001). The CREATE is idempotent.
	if err := ensureSchema(dbURL, schema); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	srcFS, dir, err := buildMigrationsSource(schema)
	if err != nil {
		return fmt.Errorf("build migration source: %w", err)
	}
	if dir != "" {
		defer os.RemoveAll(dir)
	}

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("parse db url: %w", err)
	}
	driver, err := migratepgx.WithInstance(stdlib.OpenDB(*cfg.ConnConfig), &migratepgx.Config{
		// Pin the schema_migrations bookkeeping table into the same schema as
		// the daemon's table (no default-search_path / no creation in public).
		MigrationsTable: "schema_migrations",
		SchemaName:      schema,
	})
	if err != nil {
		return fmt.Errorf("create migrate driver: %w", err)
	}
	migSrc, err := migrateiofs.New(srcFS, "migrations")
	if err != nil {
		return fmt.Errorf("new iofs source: %w", err)
	}
	defer migSrc.Close()
	m, err := migrate.NewWithInstance("iofs", migSrc, "postgres", driver)
	if err != nil {
		return fmt.Errorf("new migrator: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// ensureSchema creates the configured schema if it doesn't exist. Safe to
// call repeatedly (uses IF NOT EXISTS).
func ensureSchema(dbURL, schema string) error {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	sql := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s;`,
		pgx.Identifier{schema}.Sanitize())
	_, err = pool.Exec(context.Background(), sql)
	return err
}

// buildMigrationsSource resolves the migration source. Returns (fs.FS, tmpdir).
// When tmpdir is non-empty the caller is responsible for removing it.
func buildMigrationsSource(schema string) (fs.FS, string, error) {
	if dir := osGetenvFunc("KOIOS_MIGRATIONS_DIR"); dir != "" {
		return renderMigrationsToTempDir(os.DirFS(dir), schema, dir)
	}
	return renderMigrationsToTempDir(migrationsFS, schema, "")
}

// renderMigrationsToTempDir walks the supplied FS, replaces {schema} in every
// .sql file, and writes the result to a tempdir (or to a baseDir override).
// Returns an OS-DirFS that the migrate.iofs source can drive directly with
// the "migrations" prefix.
func renderMigrationsToTempDir(src fs.FS, schema string, baseDir string) (fs.FS, string, error) {
	var out string
	if baseDir != "" {
		out = baseDir
		if err := os.MkdirAll(out, 0o755); err != nil {
			return nil, "", err
		}
	} else {
		var err error
		out, err = os.MkdirTemp("", "pool-meta-fetcher-mig-*")
		if err != nil {
			return nil, "", err
		}
	}
	migSubdir := out + "/migrations"
	if err := os.MkdirAll(migSubdir, 0o755); err != nil {
		return nil, "", err
	}

	// Read entries; try both layouts (migrations/ subdir and flat).
	listed, err := fs.ReadDir(src, "migrations")
	if err != nil {
		listed, err = fs.ReadDir(src, ".")
		if err != nil {
			return nil, "", err
		}
	}
	var names []string
	for _, e := range listed {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, "", errors.New("no .sql migrations found")
	}
	sort.Strings(names)
	for i, name := range names {
		var body []byte
		body, err = fs.ReadFile(src, "migrations/"+name)
		if err != nil {
			body, err = fs.ReadFile(src, name)
			if err != nil {
				return nil, "", fmt.Errorf("read %s: %w", name, err)
			}
		}
		rendered := substituteSchema(string(body), schema)
		// Strip any pre-existing numeric prefix from the source name (so we
		// can renumber deterministically); preserve the remainder.
		// golang-migrate's filename regex requires ".up." or ".down." before
		// the extension — ensure our output keeps ".up.sql" suffix.
		stripped := name
		if k := strings.IndexByte(stripped, '_'); k > 0 && k <= 6 {
			stripped = stripped[k+1:]
		}
		// Keep ".up" in the migration name so the regex matches.
		// Allow source filenames to be either "0001_name.up.sql" or
		// "0001_a.up.sql" (our sort-stable a/b/c suffix style).
		if !strings.Contains(stripped, ".up.") && !strings.HasSuffix(stripped, ".up.sql") {
			stripped = strings.TrimSuffix(stripped, ".sql") + ".up.sql"
		}
		newName := fmt.Sprintf("%05d_%s", i+1, stripped)
		if err := os.WriteFile(migSubdir+"/"+newName, []byte(rendered), 0o644); err != nil {
			return nil, "", fmt.Errorf("write %s: %w", newName, err)
		}
	}
	return os.DirFS(out), out, nil
}

// schemaIdentRE is exported for testing.
var schemaIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateSchemaIdent(s string) error {
	if !schemaIdentRE.MatchString(s) {
		return fmt.Errorf("invalid schema ident %q (must match %s)", s, schemaIdentRE)
	}
	return nil
}

// substituteSchema replaces literal "{schema}" and "grest." (only as schema
// prefix, not as part of any token) with the configured schema name. Conservative
// substitution: looks for the token `grest.` at identifier boundaries.
func substituteSchema(src, schema string) string {
	src = strings.ReplaceAll(src, "{schema}", schema)
	// Replace leading "grest." schema prefix in qualified names.
	// We match word boundaries so we don't touch identifiers that happen to
	// contain "grest" as a substring (none in Koios codebase, but be safe).
	return reLeadingGrest.ReplaceAllString(src, schema+".")
}

var reLeadingGrest = regexp.MustCompile(`(^|[^A-Za-z0-9_])grest\.`)

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
