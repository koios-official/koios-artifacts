# Capability: pool-offchain-fetcher

## Purpose

A long-running Go daemon (`koios-pool-meta-fetcher`) that periodically fetches the
off-chain metadata JSON declared by each Cardano stake pool's most recent registration
certificate, validates it against configurable rules (size, content-type, JSON shape,
hash), and upserts the result into `grest.pool_offchain_metadata`. Written in Go to
remove the operational friction of `cardano-smash-server` (Haskell, hardcoded limits,
SMASH policy baggage, no per-pool tuning).

## Requirements

### Requirement: process-lifecycle

The daemon SHALL:
- read its config from `--config PATH` (default `/etc/koios/pool-meta-fetcher.yaml`),
- open a database connection pool on startup,
- validate the config (see `pool-meta-fetcher-config` capability),
- start the health HTTP server,
- enter the poll loop on a single `time.Ticker`,
- respond to SIGINT / SIGTERM with a graceful shutdown (see Requirement: graceful-shutdown),
- handle SIGHUP by re-reading the config from disk and applying tunables (see
  Requirement: live-reload).

#### Scenario: clean startup
- **WHEN** the daemon is launched with a valid config and a reachable database
- **THEN** it logs `event=ready`, exits no later than 5 seconds, and `/readyz` returns
  200 OK with body `{"status":"ready"}`

#### Scenario: invalid config
- **WHEN** the daemon is launched with `--config /nonexistent.yaml`
- **THEN** it logs `level=error event=config_load_failed path=/nonexistent.yaml reason="no
  such file"` and exits with code 2 within 1 second

### Requirement: poll-cycle

The daemon SHALL loop on a fixed cadence (`fetch.poll_interval_seconds`, default 300). On
each tick it SHALL:
1. Run the work-queue query (see Requirement: pool-discovery) to enumerate pools needing
   work, applying the per-pool `min_poll_interval_seconds` floor.
2. Push the work items into a bounded channel of size
   `fetch.work_queue.batch_size * fetch.work_queue.concurrency`.
3. Spawn `fetch.work_queue.concurrency` workers that pull work, perform HTTP GET, apply
   validators, and produce a result (success or failure).
4. Aggregate results and perform batched UPSERT into
   `grest.pool_offchain_metadata` in a single transaction at the end of the cycle.
5. Emit a structured log line summarizing: cycle_start, queue_size, success_count,
   hash_mismatch_count, size_violation_count, content_type_violation_count,
   json_validation_failure_count, retry_excluded_count, cycle_duration_ms,
   last_db_write_at. (`hash_mismatch_count` is purely informational and does not gate
   any upsert; see Requirement: hash-capture-informational.)

#### Scenario: empty queue
- **WHEN** the work-queue query returns zero rows
- **THEN** the daemon logs `event=cycle_skip reason=empty_queue`, sleeps for
  `poll_interval_seconds`, and does NOT perform any HTTP work

#### Scenario: high queue
- **WHEN** the work-queue query returns 500 rows and `concurrency = 16`
- **THEN** the daemon processes them in 32 batches of 16 each, never exceeding 16
  in-flight requests

### Requirement: pool-discovery

On each tick, the daemon SHALL execute:

```sql
SELECT
  ph.id,
  pmr.url,
  pmr.hash,
  pu.id AS pu_id
FROM public.pool_update pu
JOIN public.pool_hash   ph  ON ph.id  = pu.hash_id
JOIN public.pool_metadata_ref pmr ON pmr.id = pu.meta_id
WHERE pu.id = (SELECT MAX(id) FROM public.pool_update WHERE hash_id = pu.hash_id)
  AND pu.active_epoch_no <= (SELECT MAX(epoch_no) FROM public.epoch_param)
  AND pmr.url IS NOT NULL
  AND (
    NOT EXISTS (SELECT 1 FROM grest.pool_offchain_metadata WHERE pool_id = ph.id)
    OR grest.pool_offchain_metadata.last_pool_update_id IS DISTINCT FROM pu.id
    OR grest.pool_offchain_metadata.last_polled < now() - ($1::int * INTERVAL '1 second')
  );
```

The fetched `pmr.hash` is the on-chain declared hash. It is used purely for informational
hash-mismatch logging (see Requirement: hash-capture-informational); the daemon does
NOT enforce a match.

