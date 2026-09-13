package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// GET /api/v1/jobs/{id}/stream — the live execution timeline as Server-Sent
// Events. ADR 0013 is the argument for SSE over the WebSocket the plan document
// named; this file is the implementation, and three things about it are
// load-bearing enough to say here.
//
// FIRST, THE DURABLE TIMELINE IS THE SOURCE OF TRUTH AND THE BUS IS AN
// ACCELERATOR. Both stores' Subscribe is process-local, so on a two-replica
// deployment a handler served only from the in-process bus would show one
// dashboard the events THIS replica happened to write and silently omit the
// rest. So the loop below carries a poll ticker over Store.JobEvents alongside
// the subscription: a live event is emitted immediately and resets that ticker,
// and anything the bus never saw is picked up within one poll interval. Nothing
// can be permanently missed, and there is no "single replica only" caveat to
// document. LISTEN/NOTIFY is the eventual upgrade and drops in as a second
// producer behind internal/eventbus, changing nothing here.
//
// SECOND, THE CURSOR IS SSE's OWN. runmesh.Event.Seq is per-job, 1-based and
// gap-free, and `?after=` on the polling endpoint is exclusive on exactly that
// field. So `id:` on every frame, `Last-Event-ID` on reconnect and the existing
// resume token are all THE SAME NUMBER. The resume protocol is not designed
// here; it is inherited.
//
// THIRD, A DROP IS REPAIRABLE RATHER THAN VISIBLE. The bus drops for a slow
// consumer instead of blocking, because observability may never apply
// backpressure to execution — but because Seq is gap-free, a jump from
// lastSeq to something greater than lastSeq+1 says exactly what was missed, and
// one JobEvents read fills it. The client sees an unbroken sequence either way.
// The one case that cannot be repaired is the in-memory ring evicting history
// the client never received, and that gets an explicit `resync` frame rather
// than a timeline rendered with a silent hole in it.

// streamRoute is the pattern, spelled once. routes() registers it and
// StreamingRoutePatterns() reports it to the metrics wiring, so the two cannot
// drift — which matters, because the consequence of them drifting is a
// request-duration histogram quietly poisoned by connection lifetime.
const streamRoute = "GET /api/v1/jobs/{id}/stream"

// StreamingRoutePatterns names the routes whose connections are long-lived.
//
// It exists for one specific trap. Logger's duration observation is emitted
// from a deferred func that runs when the HANDLER RETURNS, and a stream handler
// returns when the dashboard is closed — minutes or hours later. Feeding that
// into runmesh_http_request_duration_seconds would put a 3,600,000 ms
// observation in the same histogram as a 4 ms readiness probe, and every
// latency panel in the deployment would be describing how long people leave
// browser tabs open. Nothing fails when that happens; the graph is simply
// wrong. So the metric set excludes these routes from the duration histogram
// and exports the count as a gauge instead — see API.ActiveStreams.
func StreamingRoutePatterns() []string { return []string{streamRoute} }

// streamPageLimit is the page size for every JobEvents read this handler makes:
// the snapshot, the backfill after a dropped event, and the poll catch-up. It
// is the polling endpoint's own documented maximum, so the stream can never ask
// the store for a page shape a client could not already have asked for.
const streamPageLimit = 1000

// noStreamer is the live feed for a deployment wired without one. Its channel
// is never written to and never closed, so the handler simply falls through to
// its poll ticker — which is exactly the degradation an accelerator should have.
type noStreamer struct{}

func (noStreamer) Subscribe(string, int) (<-chan runmesh.Event, func()) {
	return make(chan runmesh.Event), func() {}
}

// ActiveStreams is how many SSE connections this process is serving.
//
// Exported so the metrics wiring can bind it as a scrape-time gauge, in the
// same pull-from-the-owner shape every other gauge in this binary uses: nothing
// is mirrored, so nothing can drift from the counter admission control actually
// consults.
func (a *API) ActiveStreams() int { return int(a.streams.Load()) }

