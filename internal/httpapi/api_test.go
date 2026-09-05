package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

const apiKey = "test-key-0123456789abcdef0123"

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

type fixture struct {
	t       *testing.T
	handler http.Handler
	api     *httpapi.API
	store   *memstore.Store
	clk     *clock.Fake
}

type stubRuntime struct{ inflight, workers int }

func (s stubRuntime) Inflight() int { return s.inflight }
func (s stubRuntime) Workers() int  { return s.workers }

func newFixture(t *testing.T, opts ...func(*httpapi.Deps)) *fixture {
	t.Helper()

	f := &fixture{
		t:     t,
		clk:   clock.NewFake(epoch),
		store: memstore.New(memstore.Options{}),
	}
	t.Cleanup(func() { _ = f.store.Close() })

	deps := httpapi.Deps{
		Store:   f.store,
		Tools:   tools.Builtins(true),
		Runtime: stubRuntime{inflight: 2, workers: 8},
		Clock:   f.clk,
		Log:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		APIKeys: map[[32]byte]string{config.KeyDigest(apiKey): "ci"},
		Limits: runmesh.Limits{
			MaxSteps: 10, MaxDependsOn: 4, MaxParamsBytes: 1024,
			MaxStepTimeout: time.Minute, MaxAttempts: 5, MaxResultBytes: 4096,
		},
		Defaults:        runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3},
		MaxRequestBytes: 4096,
		MaxQueueDepth:   100,
		Durable:         false,
		StoreName:       "memory",
	}
	for _, o := range opts {
		o(&deps)
	}

	handler, api, err := httpapi.New(deps)
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	f.handler, f.api = handler, api
	return f
}

