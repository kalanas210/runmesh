package redis_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kalanas210/runmesh/internal/redis"
)

// TestRedisIsConfiguredInCI is TestDatabaseIsConfiguredInCI's exact shape
// (internal/pgstore/pgstore_test.go) for the same reason: a suite that
// silently skips is worse than no suite, because it reports green for a
// client nobody ran against a real server.
func TestRedisIsConfiguredInCI(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("not running in CI")
	}
	if os.Getenv("RUNMESH_TEST_REDIS_URL") == "" {
		t.Fatal("RUNMESH_TEST_REDIS_URL is unset in CI: the Redis client suite " +
			"would have skipped and reported green")
	}
}

func testURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("RUNMESH_TEST_REDIS_URL")
	if u == "" {
		t.Skip("set RUNMESH_TEST_REDIS_URL to run the Redis client suite, " +
			"e.g. redis://127.0.0.1:6379 (docker compose --profile ratelimit up -d --wait redis)")
	}
	return u
}

func newClient(t *testing.T) *redis.Client {
	t.Helper()
	cfg, err := redis.ParseURL(testURL(t))
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	cl := redis.New(cfg)
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func TestParseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		wantAddr string
		wantUser string
		wantPass string
		wantDB   int
	}{
		{"bare host", "redis://127.0.0.1:6379", "127.0.0.1:6379", "", "", 0},
		{"default port", "redis://127.0.0.1", "127.0.0.1:6379", "", "", 0},
		{"password only", "redis://:hunter2@127.0.0.1:6379", "127.0.0.1:6379", "", "hunter2", 0},
		{"acl user and password", "redis://limiter:hunter2@127.0.0.1:6379", "127.0.0.1:6379", "limiter", "hunter2", 0},
		{"database index", "redis://127.0.0.1:6379/3", "127.0.0.1:6379", "", "", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := redis.ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			if cfg.Addr != tc.wantAddr || cfg.Username != tc.wantUser ||
				cfg.Password != tc.wantPass || cfg.DB != tc.wantDB {
				t.Errorf("ParseURL(%q) = %+v, want Addr=%q Username=%q Password=%q DB=%d",
					tc.raw, cfg, tc.wantAddr, tc.wantUser, tc.wantPass, tc.wantDB)
			}
		})
	}
}

func TestParseURLRejectsWrongScheme(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"http://127.0.0.1:6379", "postgres://127.0.0.1", "not a url at all ://"} {
		if _, err := redis.ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) was accepted", raw)
		}
	}
}

// TestParseURLRefusesTLS: rediss:// is recognised and refused with a reason
// naming ADR 0015, not silently downgraded to plain text.
func TestParseURLRefusesTLS(t *testing.T) {
	t.Parallel()
	_, err := redis.ParseURL("rediss://127.0.0.1:6379")
	if err == nil {
		t.Fatal("rediss:// was accepted by a client that cannot speak TLS")
	}
	if !strings.Contains(err.Error(), "ADR 0015") {
		t.Errorf("error = %v, want it to point at the reopening condition", err)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	cl := newClient(t)
	if err := cl.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestDoSetAndGet(t *testing.T) {
	t.Parallel()
	cl := newClient(t)
	key := "runmesh:test:" + t.Name()

	if _, err := cl.Do(t.Context(), "SET", key, "hello"); err != nil {
		t.Fatalf("SET: %v", err)
	}
	v, err := cl.Do(t.Context(), "GET", key)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if string(v.Bulk) != "hello" {
		t.Errorf("GET = %q, want hello", v.Bulk)
	}
	if _, err := cl.Do(t.Context(), "DEL", key); err != nil {
		t.Fatalf("DEL: %v", err)
	}
}

// TestDoOnMissingKeyIsNilNotEmpty pins the distinction TestDecodeNullBulkString
// makes at the protocol layer, now against a real server's own GET semantics.
func TestDoOnMissingKeyIsNilNotEmpty(t *testing.T) {
	t.Parallel()
	cl := newClient(t)
	v, err := cl.Do(t.Context(), "GET", "runmesh:test:definitely-does-not-exist:"+t.Name())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if !v.IsNil() {
		t.Errorf("GET of a missing key = %+v, want a nil bulk reply", v)
	}
}

// TestDoServerErrorIsReturnedAsError: a command RESP refuses — wrong arity
// here — comes back through the same err return every other failure does,
// not as a Value the caller has to remember to inspect for Kind==KindError.
func TestDoServerErrorIsReturnedAsError(t *testing.T) {
	t.Parallel()
	cl := newClient(t)
	_, err := cl.Do(t.Context(), "SET", "onlyonearg")
	if err == nil {
		t.Fatal("SET with the wrong number of arguments was accepted")
	}
}

// TestEvalRunsATokenBucketShapedScript exercises exactly the call shape
// internal/ratelimit makes: a script, one key, returning a flat array of two
// integers — proving EVAL's command construction and the nested-array reply
// decode both work against a real server, not just decodeValue's own table
// tests.
func TestEvalRunsATokenBucketShapedScript(t *testing.T) {
	t.Parallel()
	cl := newClient(t)
	const script = `
		redis.call('SET', KEYS[1], ARGV[1])
		local v = redis.call('GET', KEYS[1])
		return {1, tonumber(v)}
	`
	key := "runmesh:test:eval:" + t.Name()
	v, err := cl.Eval(t.Context(), script, []string{key}, "250")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(v.Array) != 2 || v.Array[0].Int != 1 || v.Array[1].Int != 250 {
		t.Errorf("Eval reply = %+v, want [1, 250]", v)
	}
	_, _ = cl.Do(t.Context(), "DEL", key)
}

// TestConcurrentDoUsesThePoolCorrectly: many goroutines through a small pool,
// each setting and reading back its OWN key. Wrong pool bookkeeping — a
// connection handed to two goroutines at once, or one silently dropped —
// shows up here as a value that does not match what that goroutine wrote,
// not as a deadlock, which is why every goroutine verifies its own
// round trip rather than only checking that nothing panicked.
func TestConcurrentDoUsesThePoolCorrectly(t *testing.T) {
	t.Parallel()
	cfg, err := redis.ParseURL(testURL(t))
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	cfg.PoolSize = 3
	cl := redis.New(cfg)
	t.Cleanup(func() { _ = cl.Close() })

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("runmesh:test:pool:%s:%d", t.Name(), i)
			want := fmt.Sprintf("value-%d", i)
			if _, err := cl.Do(t.Context(), "SET", key, want); err != nil {
				errs <- fmt.Errorf("SET %d: %w", i, err)
				return
			}
			v, err := cl.Do(t.Context(), "GET", key)
			if err != nil {
				errs <- fmt.Errorf("GET %d: %w", i, err)
				return
			}
			if string(v.Bulk) != want {
				errs <- fmt.Errorf("goroutine %d read back %q, want %q", i, v.Bulk, want)
				return
			}
			_, _ = cl.Do(t.Context(), "DEL", key)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestCloseIsSafeConcurrentlyWithRelease: Close must never panic on a send to
// a channel it closed, because it does not close idle at all (see its own
// doc) — this is the test that would fail if a future edit added that close.
func TestCloseIsSafeConcurrentlyWithRelease(t *testing.T) {
	t.Parallel()
	cfg, err := redis.ParseURL(testURL(t))
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	cfg.PoolSize = 2
	cl := redis.New(cfg)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cl.Do(context.Background(), "PING")
		}()
	}
	wg.Wait()
	if err := cl.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