// jobStream is the handler. It is an ordinary http.HandlerFunc so it drops into
// the existing route table, carries the existing scope wrapper, and sits behind
// the whole middleware chain like every other endpoint.
//
// THE ORDER OF THE FIRST FIVE STEPS IS THE DESIGN. Every one of them can fail,
// and every one of them runs before a single byte is written — which is what
// lets all of them report through writeError, keeping the "one classifyError
// switch decides every status code" rule intact for a route that spends most of
// its life past the point where a status code can still be chosen.
func (a *API) jobStream(w http.ResponseWriter, r *http.Request) {
	// 1. Admission. An open stream costs a goroutine, a store subscription and
	//    a socket for as long as a tab is left open, so the number of them is
	//    capped. The refusal reuses runmesh.ErrQueueFull rather than inventing a
	//    status: classifyError already maps it to 429 resource_exhausted, and a
	//    Retry-After tells a reconnecting client not to spin.
	if n := a.streams.Add(1); n > int64(a.maxStreams) {
		a.streams.Add(-1)
		w.Header().Set("Retry-After", "1")
		writeError(w, r, a.log, fmt.Errorf(
			"%w: %d event streams are already open", runmesh.ErrQueueFull, n-1))
		return
	}
	defer a.streams.Add(-1)

	// 2. The cursor.
	after, err := streamCursor(r)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}

	// 3. The job. An unknown id is a 404 with the usual envelope, not a stream
	//    that opens and then says nothing.
	id := r.PathValue("id")
	job, err := a.store.Job(r.Context(), id)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}

	// 4. SUBSCRIBE BEFORE THE SNAPSHOT READ, and this ordering is the whole
	//    no-gap argument. Reading first and subscribing second leaves a window
	//    in which an event is appended after the read and before the
	//    subscription exists, and that event is lost for the life of the
	//    connection. Subscribing first can only produce DUPLICATES — events the
	//    snapshot also contains — and a duplicate is free to discard, because
	//    the cursor is gap-free and `e.Seq <= lastSeq` identifies it exactly.
	live, unsub := a.stream.Subscribe(id, a.streamBuffer)
	defer unsub()

	// 5. The snapshot page.
	page, err := a.store.JobEvents(r.Context(), id, after, streamPageLimit)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}
	if page.Events == nil {
		page.Events = []runmesh.Event{}
	}

	sw := newSSEWriter(w, a.streamWriteTimeout, a.log)
	sw.open()
	// THE LAST SOCKET WRITE OF THIS RESPONSE IS NOT OURS. net/http's
	// finishRequest — the flush of whatever is still in the response's
	// bufio.Writer, plus the chunked terminator — runs after this handler
	// returns, and it runs on whatever deadline the connection was left with.
	// Leaving it cleared would hand those last few bytes to the kernel with no
	// bound at all, which against a dead-but-open peer with a full socket buffer
	// is the same wedged-goroutine failure write() exists to prevent, moved one
	// layer out and past every defer that would have released the stream slot.
	// So the connection leaves this handler with a bounded deadline rather than
	// with none; net/http clears it itself immediately after finishRequest, so
	// nothing is left behind for a later request on a reused connection.
	//
	// Registered BEFORE the recover barrier below, so it runs AFTER it: the
	// barrier's own error frame is written under write()'s per-frame bound, and
	// this is the very last thing to touch the deadline.
	defer sw.close()

	st := &jobStream{api: a, sw: sw, req: r, id: id}

	// The handler's own panic barrier, following the engine's pattern of a
	// barrier that still produces an OUTCOME (engine/worker.go, tools/executor).
	// Recover deliberately stops writing its JSON envelope once the header is
	// out — splicing one into a text/event-stream body is what this whole design
	// is avoiding — so without this the client would see a bare EOF and
	// reconnect straight back into the same panic with nothing to explain it.
	// Here it gets a well-formed error frame first.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		a.log.Error("stream handler panic",
			"request_id", RequestIDFrom(r.Context()), "job_id", id, "panic", p)
		st.fail(errStreamPanicked)
	}()

	// A cursor that predates retained history cannot be repaired, so say so
	// before the snapshot rather than letting the client believe the page it is
	// about to receive is the beginning.
	//
	// The frame is followed by the snapshot and the stream CONTINUES, which is
	// the one place this diverges from resync's mid-stream meaning. Closing here
	// would be an infinite reconnect: the in-memory ring reports truncation
	// against ?after=0 too, so a client told to start over would be told to
	// start over again, for ever. The snapshot it gets instead IS the refetch.
	if page.Truncated {
		if err := sw.frame("resync", 0, false, streamResync{
			Reason: "truncated", OldestSeq: page.OldestSeq,
		}); err != nil {
			return
		}
	}

	if err := sw.frame("snapshot", page.NextAfter, true, streamSnapshot{
		Job:       toJobResponse(job),
		Events:    page.Events,
		NextAfter: page.NextAfter,
		Truncated: page.Truncated,
		OldestSeq: page.OldestSeq,
	}); err != nil {
		return
	}
	st.absorb(page)

	// A job that was already terminal when the client connected. Its JOB_FINISHED
	// may have been inside the snapshot page, may be beyond it if the timeline is
	// longer than one page, or may be BEHIND the client's cursor — which is the
	// case that needs the job's own state, because no event will ever arrive to
	// announce an ending that already happened.
	if job.State.Terminal() && !st.finished {
		if !st.catchUp() {
			return
		}
		if !st.finished {
			st.finished, st.endState = true, job.State.String()
		}
	}
	if st.finished {
		_ = sw.frame("end", 0, false, streamEnd{State: st.endState})
		return
	}

	// ONE SELECT OVER FIVE CASES, following the single-select design
	// internal/engine/worker.go documents at length. Splitting these across
	// goroutines is how a drain gets misreported as a completed job, or a
	// heartbeat races a terminal frame onto the wire after the stream has
	// logically ended.
	heartbeat := a.clock.NewTicker(a.streamHeartbeat)
	defer heartbeat.Stop()
	poll := a.clock.NewTicker(a.streamPollInterval)
	defer func() { poll.Stop() }()

	for {
		select {
		// The client closed the tab, or the connection died. There is no
		// http.TimeoutHandler anywhere in this chain, so this is the handler's
		// only cancellation signal besides the drain.
		case <-r.Context().Done():
			return

		// The process is shutting down. RETURNING HERE IS WHAT MAKES DEPLOYS
		// WORK: http.Server.Shutdown waits for connections to go IDLE, and an
		// SSE connection never does, so a handler that ignored this would block
		// the full RUNMESH_SHUTDOWN_GRACE, hand back DeadlineExceeded, and turn
		// every deploy with a dashboard attached into a non-zero exit.
		case <-a.draining:
			_ = sw.frame("bye", 0, false, streamBye{Reason: "draining"})
			return

		case e, ok := <-live:
			if !ok {
				// The store closed the subscription: it is shutting down. This
				// is emphatically NOT end-of-job — reporting it as `end` would
				// make a rolling restart look to every dashboard like every
				// in-flight job had completed.
				_ = sw.frame("bye", 0, false, streamBye{Reason: "store_closed"})
				return
			}
			// A jump means the bus dropped something for this subscriber.
			// Because Seq is gap-free, the repair is exact: read the missing
			// span back from the durable timeline and emit it in order.
			if e.Seq > st.lastSeq+1 && !st.catchUp() {
				return
			}
			if err := st.emit(e); err != nil {
				return
			}
			// An active stream on the replica that wrote the event has nothing
			// to poll for, so the ticker starts again from now. On an idle job
			// this changes nothing; on a busy one it is the difference between
			// one indexed query per second per viewer and none.
			poll.Stop()
			poll = a.clock.NewTicker(a.streamPollInterval)

		case <-heartbeat.C():
			// A bare comment. It carries no id, so it can never move a client's
			// cursor, and it is what keeps a proxy or a load balancer from
			// reaping a connection that has been correct but silent.
			if err := sw.ping(); err != nil {
				return
			}

		case <-poll.C():
			// The leg that makes this multi-replica correct: it picks up
			// anything written by a replica whose bus this connection is not
			// subscribed to.
			if !st.catchUp() {
				return
			}
		}

		if st.finished {
			_ = sw.frame("end", 0, false, streamEnd{State: st.endState})
			return
		}
	}
}

