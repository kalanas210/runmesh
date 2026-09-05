package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "test-key-0123456789abcdef"

// boot starts the whole binary on an ephemeral port and returns its base URL.
// It is the only test in the suite with no mocks at all: it catches the wiring
// mistakes � a middleware in the wrong order, a dependency never passed, a
// route never registered � that unit tests are structurally unable to see.
func boot(t *testing.T, env map[string]string) (string, func() int) {
	t.Helper()

	base := map[string]string{
		"RUNMESH_HTTP_ADDR":          "127.0.0.1:0",
		"RUNMESH_API_KEYS":           "ci=" + testKey,
		"RUNMESH_WORKERS":            "4",
		"RUNMESH_CLAIM_BATCH":        "4",
		"RUNMESH_POLL_INTERVAL":      "10ms",
		"RUNMESH_ENABLE_TEST_TOOLS":  "true",
		"RUNMESH_LOG_LEVEL":          "error",
		"RUNMESH_BACKOFF_BASE":       "10ms",
		"RUNMESH_BACKOFF_MAX":        "50ms",
		"RUNMESH_SHUTDOWN_GRACE":     "2s",
		"RUNMESH_STORE_TIMEOUT":      "1s",
		"RUNMESH_DRAIN_TIMEOUT":      "3s",
		"RUNMESH_HARD_EXIT_AFTER":    "30s",
		"RUNMESH_RECONCILE_INTERVAL": "50ms",
	}
	for k, v := range env {
		base[k] = v
	}

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	exit := make(chan int, 1)
	// Stderr is captured rather than discarded so a configuration mistake
	// reports what was actually wrong instead of a bare exit code.
	errOut := &syncBuffer{}

	go func() {
		exit <- run(withReadyChannel(ctx, ready),
			nil,
			func(k string) string { return base[k] },
			io.Discard, errOut)
	}()

	var addr string
	select {
	case addr = <-ready:
	case code := <-exit:
		t.Fatalf("server exited before it was ready, code %d:\n%s", code, errOut.String())
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready within 10s")
	}

	// stop is idempotent and memoises the exit code, so a test may call it to
	// assert on the code and the cleanup may call it again without a second
	// receive on a channel that has already been drained.
	var (
		once sync.Once
		code = -1
	)
	stop := func() int {
		once.Do(func() {
			cancel()
			select {
			case code = <-exit:
			case <-time.After(30 * time.Second):
				t.Error("server did not shut down within 30s")
			}
		})
		return code
	}
	t.Cleanup(func() { stop() })
	return "http://" + addr, stop
}

// syncBuffer is an io.Writer safe to read from the test goroutine while the
// server goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func request(t *testing.T, method, url, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

