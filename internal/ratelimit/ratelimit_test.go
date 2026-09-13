package ratelimit_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/ratelimit"
	"github.com/kalanas210/runmesh/internal/redis"
)

func TestRuleEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rule ratelimit.Rule
		want bool
	}{
		{"zero value", ratelimit.Rule{}, false},
		{"rate but no burst", ratelimit.Rule{RatePerSecond: 10}, false},
		{"burst but no rate", ratelimit.Rule{Burst: 10}, false},
		{"negative rate", ratelimit.Rule{RatePerSecond: -1, Burst: 10}, false},
		{"both set", ratelimit.Rule{RatePerSecond: 10, Burst: 10}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rule.Enabled(); got != tc.want {
				t.Errorf("Rule%+v.Enabled() = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}

// ─── httpRequestHost, exercised through Limiter.Allow with PerHost enabled
// and PerTool disabled, since the extractor itself is unexported. Redis is
// still required to reach it — the extraction happens inside Allow, before
// any Redis call for a tool the extractor recognises — but a burst high
// enough that the bucket never actually empties keeps these about parsing,
// not about the token math the dedicated bucket tests below cover.

func newTestLimiter(t *testing.T, cfg ratelimit.Config) *ratelimit.Limiter {
	t.Helper()
	client := testRedisClient(t)
	// Namespaced per test name so parallel subtests never share a bucket key
	// WITHIN one run — cleanupBuckets below is what keeps two separate runs
	// of the same test from sharing one either.
	prefix := "runmesh:test:" + t.Name()
	cleanupBuckets(t, client, prefix)
	cfg.KeyPrefix = prefix
	return ratelimit.New(client, cfg)
}

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("RUNMESH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("set RUNMESH_TEST_REDIS_URL to run the rate limiter suite, " +
			"e.g. redis://127.0.0.1:6379 (docker compose --profile ratelimit up -d --wait redis)")
	}
	rcfg, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	client := redis.New(rcfg)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// cleanupBuckets deletes every key under prefix, both before AND after the
// test runs. Before matters as much as after: a slow-refill Rule (several of
// these tests use one deliberately, so the test's own real time cannot
// accidentally hand back a token mid-run) means a bucket touched by a
// PREVIOUS run of the same test — t.Name() is stable across runs, on
// purpose, so parallel subtests within one run never collide — would still
// read as partially or fully spent otherwise, and every "the first N
// requests succeed" assertion in this file depends on starting from a
// genuinely empty key.
func cleanupBuckets(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	del := func() {
		ctx := context.Background()
		v, err := client.Do(ctx, "KEYS", prefix+":ratelimit:*")
		if err != nil {
			return
		}
		for _, k := range v.Array {
			_, _ = client.Do(ctx, "DEL", string(k.Bulk))
		}
	}
	del()
	t.Cleanup(del)
}

func httpParams(u string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"url": u})
	if err != nil {
		panic(err)
	}
	return b
}

func TestPerHostExtractsHostnameFromHTTPRequestParams(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		PerHost: ratelimit.Rule{RatePerSecond: 100, Burst: 100},
	})
	d, err := l.Allow(t.Context(), "http_request", httpParams("https://Example.COM/path?q=1"))
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed {
		t.Fatal("Allow refused a fresh bucket's first request")
	}
	if d.Key != "host:example.com" {
		t.Errorf("Key = %q, want host:example.com (lower-cased)", d.Key)
	}
}

func TestPerHostIgnoresToolsItDoesNotRecognise(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		PerHost: ratelimit.Rule{RatePerSecond: 100, Burst: 100},
	})
	// echo's params carry no "url" at all, and the tool is not http_request —
	// PerHost must not misfire on it, and with no other dimension configured
	// the only correct answer is unconditionally allowed.
	d, err := l.Allow(t.Context(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed || d.Key != "" {
		t.Errorf("Allow(echo) = %+v, want Allowed=true Key=\"\"", d)
	}
}

func TestPerHostToleratesAMalformedURL(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		PerHost: ratelimit.Rule{RatePerSecond: 100, Burst: 100},
	})
	d, err := l.Allow(t.Context(), "http_request", httpParams("not a url"))
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed {
		t.Error("a step whose url does not even parse was rate-limited instead of left for the tool itself to reject")
	}
}

func TestBothDimensionsDisabledAllowsEverything(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{}) // zero Rules: both disabled
	for range 20 {
		d, err := l.Allow(t.Context(), "http_request", httpParams("https://example.com"))
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !d.Allowed {
			t.Fatal("an unconfigured Limiter refused a request")
		}
	}
}

// TestPerToolExhaustsItsBurstThenRefuses is the bucket's core contract: the
// first Burst requests succeed, the next one does not, and the refusal names
// the bucket that decided it.
func TestPerToolExhaustsItsBurstThenRefuses(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		// A slow refill so the test's own real time (well under a second)
		// cannot accidentally hand back a token mid-run and flake the count.
		PerTool: ratelimit.Rule{RatePerSecond: 0.01, Burst: 3},
	})
	for i := range 3 {
		d, err := l.Allow(t.Context(), "http_request", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("Allow #%d was refused; want the first Burst=3 to succeed", i)
		}
	}
	d, err := l.Allow(t.Context(), "http_request", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Allow #4: %v", err)
	}
	if d.Allowed {
		t.Fatal("a 4th request against Burst=3 was allowed")
	}
	if d.Key != "tool:http_request" {
		t.Errorf("Key = %q, want tool:http_request", d.Key)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want a positive wait for a refused request", d.RetryAfter)
	}
}

