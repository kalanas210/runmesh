package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// crashChildEnv makes this test binary run the SERVER instead of the tests.
//
// The helper-process pattern, because a crash cannot be simulated in-process.
// Cancelling a context runs the graceful drain, which releases every lease on
// the way out — the opposite of what is being tested. Only a real process that
// is really killed leaves the database in the state a crash leaves it in:
// leases held by an owner that will never come back.
const crashChildEnv = "RUNMESH_CRASH_TEST_CHILD"

// TestCrashRecoveryPreservesWork is what PostgreSQL was for.
//
// Week 1 had leases, a reconciler and a documented recovery path, and none of
// it could ever run: the store died with the process, so the sweep always woke
// up to an empty world. This kills a server mid-step — SIGKILL, no drain, no
// warning — starts a different process against the same database, and requires
// the job to finish.
//
// It also pins the budget asymmetry end to end. The crash costs exactly one
// unit of retry budget, because a worker that reliably dies on one step must
// eventually exhaust max_attempts rather than crash-loop the fleet; a graceful
// drain, tested elsewhere, costs none.
func TestCrashRecoveryPreservesWork(t *testing.T) {
	// The child re-enters here. Anything before this is the parent's setup and
	// must not run twice.
	if os.Getenv(crashChildEnv) == "1" {
		os.Exit(run(context.Background(), nil, os.Getenv, os.Stdout, os.Stderr))
	}

	dsn := crashTestDSN(t)
	schemaDSN := isolatedSchema(t, dsn)

	port := freePort(t)
	addr := net.JoinHostPort("127.0.0.1", port)
	baseURL := "http://" + addr

	env := crashEnv(schemaDSN, addr)

	// ── 1. a server, a job, and a step that is definitely still running ──────
	child := exec.Command(os.Args[0], "-test.run=^TestCrashRecoveryPreservesWork$", "-test.v")
	child.Env = append(os.Environ(), crashChildEnv+"=1")
	for k, v := range env {
		child.Env = append(child.Env, k+"="+v)
	}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child server: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = child.Process.Kill()
		}
		_ = child.Wait()
	})
	waitReady(t, baseURL)

	jobID := submitSleep(t, baseURL, "2s")
	waitStepState(t, baseURL, jobID, "RUNNING")

	// ── 2. the crash ─────────────────────────────────────────────────────────
	// Kill, not Signal: no handler runs, no drain happens, and the lease this
	// process is holding stays in the database with nobody to renew it.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("killing the child server: %v", err)
	}
	killed = true
	_ = child.Wait()

	// ── 3. a different process, the same database ────────────────────────────
	// A fresh port: the dead child's is free, but reusing it invites a bind
	// failure from a socket still in TIME_WAIT, which would look like a
	// recovery bug and would not be one.
	successorEnv := crashEnv(schemaDSN, "127.0.0.1:0")
	successorURL, stop := boot(t, successorEnv)
	t.Cleanup(func() { stop() })

	// The job is still there. In Week 1 this alone would have failed.
	job := getJob(t, successorURL, jobID)
	if job.ID != jobID {
		t.Fatalf("the successor could not see job %s at all", jobID)
	}

	// ── 4. and it finishes ───────────────────────────────────────────────────
	final := waitJobState(t, successorURL, jobID, "SUCCEEDED", 30*time.Second)

	step := final.Steps[0]
	if step.Attempt < 2 {
		t.Errorf("step attempt = %d, want at least 2: the crashed attempt must not "+
			"be reused, because attempt names the execution", step.Attempt)
	}
	if step.Failures != 1 {
		t.Errorf("step failures = %d, want exactly 1. A lease that simply expired "+
			"spends one unit of budget — a worker that reliably dies on one step has "+
			"to exhaust max_attempts rather than crash-loop the fleet.", step.Failures)
	}

	// The timeline says what happened, which is the difference between a system
	// that recovered and a system that merely ended up in the right state.
	types := eventTypes(t, successorURL, jobID)
	if !containsString(types, "STEP_LEASE_EXPIRED") {
		t.Errorf("timeline = %v, want a STEP_LEASE_EXPIRED recording the reclaim", types)
	}
}

// ─── harness ─────────────────────────────────────────────────────────────────

func crashTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RUNMESH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RUNMESH_TEST_DATABASE_URL to run the crash-recovery test")
	}
	return dsn
}

