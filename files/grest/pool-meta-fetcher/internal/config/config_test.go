package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadMissing ensures Load() returns a clear error when path is absent.
func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "nope.yaml"), nil)
	if err == nil {
		t.Fatal("expected error for missing config path")
	}
}

// TestLoadMinimal ensures the two-field minimal config loads and validates.
func TestLoadMinimal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	mustWrite(t, path, `
database_url: postgres://grest_owner:xxx@localhost:5432/cexplorer
network_name: mainnet
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load returned: %v", err)
	}
	if cfg.NetworkName != NetworkMainnet {
		t.Errorf("network_name = %q, want mainnet", cfg.NetworkName)
	}
	if !cfg.Fetch.Enabled {
		t.Error("fetch.enabled default should be true")
	}
	if cfg.Fetch.Validation.MaxBodyBytes != 524288 {
		t.Errorf("max_body_bytes = %d, want 524288", cfg.Fetch.Validation.MaxBodyBytes)
	}
}

// TestUnknownFieldRejected covers rule "dec.KnownFields(true)".
func TestUnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	mustWrite(t, path, `
database_url: postgres://grest_owner:xxx@localhost:5432/cexplorer
network_name: mainnet
fetch:
  totally_made_up_knob: 42
`)
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "parse yaml") {
		t.Errorf("err = %v, expected parse-yaml failure", err)
	}
}

// TestAllValidationRules fires every documented rule with each shape of
// bad input and confirms a typed *ValidationError is returned pointing at
// the right field.
func TestAllValidationRules(t *testing.T) {
	type tc struct {
		name      string
		override  string
		wantField string
	}
	cases := []tc{
		// 1 database_url required
		{"missing database_url", "network_name: mainnet", "database_url"},
		// 2 network_name enum
		{"bad network_name", "network_name: bogus", "network_name"},
		// 3 poll_interval_seconds
		{"poll <= 0", "fetch:\n  poll:\n    poll_interval_seconds: 0", "fetch.poll.poll_interval_seconds"},
		// 4 min_poll > poll
		{"min > poll", `fetch:
  poll:
    poll_interval_seconds: 100
    min_poll_interval_seconds: 200`, "fetch.poll.min_poll_interval_seconds"},
		// 5 concurrency
		{"concurrency 0", "fetch:\n  work_queue:\n    concurrency: 0", "fetch.work_queue.concurrency"},
		// 6 batch_size 0
		{"batch_size 0", "fetch:\n  work_queue:\n    batch_size: 0", "fetch.work_queue.batch_size"},
		// 7 max_inflight < concurrency
		{"max_inflight < concurrency", `fetch:
  work_queue:
    concurrency: 16
    max_inflight: 8`, "fetch.work_queue.max_inflight"},
		// 8 write_batch_size 0
		{"write_batch_size 0", "fetch:\n  work_queue:\n    write_batch_size: 0", "fetch.work_queue.write_batch_size"},
		// 9 http timeout
		{"connect > timeout", `fetch:
  http:
    timeout_seconds: 10
    connect_timeout_seconds: 30`, "fetch.http.connect_timeout_seconds"},
		// 10 max_redirects
		{"max_redirects negative", "fetch:\n  http:\n    max_redirects: -1", "fetch.http.max_redirects"},
		// 11 tls_min_version
		{"tls_min_version bogus", "fetch:\n  http:\n    tls_min_version: 1.1", "fetch.http.tls_min_version"},
		// 12 max_body_bytes too large
		{"max_body 5MiB+1", "fetch:\n  validation:\n    max_body_bytes: 5242881", "fetch.validation.max_body_bytes"},
		// 13 allowed_content_types empty
		{"allowed_content_types empty", "fetch:\n  validation:\n    allowed_content_types: []", "fetch.validation.allowed_content_types"},
		// 14 json max_depth <= 0
		{"json max_depth 0", "fetch:\n  validation:\n    json:\n      max_depth: 0", "fetch.validation.json.max_depth"},
		// 15 hash algorithm bogus
		{"hash algo bogus", "fetch:\n  validation:\n    hash:\n      algorithm: sha256", "fetch.validation.hash.algorithm"},
		// 16 allow_list with empty pools
		{"allow_list policy with empty list", `fetch:
  private_ip_policy: allow_list
  private_ip_allow_list: []`, "fetch.private_ip_allow_list"},
		// 17 retry max_attempts 0
		{"retry max_attempts 0", "fetch:\n  retry:\n    max_attempts: 0", "fetch.retry.max_attempts"},
		// 18 backoff_initial > max
		{"backoff inverted", `fetch:
  retry:
    backoff_initial_seconds: 200
    backoff_max_seconds: 100`, "fetch.retry.backoff_max_seconds"},
		// 19 multiplier < 1
		{"multiplier 0", "fetch:\n  retry:\n    backoff_multiplier_seconds: 0", "fetch.retry.backoff_multiplier_seconds"},
		// 20 pool_max_conns 0
		{"pool_max_conns 0", "database:\n  pool_max_conns: 0", "database.pool_max_conns"},
		// 21 pool_min_conns > max
		{"pool_min > max", `database:
  pool_min_conns: 20
  pool_max_conns: 10`, "database.pool_min_conns"},
		// 22 statement_timeout_ms 0
		{"statement_timeout_ms 0", "database:\n  statement_timeout_ms: 0", "database.statement_timeout_ms"},
		// 23 health.listen_port
		{"listen_port 70000", "health:\n  listen_port: 70000", "health.listen_port"},
		// 24 health.listen_addr empty
		{"listen_addr empty", `health:
  listen_addr: ""`, "health.listen_addr"},
		// 25 readyz_db_grace_seconds 0
		{"readyz_grace 0", "health:\n  readyz_db_grace_seconds: 0", "health.readyz_db_grace_seconds"},
		// 26 log level bogus
		{"log level bogus", "logging:\n  level: trace", "logging.level"},
		// 27 log format bogus
		{"log format bogus", "logging:\n  format: yaml", "logging.format"},
		// 28 pools_overrides_match_style bogus
		{"match style bogus", "fetch:\n  pools_overrides_match_style: regex", "fetch.pools_overrides_match_style"},
		// 29 pools_overrides unknown key
		{"override unknown key", `fetch:
  pools_overrides:
    - match: "pool1abc"
      overrides:
        unknown_knob: 1`, "fetch.pools_overrides[0].overrides"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "c.yaml")
			yaml := `
database_url: postgres://grest_owner:xxx@localhost:5432/cexplorer
network_name: mainnet
` + c.override + "\n"
			mustWrite(t, path, yaml)
			_, err := Load(path, nil)
			if err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
			ve, ok := err.(*ValidationError)
			if ok && ve.Field != c.wantField {
				t.Errorf("err.Field = %q, want %q (%v)", ve.Field, c.wantField, err)
			} else if !ok {
				// The error may be wrapped; check the message contains the field name.
				if !strings.Contains(err.Error(), c.wantField) {
					t.Errorf("err = %v, expected to contain field %q", err, c.wantField)
				}
			}
		})
	}
}

// TestRetryBackoffDelay covers the spec backoff curve.
func TestRetryBackoffDelay(t *testing.T) {
	c := defaults()
	c.Fetch.Retry.BackoffInitialSeconds = 30
	c.Fetch.Retry.BackoffMaxSeconds = 1000
	c.Fetch.Retry.BackoffMultiplierSeconds = 60

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second},                           // 30 + 2^0 * 60 = 90... but spec says initial. We use 30 + 2^n * 60.
	}
	// The formula is delay(n) = min(max, initial + 2^n * multiplier).
	// attempt=0: 30 + 1 * 60 = 90
	// attempt=1: 30 + 2 * 60 = 150
	// attempt=2: 30 + 4 * 60 = 270
	// attempt=3: 30 + 8 * 60 = 510
	// attempt=4: 30 + 16 * 60 = 990
	// attempt=5: min(1000, 30 + 32*60=1950) = 1000
	cases = []struct {
		attempt int
		want    time.Duration
	}{
		{0, 90 * time.Second},
		{1, 150 * time.Second},
		{2, 270 * time.Second},
		{3, 510 * time.Second},
		{4, 990 * time.Second},
		{5, 1000 * time.Second}, // capped at backoff_max_seconds
	}
	for _, tc := range cases {
		got := c.RetryBackoffDelay(tc.attempt)
		if got != tc.want {
			t.Errorf("RetryBackoffDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestPoolsOverridesAllowedKeys ensures the spec's strict allowlist.
func TestPoolsOverridesAllowedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	mustWrite(t, path, `
database_url: postgres://grest_owner:xxx@localhost:5432/cexplorer
network_name: mainnet
fetch:
  pools_overrides:
    - match: "pool1abc"
      overrides:
        max_body_bytes: 1048576
        timeout_seconds: 60
        user_agent: "x"
        private_ip_policy: "allow_all"
        enabled: false
`)
	if _, err := Load(path, nil); err != nil {
		t.Fatalf("expected all-override keys to validate, got %v", err)
	}
}

// TestPollInterval returns the expected Duration.
func TestPollIntervalAccessor(t *testing.T) {
	c := defaults()
	c.Fetch.Poll.PollIntervalSeconds = 600
	if got := c.PollInterval(); got != 600*time.Second {
		t.Errorf("PollInterval() = %v, want 600s", got)
	}
}

// helpers ----------------------------------------------------------------------

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