func (f *fixture) do(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(f.t.Context(), method, target, rdr)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// envelope is the one shape every non-2xx response has to have.
type envelope struct {
	Error struct {
		Code      string           `json:"code"`
		Message   string           `json:"message"`
		Details   []runmesh.Detail `json:"details"`
		RequestID string           `json:"request_id"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("the error body is not an envelope: %v (body %s)", err, rec.Body)
	}
	if e.Error.Code == "" {
		t.Errorf("the envelope has no code: %s", rec.Body)
	}
	if e.Error.Message == "" {
		t.Errorf("the envelope has no message: %s", rec.Body)
	}
	if e.Error.RequestID == "" {
		t.Errorf("the envelope has no request id: %s", rec.Body)
	}
	return e
}

const validPlan = `{"name":"p","steps":[{"id":"a","tool":"echo","params":{"x":1}}]}`

// --------------------------------------------------------------------- auth

func TestAuth(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"valid key", "Bearer " + apiKey, http.StatusOK},
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + apiKey, http.StatusUnauthorized},
		{"bearer with no token", "Bearer ", http.StatusUnauthorized},
		{"wrong key", "Bearer " + strings.Repeat("z", len(apiKey)), http.StatusUnauthorized},
		{"key of a different length", "Bearer " + apiKey + "extra", http.StatusUnauthorized},
		{"lowercase scheme", "bearer " + apiKey, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodGet, "/api/v1/jobs", "", map[string]string{"Authorization": tc.header})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusUnauthorized {
				e := decodeEnvelope(t, rec)
				if e.Error.Code != "unauthenticated" {
					t.Errorf("code = %q, want unauthenticated", e.Error.Code)
				}
			}
		})
	}
}

func TestProbesAreUnauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for _, path := range []string{"/api/v1/health", "/api/v1/ready"} {
		rec := f.do(http.MethodGet, path, "", map[string]string{"Authorization": ""})
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s without a credential = %d, want 200", path, rec.Code)
		}
	}
}

func TestReadyReportsHonestState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodGet, "/api/v1/ready", "", nil)
	var ready struct {
		Status   string `json:"status"`
		Store    string `json:"store"`
		Durable  bool   `json:"durable"`
		Workers  int    `json:"workers"`
		Inflight int    `json:"inflight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	// The honest bit: Week 1 answers 201 to a submission as though the job were
	// safe, and it is not. A status endpoint that overstated durability would
	// be worse than none.
	if ready.Durable {
		t.Error("readiness claims the in-memory store is durable")
	}
	if ready.Store != "memory" || ready.Workers != 8 || ready.Inflight != 2 {
		t.Errorf("readiness body = %+v", ready)
	}

	// Draining fails readiness before the listener closes, so a load balancer
	// has a window to take this instance out of rotation.
	f.api.Draining()
	rec = f.do(http.MethodGet, "/api/v1/ready", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness while draining = %d, want 503", rec.Code)
	}

	// Liveness stays up: a draining process is still alive, and killing it
	// mid-drain is exactly wrong.
	if rec := f.do(http.MethodGet, "/api/v1/health", "", nil); rec.Code != http.StatusOK {
		t.Errorf("liveness while draining = %d, want 200", rec.Code)
	}
}

// ------------------------------------------------------------------- submit

func TestCreateJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	var job struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Steps []struct {
			ID         string   `json:"id"`
			BlockedBy  []string `json:"blocked_by"`
			DependsOn  []string `json:"depends_on"`
			TimeoutSec float64  `json:"timeout_seconds"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.State != "QUEUED" || !strings.HasPrefix(job.ID, "job_") {
		t.Errorf("job = %+v", job)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/jobs/"+job.ID {
		t.Errorf("Location = %q", got)
	}
	if job.Steps[0].TimeoutSec != 30 {
		t.Errorf("timeout_seconds = %v, want the configured default of 30", job.Steps[0].TimeoutSec)
	}
	// Empty lists must serialise as [] rather than null: a client iterating
	// them should not have to nil-check.
	if !strings.Contains(rec.Body.String(), `"depends_on":[]`) {
		t.Errorf("empty depends_on did not serialise as an empty array: %s", rec.Body)
	}
	// Internal fields must never reach the wire.
	for _, leaked := range []string{"lease", "idempotency", "event_seq", "LeaseID"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(leaked)) {
			t.Errorf("the response leaks %q: %s", leaked, rec.Body)
		}
	}
}

func TestCreateJobValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantField  string
		wantIssue  string
	}{
		{
			name: "cyclic plan", wantStatus: http.StatusBadRequest,
			body:      `{"name":"c","steps":[{"id":"a","tool":"echo","depends_on":["b"]},{"id":"b","tool":"echo","depends_on":["a"]}]}`,
			wantField: "steps", wantIssue: "cycle:a,b",
		},
		{
			name: "dangling dependency", wantStatus: http.StatusBadRequest,
			body:      `{"name":"c","steps":[{"id":"a","tool":"echo","depends_on":["ghost"]}]}`,
			wantField: "steps[0].depends_on[0]", wantIssue: "unknown_step",
		},
		{
			name: "unknown tool", wantStatus: http.StatusBadRequest,
			body:      `{"name":"c","steps":[{"id":"a","tool":"rm_rf"}]}`,
			wantField: "steps[0].tool", wantIssue: "unknown_tool",
		},
		{
			name: "no steps", wantStatus: http.StatusBadRequest,
			body:      `{"name":"c","steps":[]}`,
			wantField: "steps", wantIssue: "required",
		},
		{
			// DisallowUnknownFields: a typo is a 400, not a silently ignored
			// field. That matters most when the author is an LLM in a retry loop.
			name: "unknown field", wantStatus: http.StatusBadRequest,
			body: `{"name":"c","stepz":[]}`,
		},
		{name: "malformed json", body: `{"name":`, wantStatus: http.StatusBadRequest},
		{name: "empty body", body: "", wantStatus: http.StatusBadRequest},
		{name: "two json values", body: `{"name":"a","steps":[]} {"name":"b"}`, wantStatus: http.StatusBadRequest},
		{
			// Per-tool validation runs at submit time, so a malformed parameter
			// is a 400 now rather than a step that fails three times later.
			name: "tool rejects its own params", wantStatus: http.StatusBadRequest,
			body:      `{"name":"c","steps":[{"id":"a","tool":"sleep","params":{"duration":"forever"}}]}`,
			wantField: "steps[0].params",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/v1/jobs", tc.body, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			e := decodeEnvelope(t, rec)
			if e.Error.Code != "invalid_argument" {
				t.Errorf("code = %q, want invalid_argument", e.Error.Code)
			}
			if tc.wantField == "" {
				return
			}
			found := false
			for _, d := range e.Error.Details {
				if d.Field == tc.wantField && (tc.wantIssue == "" || d.Issue == tc.wantIssue) {
					found = true
				}
			}
			if !found {
				t.Errorf("no detail for %q/%q; got %+v", tc.wantField, tc.wantIssue, e.Error.Details)
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	huge := `{"name":"` + strings.Repeat("x", 8000) + `","steps":[]}`
	rec := f.do(http.MethodPost, "/api/v1/jobs", huge, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body)
	}
	decodeEnvelope(t, rec)
}

func TestIdempotencyReplay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hdr := map[string]string{"Idempotency-Key": "abc-123"}

	first := f.do(http.MethodPost, "/api/v1/jobs", validPlan, hdr)
	if first.Code != http.StatusCreated {
		t.Fatalf("first submit = %d", first.Code)
	}
	second := f.do(http.MethodPost, "/api/v1/jobs", validPlan, hdr)
	if second.Code != http.StatusOK {
		t.Fatalf("replay = %d, want 200 (body %s)", second.Code, second.Body)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("the replay is not marked as one")
	}

	var a, b struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Errorf("the replay created a second job: %s then %s", a.ID, b.ID)
	}

	// An over-long key is rejected rather than silently truncated.
	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan,
		map[string]string{"Idempotency-Key": strings.Repeat("k", 200)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an over-long idempotency key = %d, want 400", rec.Code)
	}
}

// TestAdmissionControl: a runtime that accepts unbounded work falls over
// politely instead of pushing back.
func TestAdmissionControl(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *httpapi.Deps) { d.MaxQueueDepth = 2 })

	if rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil); rec.Code != http.StatusCreated {
		t.Fatalf("first submit = %d", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil); rec.Code != http.StatusCreated {
		t.Fatalf("second submit = %d", rec.Code)
	}

	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the third submit = %d, want 429 (body %s)", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
	e := decodeEnvelope(t, rec)
	if e.Error.Code != "resource_exhausted" {
		t.Errorf("code = %q, want resource_exhausted", e.Error.Code)
	}

	// Nothing was written: a rejected submission must leave no trace.
	list := f.do(http.MethodGet, "/api/v1/jobs", "", nil)
	var page struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &page)
	if len(page.Jobs) != 2 {
		t.Errorf("%d jobs stored, want the 2 that were accepted", len(page.Jobs))
	}
}

// -------------------------------------------------------------------- reads

func TestGetJobAndNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil)
	var created struct {
		ID      string `json:"id"`
		Version uint64 `json:"version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	got := f.do(http.MethodGet, "/api/v1/jobs/"+created.ID, "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET job = %d", got.Code)
	}
	if got.Header().Get("ETag") == "" {
		t.Error("no ETag; the dashboard polls this endpoint and the version is what makes that cheap")
	}

	missing := f.do(http.MethodGet, "/api/v1/jobs/job_missing", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown job = %d, want 404", missing.Code)
	}
	if e := decodeEnvelope(t, missing); e.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", e.Error.Code)
	}
}

func TestListJobsFiltersAndPaging(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for range 5 {
		f.clk.Advance(time.Millisecond)
		if rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil); rec.Code != http.StatusCreated {
			t.Fatalf("submit = %d", rec.Code)
		}
	}

	first := f.do(http.MethodGet, "/api/v1/jobs?limit=2", "", nil)
	var page struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Jobs) != 2 || page.NextCursor == "" {
		t.Fatalf("first page has %d jobs and cursor %q", len(page.Jobs), page.NextCursor)
	}

	second := f.do(http.MethodGet, "/api/v1/jobs?limit=2&cursor="+page.NextCursor, "", nil)
	var page2 struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &page2)
	for _, a := range page.Jobs {
		for _, b := range page2.Jobs {
			if a.ID == b.ID {
				t.Fatalf("job %s appears on both pages", a.ID)
			}
		}
	}

	// A state filter that matches nothing returns an empty array, not null.
	empty := f.do(http.MethodGet, "/api/v1/jobs?state=SUCCEEDED", "", nil)
	if !strings.Contains(empty.Body.String(), `"jobs":[]`) {
		t.Errorf("an empty result serialised as %s", empty.Body)
	}

	for _, bad := range []string{"?state=NONSENSE", "?limit=0", "?limit=9999", "?limit=abc"} {
		if rec := f.do(http.MethodGet, "/api/v1/jobs"+bad, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /jobs%s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestEventsCursor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	events := f.do(http.MethodGet, "/api/v1/jobs/"+created.ID+"/events", "", nil)
	if events.Code != http.StatusOK {
		t.Fatalf("GET events = %d", events.Code)
	}
	var page struct {
		Events []struct {
			Seq  uint64 `json:"seq"`
			Type string `json:"type"`
		} `json:"events"`
		NextAfter uint64 `json:"next_after"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(events.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Type != "JOB_CREATED" {
		t.Fatalf("events = %+v", page.Events)
	}
	if page.NextAfter != 1 {
		t.Errorf("next_after = %d, want 1", page.NextAfter)
	}

	// Resuming from the cursor yields nothing new.
	more := f.do(http.MethodGet, "/api/v1/jobs/"+created.ID+"/events?after=1", "", nil)
	if !strings.Contains(more.Body.String(), `"events":[]`) {
		t.Errorf("resuming from the cursor returned %s", more.Body)
	}

	for _, bad := range []string{"?after=-1", "?after=abc", "?limit=0", "?limit=5000"} {
		if rec := f.do(http.MethodGet, "/api/v1/jobs/"+created.ID+"/events"+bad, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET events%s = %d, want 400", bad, rec.Code)
		}
	}
	if rec := f.do(http.MethodGet, "/api/v1/jobs/job_missing/events", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("events for an unknown job = %d, want 404", rec.Code)
	}
}

// TestCancelIsAcceptedNotCompleted: cancellation is a REQUEST. The body says
// what is actually true, which is the only answer that stays true once the
// cancel can land on a different replica from the step.
func TestCancelStates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/jobs", validPlan, nil)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	cancel := f.do(http.MethodPost, "/api/v1/jobs/"+created.ID+"/cancel", "", nil)
	if cancel.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d, want 202 (body %s)", cancel.Code, cancel.Body)
	}
	var job struct {
		State             string `json:"state"`
		CancelRequestedAt string `json:"cancel_requested_at"`
		CancelReason      string `json:"cancel_reason"`
	}
	_ = json.Unmarshal(cancel.Body.Bytes(), &job)
	if job.CancelRequestedAt == "" || job.CancelReason != "user" {
		t.Errorf("cancel response = %+v", job)
	}

	// Cancelling a terminal job is a conflict, not a silent success.
	again := f.do(http.MethodPost, "/api/v1/jobs/"+created.ID+"/cancel", "", nil)
	if again.Code != http.StatusConflict {
		t.Fatalf("cancelling a terminal job = %d, want 409", again.Code)
	}
	if e := decodeEnvelope(t, again); e.Error.Code != "failed_precondition" {
		t.Errorf("code = %q, want failed_precondition", e.Error.Code)
	}

	if rec := f.do(http.MethodPost, "/api/v1/jobs/job_missing/cancel", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cancelling an unknown job = %d, want 404", rec.Code)
	}
}

func TestListTools(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(http.MethodGet, "/api/v1/tools", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tools = %d", rec.Code)
	}
	var out struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("%d tools, want echo, fail and sleep", len(out.Tools))
	}
	for _, tool := range out.Tools {
		// The schema is served verbatim because it becomes the Gemini function
		// declaration in Week 5.
		if !json.Valid(tool.InputSchema) {
			t.Errorf("tool %s has an invalid input schema: %s", tool.Name, tool.InputSchema)
		}
	}
}

// ------------------------------------------------------------- infrastructure

// TestPanicBecomesAWellFormed500 also proves the middleware nesting order:
// Logger wraps Recover, so a panic is turned into a 500 and only then observed.
func TestPanicBecomesAWellFormed500(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *httpapi.Deps) { d.Store = panickingStore{} })

	rec := f.do(http.MethodGet, "/api/v1/jobs/anything", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	e := decodeEnvelope(t, rec)
	if e.Error.Code != "internal" {
		t.Errorf("code = %q, want internal", e.Error.Code)
	}
	// The cause is logged, never serialised.
	if strings.Contains(rec.Body.String(), "goroutine") || strings.Contains(rec.Body.String(), "panic") {
		t.Errorf("the 500 body leaks internals: %s", rec.Body)
	}
}

type panickingStore struct{ httpapi.Store }

func (panickingStore) Job(ctx context.Context, id string) (*runmesh.Job, error) {
	panic("boom")
}

func TestRequestIDEchoAndSanitisation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// A well-formed id is echoed so a caller can correlate its own logs.
	rec := f.do(http.MethodGet, "/api/v1/jobs", "", map[string]string{"X-Request-ID": "client-abc-123"})
	if got := rec.Header().Get("X-Request-ID"); got != "client-abc-123" {
		t.Errorf("X-Request-ID = %q, want the client's value echoed", got)
	}

	// Hostile values are replaced, not propagated: this string ends up in log
	// lines and in a JSON body.
	for name, hostile := range map[string]string{
		"control characters": "abc\ndef",
		"absurdly long":      strings.Repeat("x", 5000),
		"non-ascii":          "\x00\x01\x02",
	} {
		rec := f.do(http.MethodGet, "/api/v1/jobs", "", map[string]string{"X-Request-ID": hostile})
		got := rec.Header().Get("X-Request-ID")
		if got == hostile {
			t.Errorf("%s was propagated verbatim", name)
		}
		if !strings.HasPrefix(got, "req_") {
			t.Errorf("%s produced %q, want a generated id", name, got)
		}
	}

	// With no header at all, one is generated.
	rec = f.do(http.MethodGet, "/api/v1/jobs", "", nil)
	if !strings.HasPrefix(rec.Header().Get("X-Request-ID"), "req_") {
		t.Error("no request id was generated")
	}
}

// TestEveryErrorUsesTheEnvelope is the generic sweep: a new endpoint that
// forgets the envelope fails here rather than surprising a client.
func TestEveryErrorUsesTheEnvelope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	cases := []struct {
		method, target, body string
		headers              map[string]string
		wantStatus           int
	}{
		{http.MethodGet, "/api/v1/jobs", "", map[string]string{"Authorization": ""}, http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/jobs/job_missing", "", nil, http.StatusNotFound},
		{http.MethodPost, "/api/v1/jobs", `{"bad":true}`, nil, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/jobs/job_missing/cancel", "", nil, http.StatusNotFound},
		{http.MethodGet, "/api/v1/jobs?limit=0", "", nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := f.do(tc.method, tc.target, tc.body, tc.headers)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, tc.wantStatus)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s Content-Type = %q", tc.method, tc.target, ct)
		}
		decodeEnvelope(t, rec)
	}
}

func TestMethodAndRouteMismatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// ServeMux answers 405 for a known path with the wrong method, and 404 for
	// an unknown path. Both are handled without a handler of our own.
	if rec := f.do(http.MethodDelete, "/api/v1/jobs", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /jobs = %d, want 405", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/api/v1/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET an unknown path = %d, want 404", rec.Code)
	}
	// The retry endpoint is deliberately absent in Week 1.
	if rec := f.do(http.MethodPost, "/api/v1/jobs/job_1/retry", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /jobs/{id}/retry = %d, want 404: it is deliberately not implemented yet", rec.Code)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	if _, _, err := httpapi.New(httpapi.Deps{}); err == nil {
		t.Error("httpapi.New accepted a nil store")
	}
	store := memstore.New(memstore.Options{})
	t.Cleanup(func() { _ = store.Close() })
	if _, _, err := httpapi.New(httpapi.Deps{Store: store}); err == nil {
		t.Error("httpapi.New accepted an empty key set; that would start an unauthenticated server")
	}
	if _, _, err := httpapi.New(httpapi.Deps{
		Store:   store,
		APIKeys: map[[32]byte]string{config.KeyDigest(apiKey): "ci"},
	}); err != nil {
		t.Errorf("httpapi.New rejected a minimal valid configuration: %v", err)
	}
}

// TestLogLineCarriesRouteAndKeyID pins the fix for a bug the running server
// made obvious: every authenticated request logged route=unmatched and an
// empty api_key_id.
//
// The cause was structural. Auth added the key id with a new context, which
// means a NEW *http.Request downstream; the outermost Logger kept the old one
// and could see neither the key id nor the route pattern the mux later set.
// Both are the whole point of the log line — the route is Week 3's metric
// dimension and the key id is the audit trail — so a test holds them here.
func TestLogLineCarriesRouteAndKeyID(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	if rec := f.do(http.MethodGet, "/api/v1/jobs/job_missing", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}

	line := buf.String()
	if !strings.Contains(line, `route="GET /api/v1/jobs/{id}"`) {
		t.Errorf("the log line does not carry the matched route:\n%s", line)
	}
	if !strings.Contains(line, "api_key_id=ci") {
		t.Errorf("the log line does not carry the API key id:\n%s", line)
	}
	// The key itself must never appear anywhere in a log line.
	if strings.Contains(line, apiKey) {
		t.Errorf("the log line contains the API key itself:\n%s", line)
	}
}

// TestUnmatchedRouteStaysLowCardinality: an unknown path must not put an
// attacker-controlled string into what will become a metric label.
func TestUnmatchedRouteStaysLowCardinality(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	f.do(http.MethodGet, "/api/v1/attacker-controlled-9f2b", "", nil)
	line := buf.String()
	if !strings.Contains(line, "route=unmatched") {
		t.Errorf("an unmatched path did not produce route=unmatched:\n%s", line)
	}
	if strings.Contains(line, "route=/api/v1/attacker") {
		t.Errorf("the raw path leaked into the route label:\n%s", line)
	}
}

// syncBuffer is an io.Writer the test can read while the handler writes.
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
