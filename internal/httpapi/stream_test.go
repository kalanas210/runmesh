package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// The streaming tests run against a REAL httptest.NewServer, not the recorder
// fixture the rest of this package uses.
//
// That is not a stylistic preference. (*fixture).do drives the handler
// synchronously into an httptest.ResponseRecorder, which buffers everything and
// returns when the handler returns — so it cannot distinguish a stream that
// flushed each frame as it was produced from one that held every byte until the
// connection closed, which is precisely the bug the statusRecorder Unwrap
// exists to prevent. A real server over a real socket can, and does, below.
//
// Nothing here sleeps. Two things synchronise these tests instead: a blocking
// READ, which returns exactly when the server has written a complete frame, and
// clock.Fake.BlockUntilContext, which returns exactly when the handler has
// registered its tickers — so a subsequent Advance cannot fire a timer the
// handler is not yet waiting on.

// ------------------------------------------------------------------ fixture

// fakeBus is the live accelerator, driven by hand. The real one
// (internal/eventbus) is tested in its own package; what matters HERE is what
// the handler does with what it receives, including the events it never
// receives because the bus dropped them.
type fakeBus struct {
	mu   sync.Mutex
	subs map[string][]*busSub
}

type busSub struct {
	ch   chan runmesh.Event
	once sync.Once
}

func (s *busSub) close() { s.once.Do(func() { close(s.ch) }) }

func newFakeBus() *fakeBus { return &fakeBus{subs: make(map[string][]*busSub)} }

func (b *fakeBus) Subscribe(jobID string, buf int) (<-chan runmesh.Event, func()) {
	s := &busSub{ch: make(chan runmesh.Event, buf)}
	b.mu.Lock()
	b.subs[jobID] = append(b.subs[jobID], s)
	b.mu.Unlock()
	return s.ch, func() {
		b.mu.Lock()
		kept := b.subs[jobID][:0]
		for _, other := range b.subs[jobID] {
			if other != s {
				kept = append(kept, other)
			}
		}
		b.subs[jobID] = kept
		b.mu.Unlock()
		s.close()
	}
}

// send hands one event to every subscriber of that job. The send BLOCKS, which
// makes it a synchronisation point rather than a hope: when it returns, the
// handler's buffer holds the event.
func (b *fakeBus) send(e runmesh.Event) {
	b.mu.Lock()
	targets := append([]*busSub(nil), b.subs[e.JobID]...)
	b.mu.Unlock()
	for _, s := range targets {
		s.ch <- e.Clone()
	}
}

// closeAll is the store shutting down underneath every subscription.
func (b *fakeBus) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for jobID, set := range b.subs {
		for _, s := range set {
			s.close()
		}
		delete(b.subs, jobID)
	}
}

type streamFixture struct {
	t     *testing.T
	srv   *httptest.Server
	api   *httpapi.API
	store *memstore.Store
	clk   *clock.Fake
	bus   *fakeBus
}

// newStreamFixture builds the same API the rest of the suite builds, behind the
// same six middleware, and puts a real listener in front of it.
//
// Both tickers default to an hour out. A test that cares about one of them
// moves it in, and the other then cannot fire and interleave a frame nobody
// asked for — which is what makes every assertion below about frame ORDER
// meaningful rather than lucky.
func newStreamFixture(t *testing.T, opts ...func(*httpapi.Deps)) *streamFixture {
	t.Helper()

	sf := &streamFixture{t: t, bus: newFakeBus()}

	all := make([]func(*httpapi.Deps), 0, len(opts)+2)
	all = append(all, func(d *httpapi.Deps) {
		d.Stream = sf.bus
		d.StreamHeartbeat = time.Hour
		d.StreamPollInterval = time.Hour
	})
	all = append(all, opts...)
	all = append(all, func(d *httpapi.Deps) {
		// A test may have wrapped the store to inject a fault. Seeding still
		// has to reach the real one underneath, or the fault would fire on the
		// fixture's own writes instead of on the handler's reads.
		concrete := d.Store
		if w, ok := concrete.(unwrapStore); ok {
			concrete = w.unwrap()
		}
		store, ok := concrete.(*memstore.Store)
		if !ok {
			t.Fatalf("the stream fixture needs a *memstore.Store, got %T", d.Store)
		}
		sf.store = store
	})

	f := newFixture(t, all...)
	sf.api, sf.clk = f.api, f.clk
	sf.srv = httptest.NewServer(f.handler)
	t.Cleanup(sf.srv.Close)
	return sf
}

// withTinyEventRing replaces the store with one whose per-job event ring evicts
// almost immediately, which is the only way to make Truncated true.
func withTinyEventRing(t *testing.T, n int) func(*httpapi.Deps) {
	return func(d *httpapi.Deps) {
		s := memstore.New(memstore.Options{JobEventBuffer: n})
		t.Cleanup(func() { _ = s.Close() })
		d.Store = s
	}
}