func crashEnv(dsn, addr string) map[string]string {
	return map[string]string{
		"RUNMESH_HTTP_ADDR":         addr,
		"RUNMESH_API_KEYS":          "ci=" + testKey,
		"RUNMESH_DATABASE_URL":      dsn,
		"RUNMESH_WORKERS":           "4",
		"RUNMESH_CLAIM_BATCH":       "4",
		"RUNMESH_ENABLE_TEST_TOOLS": "true",
		"RUNMESH_LOG_LEVEL":         "error",
		"RUNMESH_POLL_INTERVAL":     "20ms",

		// Compressed so the sweep is observable in a test rather than in half a
		// minute. The invariants config enforces still hold: three heartbeats
		// per lease, and an abandon grace shorter than the lease it protects.
		"RUNMESH_LEASE_TTL":          "2s",
		"RUNMESH_HEARTBEAT_INTERVAL": "500ms",
		"RUNMESH_ABANDON_GRACE":      "1s",
		"RUNMESH_RECONCILE_INTERVAL": "200ms",

		"RUNMESH_STORE_TIMEOUT":   "1s",
		"RUNMESH_SHUTDOWN_GRACE":  "2s",
		"RUNMESH_DRAIN_TIMEOUT":   "3s",
		"RUNMESH_HARD_EXIT_AFTER": "30s",
	}
}

// isolatedSchema gives this test its own schema so it cannot collide with the
// conformance suite running against the same database.
func isolatedSchema(t *testing.T, dsn string) string {
	t.Helper()

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generating a schema name: %v", err)
	}
	schema := "crash_" + hex.EncodeToString(raw[:])

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening the admin connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("RUNMESH_TEST_DATABASE_URL is not a URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// freePort asks the kernel for a port and gives it straight back. There is a
// window in which something else could take it; on a test machine that is a
// theoretical concern, and the alternative is production code that exists only
// to report a bound address to a subprocess.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("reading the reserved port: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return port
}

func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/v1/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		poll()
	}
	t.Fatal("the child server never became ready")
}

func submitSleep(t *testing.T, baseURL, duration string) string {
	t.Helper()
	plan := `{"name":"crash","steps":[{"id":"work","tool":"sleep",` +
		`"params":{"duration":"` + duration + `"},"max_attempts":5}]}`

	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /jobs = %d, want 201. body: %s", status, body)
	}
	var job jobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("decode created job: %v (body %s)", err, body)
	}
	return job.ID
}

func getJob(t *testing.T, baseURL, jobID string) jobView {
	t.Helper()
	status, body := request(t, http.MethodGet, baseURL+"/api/v1/jobs/"+jobID, "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET job %s = %d, body %s", jobID, status, body)
	}
	var job jobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("decode job %s: %v (body %s)", jobID, err, body)
	}
	return job
}

func waitStepState(t *testing.T, baseURL, jobID, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job := getJob(t, baseURL, jobID)
		if len(job.Steps) > 0 && job.Steps[0].State == want {
			return
		}
		poll()
	}
	t.Fatalf("step never reached %s", want)
}

func waitJobState(t *testing.T, baseURL, jobID, want string, within time.Duration) jobView {
	t.Helper()
	deadline := time.Now().Add(within)
	var last jobView
	for time.Now().Before(deadline) {
		last = getJob(t, baseURL, jobID)
		if last.State == want {
			return last
		}
		poll()
	}
	t.Fatalf("job %s is %s after %s, want %s. Recovery did not happen.",
		jobID, last.State, within, want)
	return last
}

func eventTypes(t *testing.T, baseURL, jobID string) []string {
	t.Helper()
	status, body := request(t, http.MethodGet,
		baseURL+"/api/v1/jobs/"+jobID+"/events?limit=200", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET events = %d, body %s", status, body)
	}
	var page struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode the timeline: %v (body %s)", err, body)
	}
	out := make([]string, len(page.Events))
	for i, e := range page.Events {
		out[i] = e.Type
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// poll is the one place this file waits.
//
// It is a real sleep, and it is deliberate: this test drives a separate OS
// process over a network socket, so there is no injected clock to advance and
// nothing to synchronise on but the passage of time. Every other test in
// RunMesh asserts on the event stream instead — see internal/clock/purity_test.go
// for why that matters and how it is enforced.
func poll() { time.Sleep(50 * time.Millisecond) }