// jobStream carries the per-connection cursor state. It is a struct rather than
// four local variables because catchUp is called from three places and every
// one of them has to advance the same cursor and observe the same terminal
// flag; passing four pointers around would be the same thing spelled worse.
type jobStream struct {
	api *API
	sw  *sseWriter
	req *http.Request
	id  string

	// lastSeq is the highest per-job Seq the client has been sent. It is the
	// `id:` of the last frame and therefore exactly what the client will send
	// back as Last-Event-ID.
	lastSeq uint64

	finished bool
	endState string
}

// absorb records the snapshot page's cursor and notices a JOB_FINISHED inside
// it. The page's events travel inside the snapshot frame rather than through
// emit, so the cursor is taken from NextAfter — which the store pre-initialises
// to the incoming cursor, so an empty page cannot rewind a resuming client.
func (s *jobStream) absorb(page runmesh.EventPage) {
	s.lastSeq = page.NextAfter
	for _, e := range page.Events {
		s.noteTerminal(e)
	}
}

func (s *jobStream) noteTerminal(e runmesh.Event) {
	if e.Type == runmesh.JobFinished {
		s.finished, s.endState = true, e.State.String()
	}
}

// emit writes one event frame, discarding anything at or behind the cursor —
// which is how the duplicates created by subscribing before the snapshot read
// are filtered, and why that ordering is safe.
func (s *jobStream) emit(e runmesh.Event) error {
	if e.Seq <= s.lastSeq {
		return nil
	}
	if err := s.sw.frame("event", e.Seq, true, e); err != nil {
		return err
	}
	s.lastSeq = e.Seq
	s.noteTerminal(e)
	return nil
}