// ------------------------------------------------------------ driving events
//
// These seed the timeline the way the engine does — Claim, Start, Finish —
// rather than writing events directly, so the seq numbers, the types and the
// ordering a test asserts on are the ones a dashboard actually receives.

func (f *streamFixture) createJob(id string, stepIDs ...string) *runmesh.Job {
	f.t.Helper()
	plan := &runmesh.Plan{Name: id}
	for _, s := range stepIDs {
		plan.Steps = append(plan.Steps, runmesh.PlanStep{ID: s, Tool: "echo"})
	}
	j := plan.Build(id, epoch, runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3})
	out, err := f.store.CreateJob(f.t.Context(), j, "")
	if err != nil {
		f.t.Fatalf("CreateJob(%s): %v", id, err)
	}
	return out
}

// runStep drives exactly one claimable step to SUCCEEDED, which appends
// STEP_SCHEDULED, STEP_STARTED, STEP_FINISHED and — on the last step — the
// JOB_FINISHED the stream ends on.
func (f *streamFixture) runStep(jobID string) {
	f.t.Helper()
	leases, err := f.store.Claim(f.t.Context(), runmesh.ClaimRequest{
		Owner: "stream-test", Limit: 1, LeaseTTL: time.Minute, Now: epoch,
	})
	if err != nil {
		f.t.Fatalf("Claim: %v", err)
	}
	if len(leases) != 1 {
		f.t.Fatalf("Claim returned %d leases, want 1: nothing was claimable", len(leases))
	}
	l := leases[0]
	if l.JobID != jobID {
		f.t.Fatalf("claimed a step of %s, want %s", l.JobID, jobID)
	}
	if err := f.store.Start(f.t.Context(), l, epoch); err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	err = f.store.Finish(f.t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Succeeded,
		Result:    json.RawMessage(`{"ok":true}`),
		StartedAt: epoch, EndedAt: epoch.Add(time.Second),
	})
	if err != nil {
		f.t.Fatalf("Finish: %v", err)
	}
}

// lastSeq is the highest seq on a job's durable timeline.
func (f *streamFixture) lastSeq(jobID string) uint64 {
	f.t.Helper()
	page, err := f.store.JobEvents(f.t.Context(), jobID, 0, 1000)
	if err != nil {
		f.t.Fatalf("JobEvents: %v", err)
	}
	return page.NextAfter
}

// eventAt returns one event off the durable timeline by its seq, so a test can
// hand the bus exactly the event a real broker would have delivered.
func (f *streamFixture) eventAt(jobID string, seq uint64) runmesh.Event {
	f.t.Helper()
	page, err := f.store.JobEvents(f.t.Context(), jobID, seq-1, 1)
	if err != nil {
		f.t.Fatalf("JobEvents: %v", err)
	}
	if len(page.Events) != 1 {
		f.t.Fatalf("no event at seq %d on %s", seq, jobID)
	}
	return page.Events[0]
}

// --------------------------------------------------------------- the client

func (f *streamFixture) request(target string, headers map[string]string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.srv.URL+target, nil)
	if err != nil {
		f.t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatalf("GET %s: %v", target, err)
	}
	// Closing the body is what cancels the handler's request context, and
	// httptest.Server.Close waits for every ACTIVE connection — so a stream left
	// open would hang the suite rather than fail it. Registering the close here
	// means no test can forget.
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// open starts a stream. It returns once the response HEADERS have arrived,
// which for a handler that has not returned is already a statement about
// flushing.
func (f *streamFixture) open(jobID, query string, headers map[string]string) (*http.Response, *bufio.Reader) {
	f.t.Helper()
	resp := f.request("/api/v1/jobs/"+jobID+"/stream"+query, headers)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("opening the stream = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	return resp, bufio.NewReader(resp.Body)
}

// parked returns once the handler has registered both of its tickers on the
// fake clock, which only a handler inside its select loop can have done. It is
// the guard that makes a following Advance deterministic — and, incidentally,
// proof that the handler did not return after writing its first frame.
func (f *streamFixture) parked() {
	f.t.Helper()
	if err := f.clk.BlockUntilContext(f.t.Context(), 2); err != nil {
		f.t.Fatalf("the stream never reached its select loop: %v", err)
	}
}

// frame is one id/event/data triple, or a bare comment.
type frame struct {
	id      string
	event   string
	data    string
	comment string
}

// readFrame blocks until a complete blank-line-terminated frame arrives.
//
// The default arm is an assertion in its own right: any line that is not an SSE
// field fails the test, which is what catches a JSON envelope spliced into the
// middle of an event-stream body.
func readFrame(t *testing.T, br *bufio.Reader) frame {
	t.Helper()
	var fr frame
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading a frame: %v (partial frame %+v)", err, fr)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			return fr
		case strings.HasPrefix(line, ": "):
			fr.comment = line[2:]
		case strings.HasPrefix(line, "id: "):
			fr.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			fr.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			fr.data = line[6:]
		default:
			t.Fatalf("a line in the stream body is not an SSE field: %q", line)
		}
	}
}