type jobView struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Steps []struct {
		ID        string          `json:"id"`
		State     string          `json:"state"`
		Attempt   int             `json:"attempt"`
		Failures  int             `json:"failures"`
		BlockedBy []string        `json:"blocked_by"`
		Result    json.RawMessage `json:"result"`
		Error     *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"steps"`
}

// waitForJob polls until the job reaches a terminal state. It polls rather
// than sleeps a fixed interval, and it is bounded by the test's own context,
// so a hang fails with a useful message instead of a 10-minute timeout.
func waitForJob(t *testing.T, baseURL, id string) jobView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, body := request(t, http.MethodGet, baseURL+"/api/v1/jobs/"+id, "", nil)
		if status != http.StatusOK {
			t.Fatalf("GET job: status %d body %s", status, body)
		}
		var job jobView
		if err := json.Unmarshal(body, &job); err != nil {
			t.Fatalf("decode job: %v (body %s)", err, body)
		}
		switch job.State {
		case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT":
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached a terminal state; last state %s, body %s", id, job.State, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEndToEndDiamondDAG is the acceptance test for Week 1: submit a plan over
// real HTTP, watch the runtime execute its DAG concurrently, and read the
// result and the timeline back out.
func TestEndToEndDiamondDAG(t *testing.T) {
	baseURL, stop := boot(t, nil)

	plan := `{
	  "name": "csv-report",
	  "steps": [
	    {"id": "fetch",  "tool": "echo",  "params": {"msg": "hello"}},
	    {"id": "a",      "tool": "sleep", "params": {"duration": "20ms"}, "depends_on": ["fetch"]},
	    {"id": "b",      "tool": "sleep", "params": {"duration": "20ms"}, "depends_on": ["fetch"]},
	    {"id": "report", "tool": "echo",  "depends_on": ["a", "b"]}
	  ]
	}`

	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /jobs = %d, want 201. body: %s", status, body)
	}
	var created jobView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	if created.State != "QUEUED" {
		t.Fatalf("new job state = %s, want QUEUED", created.State)
	}
	for _, s := range created.Steps {
		if s.ID == "report" && len(s.BlockedBy) != 2 {
			t.Errorf("report.blocked_by = %v, want both dependencies", s.BlockedBy)
		}
	}

	job := waitForJob(t, baseURL, created.ID)
	if job.State != "SUCCEEDED" {
		t.Fatalf("job state = %s, want SUCCEEDED. steps: %+v", job.State, job.Steps)
	}
	for _, s := range job.Steps {
		if s.State != "SUCCEEDED" {
			t.Errorf("step %s = %s, want SUCCEEDED", s.ID, s.State)
		}
		if s.Attempt != 1 {
			t.Errorf("step %s ran %d times, want 1", s.ID, s.Attempt)
		}
		if len(s.Result) == 0 {
			t.Errorf("step %s stored no result", s.ID)
		}
		if len(s.BlockedBy) != 0 {
			t.Errorf("step %s still reports blocked_by %v after succeeding", s.ID, s.BlockedBy)
		}
	}

	// The timeline must tell the whole story, in order, with a gap-free cursor.
	status, body = request(t, http.MethodGet, baseURL+"/api/v1/jobs/"+created.ID+"/events?limit=1000", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET events = %d: %s", status, body)
	}
	var events struct {
		Events []struct {
			Seq    uint64 `json:"seq"`
			Type   string `json:"type"`
			StepID string `json:"step_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events.Events) == 0 {
		t.Fatal("the timeline is empty")
	}
	if events.Events[0].Type != "JOB_CREATED" {
		t.Errorf("first event = %s, want JOB_CREATED", events.Events[0].Type)
	}
	if last := events.Events[len(events.Events)-1]; last.Type != "JOB_FINISHED" {
		t.Errorf("last event = %s, want JOB_FINISHED", last.Type)
	}
	for i, e := range events.Events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d; the per-job sequence must be gap-free", i, e.Seq)
		}
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
}

// TestEndToEndRetryLadder proves the retry policy end to end: two retryable
// failures, then success on the third attempt, with the budget spent exactly
// twice.
func TestEndToEndRetryLadder(t *testing.T) {
	baseURL, _ := boot(t, nil)

	plan := `{
	  "name": "flaky",
	  "steps": [{"id": "flaky", "tool": "fail", "params": {"fail_times": 2}, "max_attempts": 3}]
	}`
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /jobs = %d: %s", status, body)
	}
	var created jobView
	_ = json.Unmarshal(body, &created)

	job := waitForJob(t, baseURL, created.ID)
	if job.State != "SUCCEEDED" {
		t.Fatalf("job state = %s, want SUCCEEDED after two retries. steps: %+v", job.State, job.Steps)
	}
	s := job.Steps[0]
	if s.Attempt != 3 {
		t.Errorf("attempt = %d, want 3", s.Attempt)
	}
	if s.Failures != 2 {
		t.Errorf("failures = %d, want 2 (the budget is spent by failures, not attempts)", s.Failures)
	}
}

// TestEndToEndTerminalErrorIsNotRetried is the counterpart: a Fatal error must
// stop immediately. A coarser assertion ("the job failed") would pass even if
// the runtime had retried three times first.
func TestEndToEndTerminalErrorIsNotRetried(t *testing.T) {
	baseURL, _ := boot(t, nil)

	plan := `{
	  "name": "doomed",
	  "steps": [
	    {"id": "bad",  "tool": "fail", "params": {"fail_times": 99, "mode": "fatal"}, "max_attempts": 3},
	    {"id": "next", "tool": "echo", "depends_on": ["bad"]}
	  ]
	}`
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /jobs = %d: %s", status, body)
	}
	var created jobView
	_ = json.Unmarshal(body, &created)

	job := waitForJob(t, baseURL, created.ID)
	if job.State != "FAILED" {
		t.Fatalf("job state = %s, want FAILED", job.State)
	}
	for _, s := range job.Steps {
		switch s.ID {
		case "bad":
			if s.Attempt != 1 {
				t.Errorf("a terminal error was retried: attempt = %d, want 1", s.Attempt)
			}
			if s.State != "FAILED" {
				t.Errorf("step bad = %s, want FAILED", s.State)
			}
		case "next":
			if s.State != "CANCELLED" {
				t.Errorf("step next = %s, want CANCELLED under fail_fast", s.State)
			}
		}
	}
}

