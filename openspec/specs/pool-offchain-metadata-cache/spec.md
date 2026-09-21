# Capability: pool-offchain-metadata-cache

## Purpose

Persist the *current* off-chain pool metadata for every Cardano stake pool in a single
grest-owned table. One row per pool, upserted in place on every successful fetch by
`koios-pool-meta-fetcher`. The on-chain declared hash is **not** enforced; the fetched
hash is recorded (informational only). No live-verification column is exposed in the
RPC API; consumers JOIN `pom.meta_hash = pmr.hash` themselves if they want.

This capability is the storage backend; populating it is the responsibility of
`pool-offchain-fetcher`. Reading it is the responsibility of `pool-rpc-repointing`.

## Requirements

### Requirement: schema

The system SHALL provide a table `grest.pool_offchain_metadata` with the following columns:

| Column | SQL type | Constraints |
|---|---|---|
| `id` | `BIGSERIAL` | `PRIMARY KEY` |
| `pool_id` | `BIGINT` | `NOT NULL`, `UNIQUE`, `REFERENCES public.pool_hash(id) ON DELETE CASCADE` |
| `meta_url` | `VARCHAR(256)` | `NOT NULL` |
| `meta_hash` | `BYTEA` | `NOT NULL`, CHECK `OCTET_LENGTH(meta_hash) = 32` |
| `meta_json` | `JSONB` | `NOT NULL` |
| `meta_bytes` | `BYTEA` | `NOT NULL`, CHECK `OCTET_LENGTH(meta_bytes) BETWEEN 16 AND 524288` |
| `last_polled` | `TIMESTAMPTZ` | `NOT NULL` |
| `last_pool_update_id` | `BIGINT` | `NOT NULL`, `REFERENCES public.pool_update(id) ON DELETE CASCADE` |
| `is_valid` | `BOOLEAN` | `NOT NULL DEFAULT TRUE` |

The `meta_hash` SHALL be the Blake2b-256 (32-byte) hash of `meta_bytes`, computed by the
fetcher at write time.

The `meta_bytes` size constraint SHALL be `ALTER`ed at daemon startup if
`fetch.validation.max_body_bytes` differs from the hardcoded 524288 (avoids coupling DB
migration to runtime config).

#### Scenario: row is upserted on successful fetch

- **WHEN** the fetcher downloads pool X's metadata, parses it, and writes successfully
- **THEN** a row exists in `grest.pool_offchain_metadata` with
  - `pool_id = X.id`
  - `meta_bytes = raw_response_body`
  - `meta_hash = blake2b_256(raw_response_body)`
  - `meta_json = json_marshal(parsed_payload)`
  - `is_valid = TRUE`
  - `last_polled = now()`

#### Scenario: existing row is overwritten when upstream pool_update changes

- **WHEN** pool X submits a new pool_update with a new `meta_id`, the fetcher downloads
  the new URL, and writes successfully
- **THEN** the existing row's `meta_url`, `meta_hash`, `meta_json`, `meta_bytes`,
  `last_polled`, `last_pool_update_id`, `is_valid` are updated in place. No second row is
  created (enforced by the UNIQUE constraint on `pool_id`).

### Requirement: indexes

The system SHALL create the following indexes:

- UNIQUE INDEX on `(pool_id)` — already enforced by the column constraint
- INDEX on `(last_polled DESC)` — supports the "pools not fetched in N minutes" ops query
- INDEX on `(is_valid)` WHERE `is_valid = FALSE` — partial index for failed pools only
- INDEX on `(last_pool_update_id)` — supports JOINs back to chain provenance

### Requirement: access-control

The table SHALL be:
- owned by the new role `grest_owner` (created in `files/grest/rpc/db-scripts/basics.sql`),
- `GRANT SELECT ON` to `web_anon` (PostgREST consumers),
- `GRANT ALL ON` to `grest_owner` (daemon).
- `GRANT USAGE ON SEQUENCE grest.pool_offchain_metadata_id_seq TO grest_owner`.

#### Scenario: web_anon can read but not write

- **WHEN** a PostgREST request `SELECT meta_json FROM grest.pool_offchain_metadata WHERE
  pool_id = 42` is executed
- **THEN** it returns the row
- AND **WHEN** the same role issues `INSERT INTO grest.pool_offchain_metadata (...)`
- **THEN** the database raises `permission denied`

### Requirement: storage-budget

The `meta_bytes` column SHALL NOT exceed `fetch.validation.max_body_bytes` (default
524288 = 512 KiB) per row, enforced by a CHECK constraint. The daemon SHALL `ALTER TABLE`
the constraint on startup if the configured value differs from the current DB value.

### Requirement: provenance-pointer

`last_pool_update_id` SHALL always correspond to the most recent `public.pool_update` row
that declared the URL / hash the fetcher is currently storing for this pool. Consumers can
JOIN through this column back to `public.pool_update` → `public.pool_metadata_ref` to get
the on-chain declared `url` and `hash`.

#### Scenario: provenance join

- **WHEN** a consumer wants to verify whether the current fetched hash equals the
  on-chain declared hash
- **THEN** the consumer JOINs `grest.pool_offchain_metadata pomet ON pomet.pool_id = $1`
  with `public.pool_update pu ON pu.id = pomet.last_pool_update_id` and
  `public.pool_metadata_ref pmr ON pmr.id = pu.meta_id`
- **AND** the comparison `pomet.meta_hash = pmr.hash` yields TRUE iff the SPO's currently
  declared hash matches the actual fetched hash