#### Scenario: brand-new pool
- **WHEN** a pool's most recent pool_update is at the canonical `pu.id` and no row exists
  in `grest.pool_offchain_metadata` for it
- **THEN** it appears in the work queue

#### Scenario: recently fetched pool, no chain change
- **WHEN** a pool's `last_polled` is less than `poll_interval_seconds` ago and
  `last_pool_update_id` matches the latest pool_update id
- **THEN** it is NOT in the work queue

#### Scenario: SPO changed their URL
- **WHEN** a pool submits a new pool_update with a new `meta_id`
- **THEN** `last_pool_update_id` differs from `pu.id`, so the pool appears in the work queue

### Requirement: hash-capture-informational

For each pool in the work queue, the daemon SHALL:
1. Download the URL declared in `pool_metadata_ref.url` via HTTPS GET.
2. Compute `blake2b_256(raw_response_body)` using `golang.org/x/crypto/blake2b` Size=256
   (configurable via `fetch.validation.hash.algorithm; "none"` disables).
3. Apply all other validation rules (size, content-type, JSON shape) — see
   `pool-meta-fetcher-config`.
4. Store the computed `meta_hash` in the corresponding row column, regardless of whether
   it equals the on-chain declared hash.
5. If `fetch.validation.hash.log_mismatches: true`, log mismatches as an informational
   event (`event=hash_mismatch pool_id=X declared=Y actual=Z level=info`) ONCE per
   cycle per pool (deduplicated).
6. There is NO live-verification column in the public RPC API. Consumers wanting to
   compare fetched vs declared hash do so themselves by JOINing
   `pom.meta_hash = pmr.hash` through `last_pool_update_id`.

#### Scenario: hash mismatch is informational, not a gate
- **WHEN** the downloaded bytes' Blake2b-256 hash differs from `pool_metadata_ref.hash`
  AND all other validators pass
- **THEN** the row IS upserted with `meta_hash = actual_hash` and `is_valid = TRUE`.
  An info-level hash_mismatch event is emitted (deduplicated per cycle).

#### Scenario: body too large
- **WHEN** response body exceeds `fetch.validation.max_body_bytes`
- **THEN** the daemon does NOT upsert; logs `level=warn event=size_exceeded pool_id=X
  size=Y limit=Z`; increments per-pool retry counter

#### Scenario: malformed JSON
- **WHEN** `json.Unmarshal` fails
- **THEN** the daemon does NOT upsert; logs `level=warn event=json_parse_failed pool_id=X
  err=Y`; increments per-pool retry counter

### Requirement: connection-security

The daemon SHALL:
- Block outgoing HTTPS connections to **any** of the following when `fetch.private_ip_policy
  = block`:
  - IPv4 RFC1918 (10/8, 172.16/12, 192.168/16)
  - IPv4 loopback (127/8) and broadcast (255.255.255.255)
  - IPv4 link-local (169.254/16) and unspecified (0.0.0.0)
  - IPv4 multicast (224/4) — also blocks SSRF to metadata services like
    169.254.169.254
  - IPv6 loopback (::1), unspecified (::), link-local (fe80::/10), unique-local
    (fc00::/7), multicast (ff00::/8)
- Implement blocking at connect time via a custom `http.Transport.DialContext` that:
  1. Resolves the URL host via `net.DefaultResolver`.
  2. Checks every resolved IP against the deny list.
  3. Aborts with `event=ip_blocked pool_id=X host=Y ip=Z` if any resolved IP is denied.
  4. Connects using the vetted address (DNS-rebind safe).
- Verify TLS certificates by default; `fetch.http.skip_tls_verify: true` allowed only for
  testing (daemon logs a `level=warn event=tls_verify_disabled` once at startup).
- Apply a per-request HTTP timeout (`fetch.http.timeout_seconds`, default 30) and a
  separate connect timeout (`fetch.http.connect_timeout_seconds`, default 10).

#### Scenario: private IP rejected
- **WHEN** the URL host resolves to `127.0.0.1` and `private_ip_policy = block`
- **THEN** the daemon aborts the request before any TCP connect; logs
  `event=ip_blocked`

#### Scenario: DNS rebind attempt
- **WHEN** a host resolves to `93.184.216.34` at the API check and `127.0.0.1` at connect
  time
- **THEN** the daemon catches the second resolution inside `DialContext`, detects
  `127.0.0.1` is private, and aborts

