-- 0002_pool_offchain_metadata_table.sql
-- Creates the cache table grest.pool_offchain_metadata in the configured
-- schema (replaced from {schema} at runtime).
--
-- All schema-qualified references in the table DDL pass through the regex
-- substitution "grest." -> "<schema>." so the resulting SQL targets whatever
-- schema the operator has chosen (defaults to "grest"; for tests "gresttmp").

CREATE TABLE IF NOT EXISTS {schema}.pool_offchain_metadata (
  id                    BIGSERIAL    PRIMARY KEY,
  pool_id               BIGINT       NOT NULL UNIQUE REFERENCES public.pool_hash(id) ON DELETE CASCADE,
  meta_url              VARCHAR(256) NOT NULL,
  meta_hash             BYTEA        NOT NULL CHECK (OCTET_LENGTH(meta_hash) = 32),
  meta_json             JSONB        NOT NULL,
  meta_bytes            BYTEA        NOT NULL CHECK (OCTET_LENGTH(meta_bytes) BETWEEN 16 AND 5242880),
  last_polled           TIMESTAMPTZ  NOT NULL,
  last_pool_update_id   BIGINT       NOT NULL REFERENCES public.pool_update(id) ON DELETE CASCADE,
  is_valid              BOOLEAN      NOT NULL DEFAULT TRUE
);

COMMENT ON TABLE  {schema}.pool_offchain_metadata                          IS 'Current raw off-chain metadata per pool, populated by koios-pool-meta-fetcher';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.pool_id                  IS 'FK -> public.pool_hash(id); UNIQUE enforces one row per pool';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.meta_url                 IS 'Last URL the daemon actually fetched';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.meta_hash                IS 'Blake2b-256 of meta_bytes (informational; no RPC verifies against pmr.hash)';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.meta_json                IS 'Parsed payload round-tripped through json.Marshal';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.meta_bytes               IS 'Raw response body, capped via DB CHECK + daemon config';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.last_polled              IS 'Last successful or attempted fetch time';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.last_pool_update_id      IS 'FK -> public.pool_update(id); provenance pointer to the chain cert';
COMMENT ON COLUMN {schema}.pool_offchain_metadata.is_valid                 IS 'TRUE if the row passed size/content-type/JSON validators (hash agreement NOT considered)';

-- Indexes (operational queries)

CREATE INDEX IF NOT EXISTS idx_pool_offchain_metadata_last_polled
  ON {schema}.pool_offchain_metadata (last_polled DESC);

CREATE INDEX IF NOT EXISTS idx_pool_offchain_metadata_last_pool_update_id
  ON {schema}.pool_offchain_metadata (last_pool_update_id);

CREATE INDEX IF NOT EXISTS idx_pool_offchain_metadata_is_valid_false
  ON {schema}.pool_offchain_metadata (is_valid)
  WHERE is_valid = FALSE;

-- Ownership + grants

ALTER TABLE {schema}.pool_offchain_metadata OWNER TO grest_owner;

GRANT SELECT ON {schema}.pool_offchain_metadata TO web_anon;

GRANT INSERT, UPDATE, DELETE ON {schema}.pool_offchain_metadata TO grest_owner;
GRANT USAGE   ON SEQUENCE {schema}.pool_offchain_metadata_id_seq TO grest_owner;