// catchUp drains the durable timeline forward from the cursor, emitting
// everything it finds. It reports whether the stream should continue: false
// means a terminal frame has already been written and the handler must return.
//
// This one function is the backfill after a dropped event, the multi-replica
// poll and the terminal-job drain. They are the same operation — "the store
// knows things this connection has not sent yet" — and writing them once is
// what stops the three from disagreeing about what a truncated page means.
func (s *jobStream) catchUp() bool {
	for {
		before := s.lastSeq
		page, err := s.api.store.JobEvents(s.req.Context(), s.id, before, streamPageLimit)
		if err != nil {
			s.fail(err)
			return false
		}
		if page.Truncated {
			// Mid-stream truncation: history this client has not received has
			// already been evicted. Unlike the open-time case there is no
			// snapshot to follow it with, so the client is told to discard its
			// state and refetch, and the connection ends.
			_ = s.sw.frame("resync", 0, false, streamResync{
				Reason: "truncated", OldestSeq: page.OldestSeq,
			})
			return false
		}
		for _, e := range page.Events {
			if err := s.emit(e); err != nil {
				return false
			}
		}
		// Guard against a store that returns a page without advancing the
		// cursor: no progress means there is nothing left, and looping on it
		// would spin this goroutine at full speed.
		if s.lastSeq <= before {
			return true
		}
	}
}