// TestPerToolAndPerHostAreIndependentBuckets: exhausting one dimension must
// not touch the other's balance — they are different questions ("is this
// tool running too often" vs "is this host being hit too hard") and a
// shared counter would silently conflate them.
func TestPerToolAndPerHostAreIndependentBuckets(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		PerTool: ratelimit.Rule{RatePerSecond: 0.01, Burst: 1},
		PerHost: ratelimit.Rule{RatePerSecond: 100, Burst: 100},
	})
	params := httpParams("https://example.com")

	first, err := l.Allow(t.Context(), "http_request", params)
	if err != nil || !first.Allowed {
		t.Fatalf("first Allow = %+v, %v; want allowed", first, err)
	}
	// The tool bucket (Burst=1) is now empty. PerHost has ample burst left,
	// but Allow checks PerTool FIRST and must stop there.
	second, err := l.Allow(t.Context(), "http_request", params)
	if err != nil {
		t.Fatalf("second Allow: %v", err)
	}
	if second.Allowed || second.Key != "tool:http_request" {
		t.Errorf("second Allow = %+v, want refused by tool:http_request", second)
	}
}

// TestBucketRefillsOverRealTime is the one place this package waits on the
// real clock rather than a fake one, because the thing under test is Redis's
// OWN TIME command — see script.go's own comment on why the script reads it
// instead of a caller-supplied timestamp — and no clock.Fake this process
// owns can move that.
func TestBucketRefillsOverRealTime(t *testing.T) {
	t.Parallel()
	l := newTestLimiter(t, ratelimit.Config{
		// 20/s refills one token roughly every 50ms; waiting 150ms should
		// comfortably restore at least one without the test taking long.
		PerTool: ratelimit.Rule{RatePerSecond: 20, Burst: 1},
	})
	first, err := l.Allow(t.Context(), "http_request", json.RawMessage(`{}`))
	if err != nil || !first.Allowed {
		t.Fatalf("first Allow = %+v, %v; want allowed", first, err)
	}
	refused, err := l.Allow(t.Context(), "http_request", json.RawMessage(`{}`))
	if err != nil || refused.Allowed {
		t.Fatalf("second Allow (immediate) = %+v, %v; want refused, the bucket just emptied", refused, err)
	}

	time.Sleep(150 * time.Millisecond) // clock:allow: proving Redis's own TIME-driven refill, which no fake clock in this process can advance

	after, err := l.Allow(t.Context(), "http_request", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("third Allow: %v", err)
	}
	if !after.Allowed {
		t.Error("the bucket had not refilled after 150ms at 20 tokens/s")
	}
}

// TestConcurrentAllowNeverOverspendsTheBucket is the property the whole
// design exists for: EVAL's atomicity against a shared bucket, proven by
// throwing more concurrent requests at a small burst than it can satisfy and
// counting exactly how many were allowed.
func TestConcurrentAllowNeverOverspendsTheBucket(t *testing.T) {
	t.Parallel()
	const burst = 10
	l := newTestLimiter(t, ratelimit.Config{
		PerTool: ratelimit.Rule{RatePerSecond: 0.01, Burst: burst},
	})

	const attempts = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(context.Background(), "http_request", json.RawMessage(`{}`))
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != burst {
		t.Errorf("allowed %d of %d concurrent requests against Burst=%d, want exactly %d",
			allowed, attempts, burst, burst)
	}
}

func TestKeyPrefixNamespacesBuckets(t *testing.T) {
	t.Parallel()
	client := testRedisClient(t)

	// t.Name() alone is a unique namespace WITHIN one run; cleanupBuckets is
	// what keeps two separate runs of this test from sharing state too.
	prefix := "runmesh:test:" + t.Name()
	cleanupBuckets(t, client, prefix+":a")
	cleanupBuckets(t, client, prefix+":b")
	a := ratelimit.New(client, ratelimit.Config{
		PerTool: ratelimit.Rule{RatePerSecond: 0.01, Burst: 1}, KeyPrefix: prefix + ":a",
	})
	b := ratelimit.New(client, ratelimit.Config{
		PerTool: ratelimit.Rule{RatePerSecond: 0.01, Burst: 1}, KeyPrefix: prefix + ":b",
	})

	if d, err := a.Allow(t.Context(), "http_request", json.RawMessage(`{}`)); err != nil || !d.Allowed {
		t.Fatalf("a.Allow = %+v, %v; want allowed", d, err)
	}
	// a's bucket is now empty. b, under a different prefix, must be
	// unaffected even though every other configuration value is identical.
	if d, err := b.Allow(t.Context(), "http_request", json.RawMessage(`{}`)); err != nil || !d.Allowed {
		t.Fatalf("b.Allow = %+v, %v; want allowed — a different KeyPrefix must be a different bucket", d, err)
	}
}