// TestEndToEndCancel exercises the cancellation path over HTTP, including the
// deliberately honest 202: the job may still be RUNNING when the response is
// written, because in-flight steps stop on their next heartbeat.
func TestEndToEndCancel(t *testing.T) {
	baseURL, _ := boot(t, map[string]string{
		"RUNMESH_HEARTBEAT_INTERVAL": "20ms",
		"RUNMESH_LEASE_TTL":          "1s",
		"RUNMESH_ABANDON_GRACE":      "500ms",
	})

	plan := `{"name": "long", "steps": [{"id": "wait", "tool": "sleep", "params": {"duration": "30s"}, "timeout_seconds": 60}]}`
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /jobs = %d: %s", status, body)
	}
	var created jobView
	_ = json.Unmarshal(body, &created)

	status, body = request(t, http.MethodPost, baseURL+"/api/v1/jobs/"+created.ID+"/cancel", "", nil)
	if status != http.StatusAccepted {
		t.Fatalf("POST cancel = %d, want 202. body: %s", status, body)
	}

	job := waitForJob(t, baseURL, created.ID)
	if job.State != "CANCELLED" {
		t.Fatalf("job state = %s, want CANCELLED", job.State)
	}
	s := job.Steps[0]
	if s.State != "CANCELLED" {
		t.Errorf("step state = %s, want CANCELLED", s.State)
	}
	if s.Failures != 0 {
		t.Errorf("cancellation spent %d units of retry budget; it must spend none", s.Failures)
	}

	// Cancelling a terminal job is a 409, not a silent success.
	status, _ = request(t, http.MethodPost, baseURL+"/api/v1/jobs/"+created.ID+"/cancel", "", nil)
	if status != http.StatusConflict {
		t.Errorf("cancelling a finished job = %d, want 409", status)
	}
}

func TestEndToEndAuthAndProbes(t *testing.T) {
	baseURL, _ := boot(t, nil)

	// The probes are reachable without a credential: a load balancer has none.
	for _, path := range []string{"/api/v1/health", "/api/v1/ready"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s unauthenticated = %d, want 200", path, resp.StatusCode)
		}
	}

	// Everything else requires one, and every rejection is INDISTINGUISHABLE
	// from every other. Asserting that the bodies are byte-identical (once the
	// per-request id is removed) is the real property: a caller probing for
	// valid keys learns nothing about which half of its guess was wrong.
	cases := []struct{ name, header string }{
		{"missing", ""},
		{"wrong scheme", "Basic " + testKey},
		{"wrong key", "Bearer not-the-right-key-at-all"},
		{"empty bearer", "Bearer "},
		{"key of a different length", "Bearer " + testKey + "-extra"},
	}
	var canonical []byte
	for _, tc := range cases {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/v1/jobs", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /jobs (%s): %v", tc.name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /jobs with a %s credential = %d, want 401", tc.name, resp.StatusCode)
		}
		if resp.Header.Get("X-Request-ID") == "" {
			t.Errorf("no X-Request-ID on the 401 for %s", tc.name)
		}
		stripped := requestIDPattern.ReplaceAll(body, []byte(`"request_id":"..."`))
		if canonical == nil {
			canonical = stripped
			continue
		}
		if !bytes.Equal(canonical, stripped) {
			t.Errorf("the 401 body for %s differs from the others, which leaks which part failed:\n got %s\nwant %s",
				tc.name, stripped, canonical)
		}
	}
}

var requestIDPattern = regexp.MustCompile(`"request_id":"[^"]*"`)

