package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/config"
)

const goodKey = "0123456789abcdef0123456789abcdef"

// env builds a getenv function over a map, starting from a minimal valid
// configuration so each test only has to state what it is changing.
func env(overrides map[string]string) func(string) string {
	base := map[string]string{"RUNMESH_API_KEYS": "ci=" + goodKey}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("a minimal environment was rejected: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"HTTPAddr", cfg.HTTPAddr, ":8080"},
		{"Workers", cfg.Workers, 8},
		{"LeaseTTL", cfg.LeaseTTL, 30 * time.Second},
		{"HeartbeatInterval", cfg.HeartbeatInterval, 5 * time.Second},
		{"BackoffBase", cfg.BackoffBase, time.Second},
		{"BackoffFactor", cfg.BackoffFactor, 2.0},
		{"StepTimeout", cfg.Defaults.StepTimeout, 30 * time.Second},
		{"MaxAttempts", cfg.Defaults.MaxAttempts, 3},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"LogFormat", cfg.LogFormat, "json"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// Test tools must be off unless somebody asks for them.
	if cfg.EnableTestTools {
		t.Error("test tools are enabled by default")
	}
	// Owner is derived so two processes on one host do not share a lease owner.
	if cfg.Owner == "" {
		t.Error("Owner is empty; leases would be unattributable")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_HTTP_ADDR":          "127.0.0.1:9999",
		"RUNMESH_WORKERS":            "32",
		"RUNMESH_CLAIM_BATCH":        "16",
		"RUNMESH_LEASE_TTL":          "45s",
		"RUNMESH_HEARTBEAT_INTERVAL": "5s",
		"RUNMESH_BACKOFF_FACTOR":     "1.5",
		"RUNMESH_BACKOFF_JITTER":     "0",
		"RUNMESH_ENABLE_TEST_TOOLS":  "true",
		"RUNMESH_LOG_LEVEL":          "debug",
		"RUNMESH_LOG_FORMAT":         "text",
		"RUNMESH_OWNER":              "worker-7",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9999" || cfg.Workers != 32 || cfg.ClaimBatch != 16 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.LeaseTTL != 45*time.Second || cfg.BackoffFactor != 1.5 || cfg.BackoffJitter != 0 {
		t.Errorf("numeric overrides not applied: %+v", cfg)
	}
	if !cfg.EnableTestTools || cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" {
		t.Errorf("flag overrides not applied: %+v", cfg)
	}
	if cfg.Owner != "worker-7" {
		t.Errorf("Owner = %q, want the configured value", cfg.Owner)
	}
}

// TestRefusesToStartUnauthenticated is the one default that does not exist:
// an unauthenticated server must not be something a forgotten variable can
// produce.
func TestRefusesToStartUnauthenticated(t *testing.T) {
	t.Parallel()
	_, err := config.Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("Load succeeded with no API keys configured")
	}
	if !strings.Contains(err.Error(), "RUNMESH_API_KEYS") {
		t.Errorf("the error does not name the missing variable: %v", err)
	}
}

func TestAPIKeyParsing(t *testing.T) {
	t.Parallel()
	second := strings.Repeat("b", 32)

	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_API_KEYS": "ci=" + goodKey + ", deploy=" + second,
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("%d keys parsed, want 2", len(cfg.APIKeys))
	}
	if id, ok := cfg.APIKeyID(config.KeyDigest(goodKey)); !ok || id != "ci" {
		t.Errorf("key lookup returned (%q, %v), want (\"ci\", true)", id, ok)
	}
	if _, ok := cfg.APIKeyID(config.KeyDigest("not-a-key")); ok {
		t.Error("an unconfigured key was accepted")
	}

	// A bare key is accepted with a generated id: the id exists for log
	// correlation, not for authentication.
	cfg, err = config.Load(env(map[string]string{"RUNMESH_API_KEYS": goodKey}))
	if err != nil {
		t.Fatalf("a bare key was rejected: %v", err)
	}
	if len(cfg.APIKeys) != 1 {
		t.Errorf("%d keys parsed from a bare value, want 1", len(cfg.APIKeys))
	}

	// A short key is refused loudly rather than silently accepted.
	if _, err := config.Load(env(map[string]string{"RUNMESH_API_KEYS": "ci=short"})); err == nil {
		t.Error("a 5-character API key was accepted")
	}
}