// fail reports a failure that happened AFTER the header went out, when
// writeError is no longer available because a status code has already been
// chosen.
//
// It still routes through classifyError, so the code and message a client sees
// in the frame are the same ones it would have seen in a normal envelope — a
// stream does not get a private error vocabulary — and the cause is logged,
// never serialised, exactly as writeError does it.
func (s *jobStream) fail(err error) {
	_, api := classifyError(err)
	api.RequestID = RequestIDFrom(s.req.Context())
	s.api.log.Error("event stream failed after its first frame",
		"request_id", api.RequestID, "job_id", s.id, "err", err)
	_ = s.sw.frame("error", 0, false, errorEnvelope{Error: api})
}

// errStreamPanicked is what the handler's own recover barrier classifies. It is
// unexported and unmapped, so classifyError's default arm renders it as the
// same opaque 500-shaped envelope every other panic produces: a client learns
// that the server broke and nothing about how.
var errStreamPanicked = errors.New("httpapi: the stream handler panicked")

// streamCursor resolves where to resume from.
//
// `?after=` is parsed with exactly the rules GET /api/v1/jobs/{id}/events
// applies, down to the wording of the 400, because it is the same cursor and a
// client that learned one set of rules should not discover a second.
// Last-Event-ID is the browser's own resume header and is honoured when the
// query parameter is absent; an EXPLICIT `?after=` wins over it, because a
// caller that wrote a cursor into the URL is deliberately overriding whatever
// the transport remembered.
//
// A malformed Last-Event-ID is IGNORED rather than a 400. It is set by the
// user agent, not by the caller, and refusing a connection because an
// intermediary mangled a header the application never wrote would be a failure
// the client cannot act on; starting from the beginning always can be.
func streamCursor(r *http.Request) (uint64, error) {
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, badRequest("after must be a non-negative integer")
		}
		return n, nil
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, nil
}

// ---------------------------------------------------------------- the writer

// sseWriter frames and flushes. It is the only thing in this package that
// writes to a ResponseWriter more than once.
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	log     *slog.Logger
	timeout time.Duration
	buf     bytes.Buffer
}

func newSSEWriter(w http.ResponseWriter, timeout time.Duration, log *slog.Logger) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w), log: log, timeout: timeout}
}

// open replaces the connection's absolute write deadline with this writer's
// per-frame one and sends the headers.
//
// RUNMESH_WRITE_TIMEOUT is an absolute deadline on the whole response rather
// than an idle timeout, so at its 30-second default every stream would be
// severed on a fixed schedule — which, because a client reconnects, looks like
// "it works" in a two-minute manual test and shows up in production as a
// reconnect storm. Clearing it HERE, per connection, leaves every other route
// protected; setting the server-wide value to 0 would remove slow-client
// protection from all eleven of them to fix one. What replaces it is not
// nothing: every socket write this writer performs, the header flush below
// included, is bracketed by arm/clear.
//
// The clear is also the PROBE. If this connection cannot carry a write
// deadline at all — no Unwrap chain reaches the net.Conn — then neither can
// arm(), so the warning is the one chance an operator gets to learn that this
// stream is running unbounded.
//
// X-Accel-Buffering is not decorative: nginx buffers text/event-stream by
// default and will hold every frame until its buffer fills, which reproduces
// exactly the bug the statusRecorder Unwrap fixed, one layer further out.
func (s *sseWriter) open() {
	if err := s.rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("could not clear this connection's write deadline; the stream "+
			"will be severed at RUNMESH_WRITE_TIMEOUT", "err", err)
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	// X-Request-ID is already on the response: RequestID sets it before any
	// handler runs, so it is part of the SSE headers for free and a client can
	// quote it when a stream misbehaves.
	s.w.WriteHeader(http.StatusOK)
	_ = s.push()
}

// close leaves the connection with a bounded write deadline instead of none,
// for the teardown net/http performs after the handler returns. See the
// `defer sw.close()` in jobStream for why that teardown needs a bound.
func (s *sseWriter) close() { s.arm() }