func TestEndToEndValidationAndIdempotency(t *testing.T) {
	baseURL, _ := boot(t, nil)

	// A cyclic plan is rejected with the exact field paths that are wrong.
	cyclic := `{"name":"c","steps":[
	  {"id":"a","tool":"echo","depends_on":["b"]},
	  {"id":"b","tool":"echo","depends_on":["a"]}]}`
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/jobs", cyclic, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("cyclic plan = %d, want 400. body: %s", status, body)
	}
	if !bytes.Contains(body, []byte("cycle")) {
		t.Errorf("the 400 body does not name the cycle: %s", body)
	}

	// An unknown field is a typo, not something to ignore silently.
	status, _ = request(t, http.MethodPost, baseURL+"/api/v1/jobs",
		`{"name":"x","stepz":[]}`, nil)
	if status != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", status)
	}

	// An unregistered tool is caught at submit time, not at execution time.
	status, body = request(t, http.MethodPost, baseURL+"/api/v1/jobs",
		`{"name":"x","steps":[{"id":"a","tool":"rm_rf"}]}`, nil)
	if status != http.StatusBadRequest {
		t.Errorf("unknown tool = %d, want 400", status)
	}
	if !bytes.Contains(body, []byte("unknown_tool")) {
		t.Errorf("the 400 body does not say the tool is unknown: %s", body)
	}

	// The same idempotency key returns the same job, not a second one.
	plan := `{"name":"idem","steps":[{"id":"a","tool":"echo"}]}`
	hdr := map[string]string{"Idempotency-Key": "abc-123"}
	status, first := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, hdr)
	if status != http.StatusCreated {
		t.Fatalf("first submit = %d: %s", status, first)
	}
	status, second := request(t, http.MethodPost, baseURL+"/api/v1/jobs", plan, hdr)
	if status != http.StatusOK {
		t.Fatalf("replayed submit = %d, want 200: %s", status, second)
	}
	var a, b jobView
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	if a.ID != b.ID {
		t.Errorf("replay created a second job: %s then %s", a.ID, b.ID)
	}
}

func TestEndToEndToolRegistry(t *testing.T) {
	baseURL, _ := boot(t, nil)

	status, body := request(t, http.MethodGet, baseURL+"/api/v1/tools", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /tools = %d: %s", status, body)
	}
	var out struct {
		Tools []struct {
			Name        string          `json:"name"`
			Version     string          `json:"version"`
			InputSchema json.RawMessage `json:"input_schema"`
			Execution   string          `json:"execution"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("registry has %d tools, want echo, sleep and fail", len(out.Tools))
	}
	for _, tool := range out.Tools {
		if tool.Version == "" || len(tool.InputSchema) == 0 || tool.Execution == "" {
			t.Errorf("tool %s has an incomplete descriptor: %+v", tool.Name, tool)
		}
	}
}

// TestTestToolsAreGated: the failure-injection tool must not exist unless it
// was explicitly enabled. A production deployment should not expose a tool
// whose entire purpose is to panic on demand.
func TestTestToolsAreGated(t *testing.T) {
	baseURL, _ := boot(t, map[string]string{"RUNMESH_ENABLE_TEST_TOOLS": "false"})

	status, body := request(t, http.MethodGet, baseURL+"/api/v1/tools", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /tools = %d", status)
	}
	if bytes.Contains(body, []byte(`"fail"`)) {
		t.Errorf("the fail tool is registered with test tools disabled: %s", body)
	}
	status, _ = request(t, http.MethodPost, baseURL+"/api/v1/jobs",
		`{"name":"x","steps":[{"id":"a","tool":"fail"}]}`, nil)
	if status != http.StatusBadRequest {
		t.Errorf("submitting a fail step with test tools disabled = %d, want 400", status)
	}
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	code := run(t.Context(), []string{"-version"}, func(string) string { return "" }, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if !strings.HasPrefix(out.String(), "runmesh ") {
		t.Fatalf("version output = %q", out.String())
	}
}

// TestMissingAPIKeysRefusesToStart: an unauthenticated server must not be
// something a forgotten environment variable can produce.
func TestMissingAPIKeysRefusesToStart(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, func(string) string { return "" }, io.Discard, &errOut)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "RUNMESH_API_KEYS") {
		t.Fatalf("the error does not name the missing variable: %s", errOut.String())
	}
}