// TestCrossFieldInvariants walks every relationship that produces a subtle
// runtime failure rather than an obvious one. Each is caught at boot, with a
// sentence saying why it matters.
func TestCrossFieldInvariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantHit string
	}{
		{
			name:    "workers below one",
			env:     map[string]string{"RUNMESH_WORKERS": "0"},
			wantHit: "RUNMESH_WORKERS",
		},
		{
			name:    "claim batch larger than the pool",
			env:     map[string]string{"RUNMESH_WORKERS": "4", "RUNMESH_CLAIM_BATCH": "8"},
			wantHit: "RUNMESH_CLAIM_BATCH",
		},
		{
			// Fewer than three heartbeats per lease means one slow store call
			// can expire a lease under a step that is running perfectly well.
			name:    "heartbeat too slow for the lease",
			env:     map[string]string{"RUNMESH_LEASE_TTL": "9s", "RUNMESH_HEARTBEAT_INTERVAL": "5s"},
			wantHit: "RUNMESH_HEARTBEAT_INTERVAL",
		},
		{
			// A store write must not be able to outlive the drain it is part of.
			name:    "store timeout longer than the drain",
			env:     map[string]string{"RUNMESH_STORE_TIMEOUT": "40s", "RUNMESH_DRAIN_TIMEOUT": "30s"},
			wantHit: "RUNMESH_STORE_TIMEOUT",
		},
		{
			// Waiting longer than a lease for an unresponsive tool would let the
			// reconciler hand the step to somebody else while we still wait.
			name:    "abandon grace longer than the lease",
			env:     map[string]string{"RUNMESH_LEASE_TTL": "30s", "RUNMESH_ABANDON_GRACE": "60s"},
			wantHit: "RUNMESH_ABANDON_GRACE",
		},
		{
			name:    "backoff max below base",
			env:     map[string]string{"RUNMESH_BACKOFF_BASE": "10s", "RUNMESH_BACKOFF_MAX": "1s"},
			wantHit: "RUNMESH_BACKOFF_MAX",
		},
		{
			name:    "backoff factor below one",
			env:     map[string]string{"RUNMESH_BACKOFF_FACTOR": "0.5"},
			wantHit: "RUNMESH_BACKOFF_FACTOR",
		},
		{
			name:    "jitter out of range",
			env:     map[string]string{"RUNMESH_BACKOFF_JITTER": "1.5"},
			wantHit: "RUNMESH_BACKOFF_JITTER",
		},
		{
			// The backstop must fire after the graceful path has had its full
			// budget, or it would abort a shutdown that was going to succeed.
			name: "hard exit before the graceful path could finish",
			env: map[string]string{
				"RUNMESH_SHUTDOWN_GRACE":  "30s",
				"RUNMESH_DRAIN_TIMEOUT":   "30s",
				"RUNMESH_HARD_EXIT_AFTER": "40s",
			},
			wantHit: "RUNMESH_HARD_EXIT_AFTER",
		},
		{
			name:    "default step timeout above its own ceiling",
			env:     map[string]string{"RUNMESH_STEP_TIMEOUT": "1h", "RUNMESH_MAX_STEP_TIMEOUT": "15m"},
			wantHit: "RUNMESH_STEP_TIMEOUT",
		},
		{
			name:    "default attempts above the ceiling",
			env:     map[string]string{"RUNMESH_MAX_ATTEMPTS": "50", "RUNMESH_MAX_ATTEMPTS_LIMIT": "10"},
			wantHit: "RUNMESH_MAX_ATTEMPTS",
		},
		{
			name:    "unknown log format",
			env:     map[string]string{"RUNMESH_LOG_FORMAT": "xml"},
			wantHit: "RUNMESH_LOG_FORMAT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(env(tc.env))
			if err == nil {
				t.Fatal("an invalid configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantHit) {
				t.Errorf("the error does not name %s:\n%v", tc.wantHit, err)
			}
		})
	}
}

func TestParseErrorsAreReported(t *testing.T) {
	t.Parallel()
	for name, e := range map[string]map[string]string{
		"bad duration": {"RUNMESH_LEASE_TTL": "thirty seconds"},
		"bad integer":  {"RUNMESH_WORKERS": "many"},
		"bad float":    {"RUNMESH_BACKOFF_FACTOR": "two"},
		"bad boolean":  {"RUNMESH_ENABLE_TEST_TOOLS": "yes please"},
		"bad level":    {"RUNMESH_LOG_LEVEL": "chatty"},
	} {
		if _, err := config.Load(env(e)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestEveryProblemIsReportedAtOnce: an operator fixing a misconfigured
// deployment should not have to restart the process once per mistake.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, err := config.Load(func(k string) string {
		return map[string]string{
			"RUNMESH_WORKERS":        "0",
			"RUNMESH_BACKOFF_FACTOR": "0.1",
			"RUNMESH_LOG_FORMAT":     "yaml",
		}[k]
	})
	if err == nil {
		t.Fatal("an invalid configuration was accepted")
	}
	for _, want := range []string{
		"RUNMESH_API_KEYS", "RUNMESH_WORKERS", "RUNMESH_BACKOFF_FACTOR", "RUNMESH_LOG_FORMAT",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the combined error does not mention %s:\n%v", want, err)
		}
	}
}

func TestWhitespaceIsTolerated(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_WORKERS":  "  4  ",
		"RUNMESH_API_KEYS": "  ci = " + goodKey + "  ",
	}))
	if err != nil {
		t.Fatalf("padded values were rejected: %v", err)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
	if _, ok := cfg.APIKeyID(config.KeyDigest(goodKey)); !ok {
		t.Error("a padded API key was not recognised")
	}
}