### Requirement: graceful-shutdown

The daemon SHALL handle SIGINT / SIGTERM by:
1. Stopping the poll loop ticker (no new work enqueued).
2. Sending a signal through the bounded work channel.
3. Workers drain their in-flight HTTP requests (with a `shutdown_grace_seconds` timeout,
   default 30) and emit final UPSERTs in one transaction.
4. Closing the database connection pool cleanly.
5. Stopping the health HTTP server.
6. Exiting with code 0 on graceful exit, code 1 on timeout.

#### Scenario: SIGTERM during in-flight work
- **WHEN** SIGTERM arrives with 4 in-flight HTTP requests
- **THEN** the daemon logs `event=shutdown_begin in_flight=4`, waits up to
  `shutdown_grace_seconds` for the requests to complete, performs the final batched
  UPSERT, and exits with code 0

#### Scenario: SIGTERM with hung requests
- **WHEN** SIGTERM arrives with 1 in-flight HTTP request and `shutdown_grace_seconds =
  30`, and that request hangs for 60s
- **THEN** the daemon logs `event=shutdown_timeout in_flight=1`, exits with code 1
  after 30s, and the final UPSERT may be lost for the in-flight pools (those rows will
  be re-discovered on next start)

### Requirement: health-endpoints

The daemon SHALL expose on `fetch.health.listen_addr:fetch.health.listen_port`:
- `GET /healthz` — always 200 OK with body `{"status":"ok"}` while the process is alive
- `GET /readyz` — 200 OK if the last DB ping succeeded within
  `fetch.health.readyz_db_grace_seconds`, else 503 with body
  `{"status":"degraded","reason":"db_unreachable","last_ping_age_seconds":N}`
- `GET /debug/pools` — only if `fetch.work_queue.expose_debug_endpoint: true`;
  returns JSON `{in_flight: [...], recent_failures: [...]}`. Otherwise this route is
  not registered (so 404 responses cannot leak existence).

#### Scenario: database unreachable
- **WHEN** the most recent DB ping is older than `readyz_db_grace_seconds`
- **THEN** `/readyz` returns 503

### Requirement: live-reload

The daemon SHALL handle SIGHUP by:
1. Re-reading the config file from disk.
2. Applying tunables that do not require a restart: `poll_interval_seconds`,
   `concurrency`, `min_poll_interval_seconds`, `log_level`, `user_agent`,
   `allowed_content_types`, `max_body_bytes`, `min_body_bytes`, all retry knobs,
   JSON validators.
3. Logging a diff of changed values: `event=config_reloaded changed=[...]`.
4. NOT applying tunables that require a restart: `database_url`, `listen_addr`,
   `listen_port`, network change. These changes are logged but ignored; the operator
   must restart the daemon.

#### Scenario: SIGHUP with valid updated config
- **WHEN** the daemon is running and the operator updates `poll_interval_seconds: 300 →
  600` in the config file and sends SIGHUP
- **THEN** the next tick occurs 600 seconds after the previous tick, and the daemon logs
  `event=config_reloaded changed=[poll_interval_seconds: 300 → 600]`

### Requirement: retry-policy

The daemon SHALL implement per-pool retry counters in memory (`sync.Map[pool_id]uint`)
with exponential backoff: `delay(n) = min(backoff_max, backoff_initial + 2^n * 60)`.

After `max_attempts` consecutive failures, the pool SHALL be excluded from the work
queue until the daemon restarts. The counter resets on successful fetch.

#### Scenario: pool fails 3 times then succeeds
- **WHEN** pool X fails 3 consecutive fetches then succeeds on the 4th
- **THEN** the backoff delays were 30s, 90s, 210s (n=0,1,2). After success, the counter
  resets to 0

### Requirement: per-pool-overrides

The daemon SHALL support `fetch.pools_overrides` (list of `{match, overrides}` records)
for fine-tuning individual pools. Supported override keys per pool:
- `max_body_bytes`
- `timeout_seconds`
- `user_agent`
- `private_ip_policy` (override global)
- `enabled` (boolean — override global `fetch.enabled` per pool)

#### Scenario: matched override applied
- **WHEN** `pools_overrides` contains
  `{match: "pool1abc...exact_bech32", overrides: {max_body_bytes: 1048576}}`
- **THEN** pool `pool1abc...` accepts bodies up to 1 MiB; all other pools use the
  global default