// frame writes one id/event/data triple terminated by a blank line.
//
// The payload is marshalled with encoding/json, which compacts and escapes, so
// a data line can never contain a raw newline and split one frame into two —
// including for stepResponse.Result, which is arbitrary tool output arriving as
// a json.RawMessage.
func (s *sseWriter) frame(event string, id uint64, hasID bool, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		// The frame is beyond saving and the connection is mid-body, so there
		// is no envelope to fall back on. Log it and end the stream; the client
		// reconnects from its last id and loses nothing it had been sent.
		s.log.Error("could not marshal a stream frame", "event", event, "err", err)
		return err
	}

	s.buf.Reset()
	if hasID {
		s.buf.WriteString("id: ")
		s.buf.WriteString(strconv.FormatUint(id, 10))
		s.buf.WriteByte('\n')
	}
	s.buf.WriteString("event: ")
	s.buf.WriteString(event)
	s.buf.WriteByte('\n')
	s.buf.WriteString("data: ")
	s.buf.Write(body)
	s.buf.WriteString("\n\n")
	return s.write(s.buf.Bytes())
}

// ping is the heartbeat: an SSE comment, which every client ignores and no
// client can mistake for an event. It deliberately carries no id.
func (s *sseWriter) ping() error { return s.write([]byte(": ping\n\n")) }

// arm bounds the next socket write; clear takes the bound off again.
//
// THE DEADLINE IS A REAL CLOCK READ, AND IT HAS TO BE. Every other deadline in
// this codebase goes through internal/clock so a test can drive it — but this
// one is not handed to Go code, it is handed to the KERNEL's netpoller, which
// compares it against wall-clock time and knows nothing about a fake. Handing
// it a fake epoch of 2026-09-05 would make every write on a real socket expire
// instantly, so the fake clock drives this stream's protocol timing — the
// heartbeat and the poll, both through a.clock — and the socket deadline stays
// what the operating system understands.
//
// It is bounded rather than absent because a TCP peer that is dead but not
// closed, with a full socket buffer, is otherwise an unbounded handler
// goroutine: the write blocks for ever and the connection is counted against
// the stream budget until the process restarts.
func (s *sseWriter) arm() {
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout)) // clock:allow: a socket write deadline is a kernel deadline in wall-clock time; a fake epoch here expires every write instantly
}

// clear drops the bound, so the next frame's idle wait — which may be a whole
// heartbeat interval away — is not measured against the previous frame's
// deadline. Every armed span in this file is closed by a paired clear, except
// the deliberate one close() leaves for net/http's teardown.
func (s *sseWriter) clear() { _ = s.rc.SetWriteDeadline(time.Time{}) }

// push empties the response's buffer onto the socket under the bound.
//
// THE FLUSH IS THE SYSCALL, WHICH IS WHY THE BOUND HAS TO SPAN IT. Every frame
// this handler emits — event, ping, end, bye, resync, error — is far smaller
// than net/http's 2048-byte bufferBeforeChunkingSize, so a bare w.Write only
// fills a bufio.Writer and touches no file descriptor at all. Arming the
// deadline around the Write and clearing it before the Flush, which is what
// this writer used to do, therefore bounded the one call that cannot block and
// left the one that can running free: against a dead-but-open peer whose socket
// buffer is full, rc.Flush blocked for ever and held both a handler goroutine
// and a stream slot until the process restarted — precisely the failure the
// deadline was added to prevent.
func (s *sseWriter) push() error {
	s.arm()
	defer s.clear()
	return s.rc.Flush()
}

// write puts one frame on the wire under a bounded deadline.
//
// One armed span covers the buffer fill AND the flush, for the reason push
// argues: the flush is where the bytes actually leave.
func (s *sseWriter) write(b []byte) error {
	s.arm()
	defer s.clear()
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	return s.rc.Flush()
}

// Compile-time proof the no-op streamer really is a Streamer, so a nil Deps
// field cannot become a nil-pointer panic on the first request instead of a
// build error here.
var _ Streamer = noStreamer{}
