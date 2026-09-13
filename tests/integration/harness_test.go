//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/pgstore"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/storetest"
	"github.com/kalanas210/runmesh/internal/tools"
)

const testKey = "integration-key-0123456789abcdef"

// ─── the stack ───────────────────────────────────────────────────────────────

// harness is a whole RunMesh, assembled the way cmd/server assembles one, with
// two differences that the tests here depend on.
//
// FIRST, THE ENGINE IS NOT RUNNING WHEN IT IS BUILT. run() starts it
// immediately, which is correct for a server and useless for a failure test:
// every case below has to put the store into a specific state — a step with an
// orphaned lease, a queue at its admission ceiling — BEFORE anything starts
// draining it. A harness that started the pool at construction would make every
// one of these a race against the dispatcher.
//
// SECOND, IT IS ONE PROCESS. cmd/server/crash_test.go is the test that needs two
// processes and a real SIGKILL; see doc.go for why these do not.
//
// This is deliberately a small duplication of the wiring in cmd/server/run.go,
// and it is worth naming as one. The rule that run() is the only wiring in the
// codebase is a rule about PRODUCTION wiring: nothing outside it may reach for a
// global or invent a dependency graph the server does not have. A test that
// constructs the same graph with the same constructors, in the same order, is
// held to the same shape by the compiler — if a dependency becomes mandatory in
// run(), this file stops building too.
type harness struct {
	t     *testing.T
	store storetest.Store
	eng   *engine.Engine
	srv   *httptest.Server
	base  string

	started bool
}

// runtimeConfig is the compressed timing every case here runs with.
//
// Compressed, not defaulted, and each value has its own reason. LeaseTTL is one
// second because a lease-expiry test that waited the shipped thirty would be a
// thirty-second test nobody runs. ReconcileInterval is 100ms because the sweep
// is the subject rather than a backstop. HeartbeatInterval is 250ms because the
// cancellation test's whole mechanism is a directive riding back on a heartbeat,
// and the shipped five seconds would make that test measure the heartbeat
// interval and nothing else. The invariants config.Validate enforces still hold:
// at least three heartbeats inside a lease, and an abandon grace shorter than
// the lease it protects.
var runtimeConfig = engine.Config{
	Owner:             "integration",
	Workers:           4,
	ClaimBatch:        4,
	PollInterval:      10 * time.Millisecond,
	LeaseTTL:          time.Second,
	HeartbeatInterval: 250 * time.Millisecond,
	StoreTimeout:      2 * time.Second,
	AbandonGrace:      300 * time.Millisecond,
	ReconcileInterval: 100 * time.Millisecond,
	ReconcileBatch:    100,
	Backoff:           engine.Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2},
}

type options struct {
	maxQueueDepth int
	workers       int
}