func expectFrame(t *testing.T, br *bufio.Reader, event string) frame {
	t.Helper()
	fr := readFrame(t, br)
	if fr.event != event {
		t.Fatalf("frame is %q (data %s), want %q", fr.event, fr.data, event)
	}
	return fr
}

func decodeFrame(t *testing.T, fr frame, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(fr.data), into); err != nil {
		t.Fatalf("the %q frame's data is not JSON: %v (%s)", fr.event, err, fr.data)
	}
}

func expectEOF(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if _, err := br.ReadString('\n'); err != io.EOF {
		t.Fatalf("the body did not end: err = %v", err)
	}
}

type snapshotFrame struct {
	Job struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"job"`
	Events    []runmesh.Event `json:"events"`
	NextAfter uint64          `json:"next_after"`
	Truncated bool            `json:"truncated"`
	OldestSeq uint64          `json:"oldest_seq"`
}

func readSnapshot(t *testing.T, br *bufio.Reader) snapshotFrame {
	t.Helper()
	fr := expectFrame(t, br, "snapshot")
	var snap snapshotFrame
	decodeFrame(t, fr, &snap)
	if fr.id != strconv.FormatUint(snap.NextAfter, 10) {
		t.Errorf("the snapshot's id is %q but its next_after is %d; the SSE id and "+
			"the resume cursor have to be the same number", fr.id, snap.NextAfter)
	}
	return snap
}

// ------------------------------------------------------------------- 1. flush

// TestStreamFlushesBeforeTheHandlerReturns is the test that makes blocker #1
// impossible to regress.
//
// Logger installs a *statusRecorder around every response, and before that type
// grew Unwrap it satisfied neither http.Flusher nor http.Hijacker and could not
// be unwrapped by http.NewResponseController — so a stream buffered every frame
// until the handler returned, which for a stream is for ever. Remove that one
// method and this test stops: the handler's first write fails, it returns
// before registering a ticker, and parked() never sees two waiters.
func TestStreamFlushesBeforeTheHandlerReturns(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t, func(d *httpapi.Deps) { d.StreamHeartbeat = time.Second })
	job := f.createJob("job_flush", "a")

	resp, br := f.open(job.ID, "", nil)

	for _, h := range []struct{ name, want string }{
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Cache-Control", "no-store"},
		// nginx buffers text/event-stream by default and would otherwise hold
		// every frame — the same bug as the missing Unwrap, one layer out.
		{"X-Accel-Buffering", "no"},
	} {
		if got := resp.Header.Get(h.name); got != h.want {
			t.Errorf("%s = %q, want %q", h.name, got, h.want)
		}
	}
	// RequestID runs before the handler, so the id is on the SSE response for
	// free and a client can quote it when a stream misbehaves.
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("the stream response carries no X-Request-ID")
	}

	snap := readSnapshot(t, br)
	if snap.Job.ID != job.ID {
		t.Errorf("the snapshot carries job %q, want %q", snap.Job.ID, job.ID)
	}
	if len(snap.Events) == 0 {
		t.Error("the snapshot carries no events; JOB_CREATED is already on the timeline")
	}

	// The frame above arrived while the handler was still running. These two
	// assertions are what say so.
	f.parked()
	if n := f.api.ActiveStreams(); n != 1 {
		t.Errorf("ActiveStreams() = %d while a stream is open, want 1", n)
	}

	// And a frame produced AFTER the snapshot arrives on its own, which is only
	// possible if the connection is genuinely incremental.
	f.clk.Advance(time.Second)
	ping := readFrame(t, br)
	if ping.comment != "ping" {
		t.Errorf("after a heartbeat interval the next frame is %+v, want a `: ping` comment", ping)
	}
	if ping.id != "" || ping.event != "" {
		t.Errorf("the heartbeat carried id %q and event %q; a ping is a bare comment "+
			"precisely so it can never move a client's cursor", ping.id, ping.event)
	}
}

// ------------------------------------------------- 1b. the write deadline

// deafPeer is a ResponseWriter modelling the one failure a bounded write
// deadline exists for: a TCP peer that is dead but not closed, whose socket
// buffer is full and will never drain.
//
// It is a hand-written writer rather than a real socket because a real socket
// cannot be made to reproduce this on demand: filling a peer's receive window
// means pushing hundreds of kilobytes through a connection whose reader has to
// be stopped at exactly the right moment, and the test would then be a race
// against buffer sizes the operating system chooses.
//
// The division of labour is the whole point. Write NEVER blocks, because in
// net/http it does not reach the socket at all: every frame this handler emits
// — event, ping, end, bye, resync, error — is far smaller than the 2048-byte
// bufferBeforeChunkingSize, so a Write only fills a bufio.Writer. FlushError is
// the syscall, and against this peer it can never succeed, so it has exactly
// two possible outcomes and the armed deadline decides which:
//
//   - A deadline IS armed. The kernel gives up and reports
//     os.ErrDeadlineExceeded, the frame fails, and the handler returns. This is
//     the behaviour the design claims.
//   - No deadline is armed. The real syscall would block for ever and this
//     handler goroutine would never return, taking a stream slot with it until
//     the process restarted. A fake cannot reproduce "for ever" without hanging
//     the suite, so it names the violation on a channel the test selects on and
//     returns nil — an unbounded handler becomes a legible failure instead of a
//     ten-minute timeout.
type deafPeer struct {
	hdr http.Header

	mu       sync.Mutex
	status   int
	deadline time.Time
	body     strings.Builder

	// unbounded is closed on the first flush that ran with no deadline armed.
	unbounded chan struct{}
	once      sync.Once
}

func newDeafPeer() *deafPeer {
	return &deafPeer{hdr: make(http.Header), unbounded: make(chan struct{})}
}

func (p *deafPeer) Header() http.Header { return p.hdr }

func (p *deafPeer) WriteHeader(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == 0 {
		p.status = code
	}
}

func (p *deafPeer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.body.Write(b)
}

func (p *deafPeer) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deadline = t
	return nil
}

// FlushError, not Flush: http.ResponseController prefers it, and a plain
// Flush() has nowhere to report the deadline the kernel just enforced — which
// is how an unbounded flush would look bounded to a caller.
func (p *deafPeer) FlushError() error {
	if p.armed() {
		return os.ErrDeadlineExceeded
	}
	p.once.Do(func() { close(p.unbounded) })
	return nil
}

func (p *deafPeer) armed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.deadline.IsZero()
}

func (p *deafPeer) snapshotOf() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, p.body.String()
}

// TestStreamWriteDeadlineSpansTheFlush is the regression test for a deadline
// that was armed around the one call that cannot block.
//
// sseWriter used to arm the deadline, call w.Write, clear it, and only THEN
// call rc.Flush — so the protection its comment argued for did not exist. The
// Write it bounded merely filled a bufio.Writer; the flush it left unbounded
// was the syscall. This test fails against that ordering, and against a
// handler that leaves the connection deadline-free for net/http's own teardown.
//
// It drives the chain with a writer of its own rather than through
// newStreamFixture, because the fixture's real socket has a real peer that
// reads — and a peer that reads is exactly the case where this bug is
// invisible.
func TestStreamWriteDeadlineSpansTheFlush(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(d *httpapi.Deps) { d.StreamWriteTimeout = time.Second })
	plan := &runmesh.Plan{Name: "job_deaf", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo"}}}
	job := plan.Build("job_deaf", epoch, runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3})
	if _, err := f.store.CreateJob(t.Context(), job, ""); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/jobs/job_deaf/stream", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	peer := newDeafPeer()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handler.ServeHTTP(peer, req)
	}()

	// THE ASSERTION IS THAT THE HANDLER RETURNS. Nothing here waits on elapsed
	// time: either the handler comes back, or the peer reports a flush that ran
	// unbounded, and both are channels.
	select {
	case <-done:
	case <-peer.unbounded:
		t.Fatal("a frame was flushed with no write deadline armed, so this handler " +
			"is waiting on a socket that will never drain: a dead-but-open peer " +
			"holds the goroutine and its stream slot until the process restarts")
	case <-t.Context().Done():
		t.Fatal("the stream handler never returned against a peer that stopped reading")
	}
	select {
	case <-peer.unbounded:
		t.Error("the handler returned, but a flush along the way ran with no " +
			"deadline armed")
	default:
	}

	// The connection does not go back to net/http deadline-free. finishRequest
	// — the flush of whatever is still buffered, plus the chunked terminator —
	// runs after the handler returns and against this peer would block for
	// ever, past every defer that releases the stream slot.
	if !peer.armed() {
		t.Error("the handler left the connection with no write deadline, so " +
			"net/http's own teardown write is unbounded")
	}

	status, body := peer.snapshotOf()
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200: the stream opened before the peer went deaf", status)
	}
	// Proof it failed on a real frame's flush rather than before writing
	// anything: the snapshot reached the buffer, and only the syscall failed.
	if !strings.Contains(body, "event: snapshot") {
		t.Errorf("the handler never got as far as a frame: %q", body)
	}
	if n := f.api.ActiveStreams(); n != 0 {
		t.Errorf("ActiveStreams() = %d after the handler returned, want 0", n)
	}
}

// ------------------------------------------------------------------- 2. drain

// TestStreamEndsOnDrain is the test that stops every deploy with a dashboard
// attached from exiting non-zero.
//
// http.Server.Shutdown waits for connections to become IDLE, and an SSE
// connection never does. A handler that ignored the drain signal would hold
// Shutdown for the full RUNMESH_SHUTDOWN_GRACE, hand back DeadlineExceeded, and
// turn that into code = 1 in cmd/server/run.go.
func TestStreamEndsOnDrain(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t)
	job := f.createJob("job_drain", "a")

	resp, br := f.open(job.ID, "", nil)
	readSnapshot(t, br)
	f.parked()

	f.api.Draining()

	bye := expectFrame(t, br, "bye")
	var payload struct {
		Reason string `json:"reason"`
	}
	decodeFrame(t, bye, &payload)
	if payload.Reason != "draining" {
		t.Errorf("bye reason = %q, want %q", payload.Reason, "draining")
	}
	if bye.id != "" {
		t.Errorf("the bye frame carried id %q; only real events move the cursor", bye.id)
	}
	expectEOF(t, br)
	_ = resp.Body.Close()

	// The real property: the connection went idle, so the server can shut down.
	// Before the drain case existed this call never returned.
	done := make(chan error, 1)
	go func() { done <- f.srv.Config.Shutdown(t.Context()) }()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown with a stream attached: %v", err)
	}
}

// TestStreamSaysGoodbyeWhenTheStoreCloses is the sibling case, and it exists
// because reading a closed subscription as "this job's timeline ended" would
// make a rolling restart look to every dashboard like every in-flight job had
// completed.
func TestStreamSaysGoodbyeWhenTheStoreCloses(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t)
	job := f.createJob("job_store_close", "a")

	_, br := f.open(job.ID, "", nil)
	readSnapshot(t, br)
	f.parked()

	f.bus.closeAll()

	bye := expectFrame(t, br, "bye")
	var payload struct {
		Reason string `json:"reason"`
	}
	decodeFrame(t, bye, &payload)
	if payload.Reason != "store_closed" {
		t.Errorf("bye reason = %q, want %q", payload.Reason, "store_closed")
	}
	if bye.event == "end" {
		t.Error("a shutting-down store must never be reported as the job ending")
	}
	expectEOF(t, br)
}

// ---------------------------------------------------------------- 3. backfill

// TestStreamBackfillsADroppedEvent: the bus drops for a slow consumer rather
// than blocking, because observability may never apply backpressure to
// execution. That is only acceptable because the per-job seq is gap-free, so a
// jump says exactly what was missed and one durable read repairs it.
//
// The drop is injected the honest way — the durable timeline gains a burst and
// the bus delivers only the LAST event of it, which is precisely what a
// subscriber whose buffer overflowed sees.
func TestStreamBackfillsADroppedEvent(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t, func(d *httpapi.Deps) { d.StreamBuffer = 1 })
	job := f.createJob("job_backfill", "a", "b")

	_, br := f.open(job.ID, "", nil)
	snap := readSnapshot(t, br)
	f.parked()

	// The whole job runs while the client is not reading.
	f.runStep(job.ID)
	f.runStep(job.ID)
	last := f.lastSeq(job.ID)
	if last <= snap.NextAfter+1 {
		t.Fatalf("the burst appended %d events; this test needs several", last-snap.NextAfter)
	}
	f.bus.send(f.eventAt(job.ID, last))

	var got []uint64
	for {
		fr := readFrame(t, br)
		if fr.event == "end" {
			break
		}
		if fr.event != "event" {
			t.Fatalf("unexpected %q frame mid-backfill (data %s)", fr.event, fr.data)
		}
		var e runmesh.Event
		decodeFrame(t, fr, &e)
		if fr.id != strconv.FormatUint(e.Seq, 10) {
			t.Errorf("frame id %q does not match the event's seq %d", fr.id, e.Seq)
		}
		got = append(got, e.Seq)
	}

	want := snap.NextAfter + 1
	for _, seq := range got {
		if seq != want {
			t.Fatalf("the client observed seq %d where %d was due; the sequence has a "+
				"hole or a duplicate in it (%v)", seq, want, got)
		}
		want++
	}
	if want-1 != last {
		t.Errorf("the client's cursor reached %d, want %d", want-1, last)
	}
	expectEOF(t, br)
}

// ------------------------------------------------------------------ 4. resync

// TestStreamResyncsOnTruncation: the one drop that cannot be repaired is the
// in-memory ring evicting history the client never received. Rendering a
// timeline with a silent hole is the failure worth a frame of its own.
func TestStreamResyncsOnTruncation(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t, withTinyEventRing(t, 2))
	job := f.createJob("job_truncated", "a")
	f.runStep(job.ID)

	// A cursor that predates everything the ring still holds.
	_, br := f.open(job.ID, "?after=1", nil)

	fr := expectFrame(t, br, "resync")
	var payload struct {
		Reason    string `json:"reason"`
		OldestSeq uint64 `json:"oldest_seq"`
	}
	decodeFrame(t, fr, &payload)
	if payload.Reason != "truncated" {
		t.Errorf("resync reason = %q, want %q", payload.Reason, "truncated")
	}
	if payload.OldestSeq < 2 {
		t.Errorf("resync oldest_seq = %d, want the ring's oldest surviving seq", payload.OldestSeq)
	}

	// The snapshot FOLLOWS the resync and the stream continues, which is the one
	// place resync does not mean "close". Closing here would be an infinite
	// reconnect: the ring reports truncation against ?after=0 too, so a client
	// told to start over would be told to start over again, for ever. The
	// snapshot it gets instead IS the refetch.
	snap := readSnapshot(t, br)
	if !snap.Truncated {
		t.Error("the snapshot does not repeat the truncation it was preceded by")
	}
	if len(snap.Events) == 0 {
		t.Error("the snapshot after a resync is empty; there is nothing to resync TO")
	}
	expectFrame(t, br, "end")
	expectEOF(t, br)
}

// ------------------------------------------------------------------ 5. resume

// TestStreamResumesWithoutGapOrDuplicate: a dropped connection resumes from its
// last id and receives exactly what happened while it was away — no gap, and no
// event it has already rendered.
func TestStreamResumesWithoutGapOrDuplicate(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t)
	job := f.createJob("job_resume", "a", "b")

	first, br := f.open(job.ID, "", nil)
	snap := readSnapshot(t, br)
	f.parked()
	if snap.NextAfter == 0 {
		t.Fatal("the first snapshot carried no cursor to resume from")
	}
	_ = first.Body.Close()

	f.runStep(job.ID)

	_, br2 := f.open(job.ID, "?after="+strconv.FormatUint(snap.NextAfter, 10), nil)
	resumed := readSnapshot(t, br2)

	if len(resumed.Events) == 0 {
		t.Fatal("the resumed snapshot is empty; the events written while the client " +
			"was away were lost")
	}
	if got := resumed.Events[0].Seq; got != snap.NextAfter+1 {
		t.Errorf("the first event after resuming is seq %d, want %d", got, snap.NextAfter+1)
	}
	for i, e := range resumed.Events {
		if want := snap.NextAfter + uint64(i) + 1; e.Seq != want {
			t.Fatalf("event %d is seq %d, want %d: `after` is exclusive and gap-free",
				i, e.Seq, want)
		}
	}
}

// ------------------------------------------------------------- 6. the cursor

// TestLastEventIDIsHonouredWhenAfterIsAbsent: `id:` on every frame,
// `Last-Event-ID` on reconnect and the polling endpoint's `?after=` are all the
// same number, which is why this transport needed no resume protocol designing.
func TestLastEventIDIsHonouredWhenAfterIsAbsent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		query     string
		lastEvent string
		wantAfter uint64
	}{
		{
			name:      "Last-Event-ID alone resumes from it",
			lastEvent: "2",
			wantAfter: 2,
		},
		{
			// A caller that wrote a cursor into the URL is deliberately
			// overriding whatever the transport remembered.
			name:      "an explicit after wins over Last-Event-ID",
			query:     "?after=1",
			lastEvent: "5",
			wantAfter: 1,
		},
		{
			// The header is set by the user agent, not by the caller. Refusing
			// a connection because an intermediary mangled it would be a
			// failure the client cannot act on; starting over always can be.
			name:      "a mangled Last-Event-ID is ignored rather than refused",
			lastEvent: "not-a-number",
			wantAfter: 0,
		},
		{
			name:      "no cursor at all starts from the beginning",
			wantAfter: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStreamFixture(t)
			job := f.createJob("job_cursor", "a", "b")
			f.runStep(job.ID)
			f.runStep(job.ID)

			headers := map[string]string{}
			if tc.lastEvent != "" {
				headers["Last-Event-ID"] = tc.lastEvent
			}
			_, br := f.open(job.ID, tc.query, headers)
			snap := readSnapshot(t, br)

			if len(snap.Events) == 0 {
				t.Fatalf("the snapshot is empty; nothing to check a cursor against")
			}
			if got := snap.Events[0].Seq; got != tc.wantAfter+1 {
				t.Errorf("the first event is seq %d, want %d (cursor %d, exclusive)",
					got, tc.wantAfter+1, tc.wantAfter)
			}
		})
	}
}

// TestStreamRejectsAMalformedAfter: the same rules, down to the wording, as
// GET /api/v1/jobs/{id}/events. It is the same cursor, and a client that
// learned one set of rules should not discover a second.
func TestStreamRejectsAMalformedAfter(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t)
	job := f.createJob("job_bad_cursor", "a")

	resp := f.request("/api/v1/jobs/"+job.ID+"/stream?after=abc", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("?after=abc = %d, want 400", resp.StatusCode)
	}
	env := decodeResponseEnvelope(t, resp)
	if env.Error.Code != httpapi.CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodeInvalidArgument)
	}
	if !strings.Contains(env.Error.Message, "after") {
		t.Errorf("message %q does not name the parameter", env.Error.Message)
	}
}

// ---------------------------------------------------------------- 7. the cap

// TestStreamRefusesOverTheLimit: an open stream costs a goroutine, a store
// subscription and a socket for as long as a tab is left open. The refusal
// reuses runmesh.ErrQueueFull rather than inventing a status, so the one
// classifyError switch still decides every status code this package returns.
func TestStreamRefusesOverTheLimit(t *testing.T) {
	t.Parallel()

	f := newStreamFixture(t, func(d *httpapi.Deps) { d.StreamMax = 1 })
	job := f.createJob("job_capped", "a")

	_, br := f.open(job.ID, "", nil)
	readSnapshot(t, br)
	f.parked() // the one permitted slot is definitively held

	resp := f.request("/api/v1/jobs/"+job.ID+"/stream", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the second stream = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q: a reconnecting client must be told not "+
			"to spin", got, "1")
	}
	env := decodeResponseEnvelope(t, resp)
	if env.Error.Code != httpapi.CodeResourceExhausted {
		t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodeResourceExhausted)
	}

	// The refusal must not have consumed the slot it refused: the counter is
	// incremented to test it and decremented again on the way out, and getting
	// that wrong would make the cap shrink by one on every rejected request
	// until the route stopped working entirely.
	if n := f.api.ActiveStreams(); n != 1 {
		t.Errorf("ActiveStreams() = %d after a refusal, want 1", n)
	}
}

// ----------------------------------------------------------- 8. the terminal

// TestStreamEndsWhenTheJobIsTerminal: `end` means STOP RECONNECTING. Without
// it a finished job's dashboard holds an idle connection, and its poll ticker,
// for as long as the tab is open.
func TestStreamEndsWhenTheJobIsTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// cursorAtTheEnd asks for a cursor PAST the JOB_FINISHED event, which is
		// the case the job's own state has to answer: no event will ever arrive
		// to announce an ending that already happened.
		cursorAtTheEnd bool
	}{
		{name: "the terminal event is inside the snapshot"},
		{name: "the cursor is already past the terminal event", cursorAtTheEnd: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStreamFixture(t)
			job := f.createJob("job_terminal", "a")
			f.runStep(job.ID)

			query := ""
			if tc.cursorAtTheEnd {
				query = "?after=" + strconv.FormatUint(f.lastSeq(job.ID), 10)
			}
			_, br := f.open(job.ID, query, nil)
			snap := readSnapshot(t, br)
			if tc.cursorAtTheEnd && len(snap.Events) != 0 {
				t.Errorf("a cursor at the end returned %d events", len(snap.Events))
			}

			fr := expectFrame(t, br, "end")
			var payload struct {
				State string `json:"state"`
			}
			decodeFrame(t, fr, &payload)
			if payload.State != runmesh.Succeeded.String() {
				t.Errorf("end state = %q, want %q", payload.State, runmesh.Succeeded.String())
			}
			expectEOF(t, br)

			if n := f.api.ActiveStreams(); n != 0 {
				t.Errorf("ActiveStreams() = %d after the stream ended, want 0", n)
			}
		})
	}
}

// ------------------------------------------------------------------ 9. panic

// TestPanicMidStreamDoesNotSpliceJSON pins blocker #3.
//
// Recover used to write its JSON envelope unconditionally. On a response whose
// header is already out that produces a superfluous-WriteHeader warning through
// srv.ErrorLog AND splices a raw `{"error":...}` object into a text/event-stream
// body — which is not a frame, so the browser's parser sees a malformed chunk
// and the client's own reconnect walks straight back into the same panic with
// no record of why.
//
// The chain is composed by hand rather than driven through the route table,
// because the panic has to ESCAPE the handler to reach Recover at all, and
// jobStream's own barrier catches its own panics by design (the case below).
// This is the middleware under test, not the stream handler.
func TestPanicMidStreamDoesNotSpliceJSON(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(epoch)
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))

	// flushed carries the flush's own error, and receiving from it is the
	// SYNCHRONISATION rather than a report read afterwards.
	//
	// It used to be an atomic.Bool the test read once the client's Do had
	// returned, and that was a race the test lost under load: Do returns when
	// the response HEADERS have been parsed, and the headers reach the client
	// from inside rc.Flush — so the client can be scheduled, read the headers
	// and return from Do while this handler goroutine has not yet come back out
	// of Flush to record anything. On an idle machine the handler always won;
	// with the CPU saturated it did not, and the test failed claiming the chain
	// would not flush. A buffered channel makes the handler's progress a
	// happens-before edge instead of a hope.
	flushed := make(chan error, 1)
	streaming := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("id: 1\nevent: snapshot\ndata: {\"job\":{}}\n\n"))
		// Through the recorder Logger installed, which is the whole point.
		flushed <- http.NewResponseController(w).Flush()
		panic("injected mid-stream")
	})

	handler := httpapi.RequestID(clk)(httpapi.Logger(log, clk, nil)(httpapi.Recover(log)(streaming)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// A blocking receive: it returns exactly when the handler has flushed, so
	// nothing below is read before the value it is asserting on exists.
	select {
	case err := <-flushed:
		if err != nil {
			t.Errorf("the chain would not flush; http.NewResponseController could "+
				"not unwrap the status recorder: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("the handler never reached its flush")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: the header was already sent, so a panic "+
			"cannot retroactively make this a 500", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !strings.Contains(string(body), "event: snapshot") {
		t.Fatalf("the frame written before the panic is missing: %q", body)
	}
	if strings.Contains(string(body), `{"error"`) {
		t.Fatalf("a JSON envelope was spliced into an event-stream body: %q", body)
	}
	// And the body is still nothing but SSE, line by line — readFrame's default
	// arm fails on anything that is not a field.
	br := bufio.NewReader(strings.NewReader(string(body)))
	if fr := readFrame(t, br); fr.event != "snapshot" {
		t.Errorf("the surviving frame is %q, want snapshot", fr.event)
	}
}

// TestAMidStreamFailureBecomesAnErrorFrame is the other half of the same
// argument. Recover falling silent once the header is out is correct for the
// wire, but on its own it would leave the client with a bare EOF and no
// explanation — so the handler owns its own failure reporting from its first
// flush onward, the way the engine's panic barriers still produce an outcome.
//
// The frame still routes through classifyError, so a stream does not get a
// private error vocabulary, and the cause is logged rather than serialised.
func TestAMidStreamFailureBecomesAnErrorFrame(t *testing.T) {
	t.Parallel()

	// One successful read for the snapshot, then every later one panics.
	faulty := &panicAfterNStore{after: 1}
	f := newStreamFixture(t,
		func(d *httpapi.Deps) { d.StreamPollInterval = time.Second },
		withFaultyStore(faulty),
	)
	job := f.createJob("job_faulty", "a")

	_, br := f.open(job.ID, "", nil)
	readSnapshot(t, br)
	f.parked()

	// The poll tick is what reaches the store again.
	f.clk.Advance(time.Second)

	fr := expectFrame(t, br, "error")
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	decodeFrame(t, fr, &payload)
	if payload.Error.Code != httpapi.CodeInternal {
		t.Errorf("code = %q, want %q", payload.Error.Code, httpapi.CodeInternal)
	}
	if payload.Error.RequestID == "" {
		t.Error("the error frame carries no request id, so nothing correlates it " +
			"with the log line that holds the cause")
	}
	if strings.Contains(fr.data, "panic") || strings.Contains(fr.data, "goroutine") {
		t.Errorf("the error frame leaks internals: %s", fr.data)
	}
	expectEOF(t, br)

	if n := f.api.ActiveStreams(); n != 0 {
		t.Errorf("ActiveStreams() = %d after a panicking stream ended, want 0: the "+
			"admission slot has to be released on the panic path too", n)
	}
}

// unwrapStore lets the fixture find the concrete store under a test wrapper.
type unwrapStore interface{ unwrap() httpapi.Store }

// withFaultyStore wraps whatever store the fixture built, so a fault fires on
// the handler's reads and not on the fixture's own seeding.
func withFaultyStore(fault *panicAfterNStore) func(*httpapi.Deps) {
	return func(d *httpapi.Deps) {
		fault.Store = d.Store
		d.Store = fault
	}
}

// panicAfterNStore fails the way a store actually fails at the worst possible
// moment: after the stream's header has gone out, when writeError is no longer
// available because a status code has already been chosen.
type panicAfterNStore struct {
	httpapi.Store
	after int64
	calls atomic.Int64
}

func (s *panicAfterNStore) unwrap() httpapi.Store { return s.Store }

func (s *panicAfterNStore) JobEvents(ctx context.Context, jobID string, afterSeq uint64, limit int) (runmesh.EventPage, error) {
	if s.calls.Add(1) > s.after {
		panic("injected store panic")
	}
	return s.Store.JobEvents(ctx, jobID, afterSeq, limit)
}

// decodeResponseEnvelope reads the one error shape off a real *http.Response,
// the way decodeEnvelope does off a recorder.
func decodeResponseEnvelope(t *testing.T, resp *http.Response) envelope {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the error body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("the error body is not an envelope: %v (body %s)", err, body)
	}
	if e.Error.Code == "" || e.Error.Message == "" || e.Error.RequestID == "" {
		t.Errorf("the envelope is incomplete: %s", body)
	}
	return e
}