func newHarness(t *testing.T, be backend, opt options) *harness {
	t.Helper()

	if opt.maxQueueDepth == 0 {
		opt.maxQueueDepth = 1000
	}
	cfg := runtimeConfig
	if opt.workers > 0 {
		cfg.Workers, cfg.ClaimBatch = opt.workers, opt.workers
	}

	h := &harness{t: t, store: be.open(t)}

	registry := tools.Builtins(tools.Options{EnableTestTools: true})

	eng, err := engine.New(cfg, engine.Deps{
		Store:    h.store,
		Executor: tools.Local{Registry: registry, MaxOutputBytes: 64 << 10, Log: quiet()},
		Clock:    clock.System(),
		Log:      quiet(),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.eng = eng

	handler, _, err := httpapi.New(httpapi.Deps{
		Store:   h.store,
		Tools:   registry,
		Runtime: eng,
		Clock:   clock.System(),
		Log:     quiet(),
		APIKeys: map[[32]byte]config.APIKey{
			config.KeyDigest(testKey): {ID: "integration", Unscoped: true},
		},
		Limits: runmesh.Limits{
			MaxSteps: 100, MaxDependsOn: 32, MaxParamsBytes: 64 << 10,
			MaxStepTimeout: time.Minute, MaxAttempts: 10, MaxResultBytes: 256 << 10,
		},
		Defaults:        runmesh.Defaults{StepTimeout: 10 * time.Second, MaxAttempts: 3},
		MaxRequestBytes: 1 << 20,
		MaxQueueDepth:   opt.maxQueueDepth,
		Durable:         be.durable,
		StoreName:       be.name,
		Sandbox:         openPolicy{registry: registry},
		ExecutionMode:   tools.ModeInProcess,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	h.srv = httptest.NewServer(handler)
	h.base = h.srv.URL
	t.Cleanup(func() {
		h.srv.Close()
		h.stop()
		_ = h.store.Close()
	})
	return h
}

// start launches the runtime. Every case calls it at the moment it is ready for
// work to begin moving, and several call it AFTER they have already broken
// something, which is the whole reason it is not folded into newHarness.
func (h *harness) start() {
	h.t.Helper()
	if h.started {
		return
	}
	if err := h.eng.Start(context.Background()); err != nil {
		h.t.Fatalf("engine.Start: %v", err)
	}
	h.started = true
}

func (h *harness) stop() {
	if !h.started {
		return
	}
	h.started = false
	ctx, cancel := clock.WithWriteDeadline(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.eng.Shutdown(ctx); err != nil {
		h.t.Errorf("engine.Shutdown: %v", err)
	}
}

// openPolicy is the execution policy for these tests: everything is allowed.
//
// It is a four-line stub rather than a real policy.Sandbox because none of these
// cases is about the policy — internal/policy has its own tests, and a denied
// tool here would only change which error a step failed with. Satisfying
// httpapi.Policy in four lines is exactly what a consumer-declared interface is
// for, and the API's own test fixtures do the same.
type openPolicy struct{ registry tools.Registry }

func (openPolicy) Allows(string) error               { return nil }
func (p openPolicy) Descriptors() []tools.Descriptor { return p.registry.Descriptors() }
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ─── backends ────────────────────────────────────────────────────────────────

// backend is one store implementation to run a case against.
//
// Every case here runs against both, when both are available, for the reason
// internal/storetest exists: these are assertions about lease expiry, retry
// budget and fail-fast, and all three are implemented twice — once as Go under
// a mutex and once as SQL under a row lock. A failure test that only ever ran
// against the in-memory simulation would be proving a property of the
// simulation.
type backend struct {
	name    string
	durable bool
	open    func(t *testing.T) storetest.Store
}

func backends(t *testing.T) []backend {
	t.Helper()
	out := []backend{{
		name:    "memory",
		durable: false,
		open: func(t *testing.T) storetest.Store {
			return memstore.New(memstore.Options{})
		},
	}}
	// The repository's skip convention, unchanged: without a database the
	// PostgreSQL rows are absent rather than red, so `go test ./...` works on a
	// laptop, and internal/pgstore's TestDatabaseIsConfiguredInCI is what makes
	// the skip impossible in CI.
	if os.Getenv("RUNMESH_TEST_DATABASE_URL") != "" {
		out = append(out, backend{name: "postgres", durable: true, open: newPGStore})
	}
	return out
}

func newPGStore(t *testing.T) storetest.Store {
	t.Helper()

	dsn := os.Getenv("RUNMESH_TEST_DATABASE_URL")

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generating a schema name: %v", err)
	}
	schema := "it_" + hex.EncodeToString(raw[:])

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening the admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("RUNMESH_TEST_DATABASE_URL is not a URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("opening %s: %v", schema, err)
	}
	// Larger than the worker count, for the reason pgstore.Options states: a
	// pool that does not exceed it can deadlock a drain, with every worker
	// holding a connection to settle while the dispatcher waits for one to
	// claim.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)

	if _, err := pgstore.Migrate(t.Context(), db, quiet()); err != nil {
		t.Fatalf("migrating %s: %v", schema, err)
	}
	s, err := pgstore.New(db, pgstore.Options{Log: quiet()})
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		dctx, cancel := clock.WithWriteDeadline(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = drop.ExecContext(dctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	return s
}

// ─── HTTP ────────────────────────────────────────────────────────────────────

type jobView struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Steps []struct {
		ID          string   `json:"id"`
		Tool        string   `json:"tool"`
		State       string   `json:"state"`
		Attempt     int      `json:"attempt"`
		Failures    int      `json:"failures"`
		MaxAttempts int      `json:"max_attempts"`
		DependsOn   []string `json:"depends_on"`
		Error       *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"steps"`
}

func (j jobView) step(t *testing.T, id string) int {
	t.Helper()
	for i, s := range j.Steps {
		if s.ID == id {
			return i
		}
	}
	t.Fatalf("job %s has no step %q", j.ID, id)
	return -1
}

func (h *harness) do(method, path, body string) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.base+path, rdr)
	if err != nil {
		h.t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func (h *harness) submit(plan string) jobView {
	h.t.Helper()
	status, body := h.do(http.MethodPost, "/api/v1/jobs", plan)
	if status != http.StatusCreated {
		h.t.Fatalf("POST /api/v1/jobs = %d, want 201. body: %s", status, body)
	}
	var job jobView
	if err := json.Unmarshal(body, &job); err != nil {
		h.t.Fatalf("decoding the created job: %v (body %s)", err, body)
	}
	return job
}

func (h *harness) job(id string) jobView {
	h.t.Helper()
	status, body := h.do(http.MethodGet, "/api/v1/jobs/"+id, "")
	if status != http.StatusOK {
		h.t.Fatalf("GET /api/v1/jobs/%s = %d, body %s", id, status, body)
	}
	var job jobView
	if err := json.Unmarshal(body, &job); err != nil {
		h.t.Fatalf("decoding job %s: %v (body %s)", id, err, body)
	}
	return job
}

// timeline is the event stream, which is what these tests assert against
// wherever they can.
//
// A final-status check says a job ended up in the right state. The timeline says
// HOW — and the difference is the entire subject here: a step that was reclaimed
// by the reconciler and a step that simply retried both end SUCCEEDED, and only
// the presence of STEP_LEASE_EXPIRED distinguishes a recovery from a runtime
// that got lucky.
func (h *harness) timeline(id string) []runmesh.Event {
	h.t.Helper()
	status, body := h.do(http.MethodGet, "/api/v1/jobs/"+id+"/events?limit=1000", "")
	if status != http.StatusOK {
		h.t.Fatalf("GET events for %s = %d, body %s", id, status, body)
	}
	var page struct {
		Events    []runmesh.Event `json:"events"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		h.t.Fatalf("decoding the timeline for %s: %v (body %s)", id, err, body)
	}
	if page.Truncated {
		h.t.Fatalf("the timeline for %s is truncated; the assertions below would be reading a "+
			"partial history and could pass or fail for the wrong reason", id)
	}
	return page.Events
}

func hasEvent(events []runmesh.Event, t runmesh.EventType, stepID string) bool {
	for _, e := range events {
		if e.Type == t && (stepID == "" || e.StepID == stepID) {
			return true
		}
	}
	return false
}

func eventTypes(events []runmesh.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Type)
	}
	return out
}

// ─── waiting ─────────────────────────────────────────────────────────────────

// waitFor polls until want reports true, then returns the last view it read.
//
// It polls, and the poll is a real sleep, and that is worth stating rather than
// hiding. Every other test in RunMesh drives a clock.Fake and synchronises on
// the event stream, and internal/clock/purity_test.go enforces it — over
// internal/, which this directory is not under. These cases cannot do that: they
// drive a real engine with real goroutines over a real HTTP listener, so there
// is no clock to advance and nothing to synchronise on but elapsed time. This is
// the same concession cmd/server/crash_test.go makes, for the same reason, and
// it is confined to this one function for the same reason it is confined to one
// there.
func (h *harness) waitFor(id string, within time.Duration, why string, want func(jobView) bool) jobView {
	h.t.Helper()

	deadline := clock.System().Now().Add(within)
	var last jobView
	for {
		last = h.job(id)
		if want(last) {
			return last
		}
		if clock.System().Now().After(deadline) {
			h.t.Fatalf("job %s never %s within %s; last state %s, steps %s\ntimeline: %v",
				id, why, within, last.State, stepStates(last), eventTypes(h.timeline(id)))
		}
		poll()
	}
}

func (h *harness) waitForJobState(id, want string, within time.Duration) jobView {
	h.t.Helper()
	return h.waitFor(id, within, "reached "+want, func(j jobView) bool { return j.State == want })
}

func (h *harness) waitForStepState(id, stepID, want string, within time.Duration) jobView {
	h.t.Helper()
	return h.waitFor(id, within, "saw "+stepID+" reach "+want, func(j jobView) bool {
		for _, s := range j.Steps {
			if s.ID == stepID {
				return s.State == want
			}
		}
		return false
	})
}

func stepStates(j jobView) string {
	var b strings.Builder
	for i, s := range j.Steps {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(s.ID + "=" + s.State)
	}
	return b.String()
}

// poll is the one place this package waits. See waitFor for why it is allowed
// to be a sleep and why it is the only one.
func poll() { time.Sleep(20 * time.Millisecond) }
