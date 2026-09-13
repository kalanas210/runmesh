// Package httpapi is the HTTP surface: net/http and nothing else.
//
// Go 1.22's ServeMux understands method and wildcard patterns
// ("POST /api/v1/jobs", "GET /api/v1/jobs/{id}"), which is the routing that
// used to justify reaching for Chi or Gin. Middleware is plain
// func(http.Handler) http.Handler composition, and there is no framework
// context type propagating through the codebase to obscure the
// context.Context plumbing this project exists to demonstrate.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Runtime is what the API needs to know about the engine. Declaring it here,
// as a two-method interface, keeps httpapi from importing engine — so the two
// packages can be tested independently and neither can reach into the other.
type Runtime interface {
	Inflight() int
	Workers() int
}

// Deps are the API's collaborators.
type Deps struct {
	Store    Store
	Tools    tools.Registry
	Runtime  Runtime
	Clock    clock.Clock
	Log      *slog.Logger
	APIKeys  map[[32]byte]config.APIKey
	Limits   runmesh.Limits
	Defaults runmesh.Defaults

	MaxRequestBytes int64
	MaxQueueDepth   int
	// Durable reports whether the backing store survives a restart. Week 1
	// says false, loudly, on GET /ready.
	Durable bool
	// StoreName appears in the readiness body: "memory" or "postgres".
	StoreName string

	// ExecutionMode is where tools actually run. The registry's own
	// descriptors cannot know: the same echo tool is in_process under the
	// local executor and container under the Kubernetes one, and serving the
	// wrong answer would mislead the dashboard and, in Week 5, the planner
	// that is handed these descriptors as function declarations.
	ExecutionMode tools.ExecutionMode

	// Sandbox is the execution policy. The API consults it at SUBMIT time, so
	// a plan naming a tool this deployment refuses is a 400 that says which
	// switch to flip — rather than a job that is accepted, queued, dispatched
	// and then failed by a worker with the same message an hour later.
	//
	// It is the same object the engine holds, which is what makes the two
	// answers identical by construction rather than by review.
	Sandbox Policy

	// Planner turns a goal into a validated plan. Optional: a deployment with
	// no planner still serves every other route, and the two planning
	// endpoints answer 501 rather than 404 — the difference between "this
	// server does not do that" and "you got the URL wrong" is worth a status
	// code.
	Planner Planner

	// Gatherer renders GET /api/v1/metrics. Optional, on the same terms as
	// Planner: nil answers 501 rather than 404.
	Gatherer Gatherer

	// Stream is the live event accelerator. Optional, and a nil one is
	// defaulted to a no-op rather than refused, because correctness does not
	// depend on it: GET /api/v1/jobs/{id}/stream serves the whole timeline from
	// the durable store on its poll ticker either way, and the bus only shortens
	// the wait. A deployment without one degrades to poll-speed, which is the
	// right failure mode for an accelerator.
	Stream Streamer

	// The streaming knobs, all five loaded from the environment in
	// cmd/server/run.go. Each is defaulted here to the same value config.Load
	// defaults it to, so a test that constructs an API without mentioning any of
	// them gets the shipped behaviour rather than a zero-valued ticker panic.
	StreamMax          int
	StreamBuffer       int
	StreamHeartbeat    time.Duration
	StreamPollInterval time.Duration
	StreamWriteTimeout time.Duration
	// Metrics receives one call per finished request, from inside the log
	// line's own deferred func. Optional and nil-safe, so a test that builds an
	// API without telemetry composes exactly as it did before.
	Metrics HTTPObserver
}

// Planner is the slice of internal/planner the API needs, declared here as an
// interface for the same reason Runtime and Policy are: httpapi imports the
// planner package for its Goal and Trace types, and nothing else.
type Planner interface {
	Plan(ctx context.Context, goal planner.Goal) (planner.Result, error)
}

// Streamer is the live event source, declared here as the narrowest slice the
// API needs — one method — so httpapi never imports internal/eventbus and the
// two packages stay independently testable. It is the same consumer-declared
// pattern as Runtime, Policy and Planner above.
//
// It is deliberately NOT added to Store. The stream handler reads its history
// through Store.Job and Store.JobEvents, which that interface already has;
// live push is a different concern with a different failure mode (lossy by
// design, process-local, optional) and folding it into the persistence
// interface would make every store implementation and every test double
// responsible for it.
type Streamer interface {
	// Subscribe returns this job's live events and the function that ends the
	// subscription. Delivery is lossy under backpressure by contract: a full
	// buffer drops rather than blocking, because a dashboard must never be able
	// to slow down execution. The caller detects a drop from the gap-free
	// per-job Seq and repairs it from the durable timeline.
	Subscribe(jobID string, buf int) (<-chan runmesh.Event, func())
}

// Policy is the slice of the execution policy the API needs. Declared here as
// an interface, like Runtime above, so httpapi does not import the policy
// package and a test can refuse a tool in three lines.
type Policy interface {
	// Allows reports whether this deployment will run the named tool at all.
	Allows(tool string) error
	// Descriptors is the catalogue as it will actually behave here.
	Descriptors() []tools.Descriptor
}

// API holds the handler dependencies. It is unexported state behind a
// http.Handler, so nothing outside this package can reach the store through it.
type API struct {
	store   Store
	tools   tools.Registry
	runtime Runtime
	clock   clock.Clock
	log     *slog.Logger

	limits        runmesh.Limits
	defaults      runmesh.Defaults
	policy        Policy
	planner       Planner
	gatherer      Gatherer
	executionMode tools.ExecutionMode
	maxQueueDepth int
	durable       bool
	storeName     string

	// The live stream and its budget. streams is the admission counter: an
	// open SSE connection costs a goroutine, a store subscription and a socket
	// for as long as a dashboard is left open, so the number of them is capped
	// and the cap is answered with the same 429 the queue-full path already
	// produces rather than with a status invented for this route.
	stream             Streamer
	maxStreams         int
	streamBuffer       int
	streamHeartbeat    time.Duration
	streamPollInterval time.Duration
	streamWriteTimeout time.Duration
	streams            atomic.Int64

	// draining is set by Draining() so that readiness fails as soon as
	// shutdown begins, giving a load balancer time to take this instance out
	// of rotation before the listener actually closes.
	draining chan struct{}
}

// New builds the router with its middleware chain.
func New(d Deps) (http.Handler, *API, error) {
	if d.Store == nil {
		return nil, nil, errors.New("httpapi: a Store is required")
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if len(d.APIKeys) == 0 {
		return nil, nil, errors.New("httpapi: at least one API key is required")
	}
	if d.MaxRequestBytes < 1 {
		d.MaxRequestBytes = 1 << 20
	}
	if d.StoreName == "" {
		d.StoreName = "memory"
	}
	if d.ExecutionMode == "" {
		d.ExecutionMode = tools.ModeInProcess
	}
	// A nil Streamer becomes a no-op rather than an error, exactly as a nil
	// Sandbox does above: the stream route still works, it just waits for the
	// poll ticker instead of being woken by the bus.
	if d.Stream == nil {
		d.Stream = noStreamer{}
	}
	if d.StreamMax < 1 {
		d.StreamMax = 64
	}
	if d.StreamBuffer < 1 {
		d.StreamBuffer = 256
	}
	if d.StreamHeartbeat <= 0 {
		d.StreamHeartbeat = 15 * time.Second
	}
	if d.StreamPollInterval <= 0 {
		d.StreamPollInterval = time.Second
	}
	if d.StreamWriteTimeout <= 0 {
		d.StreamWriteTimeout = 10 * time.Second
	}

	a := &API{
		store:         d.Store,
		tools:         d.Tools,
		runtime:       d.Runtime,
		clock:         d.Clock,
		log:           d.Log.With("component", "httpapi"),
		limits:        d.Limits,
		defaults:      d.Defaults,
		policy:        d.Sandbox,
		planner:       d.Planner,
		gatherer:      d.Gatherer,
		executionMode: d.ExecutionMode,
		maxQueueDepth: d.MaxQueueDepth,
		durable:       d.Durable,
		storeName:     d.StoreName,
		draining:      make(chan struct{}),

		stream:             d.Stream,
		maxStreams:         d.StreamMax,
		streamBuffer:       d.StreamBuffer,
		streamHeartbeat:    d.StreamHeartbeat,
		streamPollInterval: d.StreamPollInterval,
		streamWriteTimeout: d.StreamWriteTimeout,
	}

	mux := http.NewServeMux()
	public := make(map[string]bool)
	for _, rt := range a.routes() {
		mux.Handle(rt.pattern, a.scoped(rt.scope, rt.handler))
		if rt.scope == scopePublic {
			public[pathOf(rt.pattern)] = true
		}
	}

	// POST /api/v1/jobs/{id}/retry is in the plan's API sketch and is
	// deliberately absent from Week 1: "reset the DAG from step X while
	// keeping the outputs that succeeded" is a semantic that deserves its own
	// design, and clone-and-resubmit is a three-line client loop today.
	// Cutting it is scope discipline, not an oversight.

	// Order matters, outermost first. Logger wraps Recover so a panic is
	// turned into a 500 and THEN logged with that status.
	handler := chain(mux,
		RequestID(a.clock),
		Logger(a.log, a.clock, d.Metrics),
		Recover(a.log),
		BodyLimit(d.MaxRequestBytes),
		Auth(d.APIKeys, a.log, func(r *http.Request) bool { return public[r.URL.Path] }),
		CaptureRoute(), // innermost: it must see the request the mux dispatched
	)
	return handler, a, nil
}

// route is one endpoint and the scope it demands.
//
// The table is data rather than eight HandleFunc calls so that "what may this
// key do" is answerable by reading one screen, and so a test can walk it: see
// TestEveryRouteStatesItsScope. Authorisation that lives inside handlers is
// authorisation nobody can audit.
type route struct {
	pattern string
	scope   config.Scope
	handler http.HandlerFunc
}

// scopePublic marks a route that needs no credential at all. It is spelled out
// rather than left as a zero value so that a route with no scope reads as a
// decision instead of an omission.
const scopePublic config.Scope = ""

func (a *API) routes() []route {
	return []route{
		{"POST /api/v1/jobs", config.ScopeJobsWrite, a.createJob},
		{"GET /api/v1/jobs", config.ScopeJobsRead, a.listJobs},
		{"GET /api/v1/jobs/{id}", config.ScopeJobsRead, a.getJob},
		// Cancelling is its own scope: submitting work and stopping somebody
		// else's work are different authorities, and an agent that only ever
		// submits should not be able to halt the fleet.
		{"POST /api/v1/jobs/{id}/cancel", config.ScopeJobsCancel, a.cancelJob},
		{"GET /api/v1/jobs/{id}/events", config.ScopeJobsRead, a.jobEvents},

		// The live timeline. jobs.read and nothing more: it is the same data
		// GET /api/v1/jobs/{id}/events returns, arriving sooner, and a scope
		// that differed from the polling endpoint's would mean a key could read
		// a job's history but not watch it happen — a distinction nobody wants
		// to administer. See ADR 0013 for why this is SSE and not a WebSocket.
		{streamRoute, config.ScopeJobsRead, a.jobStream},

		{"GET /api/v1/tools", config.ScopeJobsRead, a.listTools},

		// The planning endpoints. Both require jobs.write, including the dry
		// run: /plans executes nothing, but it spends model tokens, and a
		// capability that costs money is a write however little it changes.
		{"POST /api/v1/plans", config.ScopeJobsWrite, a.createPlan},
		{"POST /api/v1/goals", config.ScopeJobsWrite, a.createGoal},

		// Metrics is SCOPED, not public, and it is under /api/v1 like
		// everything else here.
		//
		// Scoped, because the justification written two entries below — a load
		// balancer often cannot hold a credential — does not extend to
		// Prometheus, which has an authorization stanza and a credentials_file
		// in scrape_configs. An unauthenticated /metrics exports queue depth,
		// job counts, worker counts and per-tool failure codes to anyone who can
		// reach the port: free reconnaissance, and the only endpoint here that
		// would hand it over.
		//
		// Its own scope rather than jobs.read, because a scrape token that can
		// also list every job and read every event body — tool results
		// included — is a much larger grant than the one Prometheus needs; see
		// config.ScopeMetricsRead.
		//
		// Versioned, because every route in this table is, and because the auth
		// exemption is exact-path: an unversioned /metrics would be the only
		// path in the process whose shape says nothing about which API it
		// belongs to.
		{"GET /api/v1/metrics", config.ScopeMetricsRead, a.metrics},

		// The probes carry no credential: a load balancer must be able to ask
		// whether this process is alive and ready without holding one.
		{"GET /api/v1/health", scopePublic, a.health},
		{"GET /api/v1/ready", scopePublic, a.ready},
	}
}

// scoped rejects a request whose key lacks the scope this route requires.
//
// It runs INSIDE the mux, after Auth has resolved the key, so a 403 names the
// scope that was missing — a caller that has already authenticated learns what
// it would need, which is help rather than disclosure. Authentication failures
// stay deliberately uninformative; see errUnauthenticated.
func (a *API) scoped(s config.Scope, h http.HandlerFunc) http.Handler {
	if s == scopePublic {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := APIKeyFrom(r.Context())
		if !ok || !key.Allows(s) {
			writeError(w, r, a.log, &permissionError{scope: s})
			return
		}
		h(w, r)
	})
}

// pathOf strips the method from a ServeMux pattern. Only the fixed-path public
// routes use it, so it needs no wildcard handling — and matching those paths
// EXACTLY, rather than by prefix, is what stops a future "/api/v1/readyz" from
// inheriting an exemption nobody granted it.
func pathOf(pattern string) string {
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

// Draining makes readiness start failing. Calling it before the HTTP server
// stops accepting gives a load balancer a window to drain this instance
// gracefully instead of watching connections fail.
func (a *API) Draining() {
	select {
	case <-a.draining:
	default:
		close(a.draining)
	}
}

func (a *API) isDraining() bool {
	select {
	case <-a.draining:
		return true
	default:
		return false
	}
}

// listTools is GET /api/v1/tools: the registry, with each tool's contract.
// In Week 5 this same descriptor list is what Gemini is given as its function
// declarations, which is why input_schema is served verbatim.
//
// The limits served are the EFFECTIVE ones — what the operator's execution
// policy will actually grant — not what the tool's descriptor asked for.
// Publishing the request rather than the grant would tell a planner it has 2
// CPUs on a cluster that will give it 250m, and every plan built on that
// number would be wrong in the same direction.
func (a *API) listTools(w http.ResponseWriter, r *http.Request) {
	var descriptors []tools.Descriptor
	if a.policy != nil {
		descriptors = a.policy.Descriptors()
	} else {
		descriptors = a.tools.Descriptors()
		for i := range descriptors {
			descriptors[i].Execution = a.executionMode
		}
	}
	writeJSON(w, a.log, http.StatusOK, toolsResponse{Tools: descriptors})
}

// health is liveness: is this process running at all. It must not depend on
// the store, or a store outage would get the process killed instead of
// getting it marked unready.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.log, http.StatusOK, healthResponse{Status: "ok"})
}

// ready is readiness: should this process receive traffic.
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	resp := readyResponse{
		Status:  "ready",
		Store:   a.storeName,
		Durable: a.durable,
	}
	if a.runtime != nil {
		resp.Workers = a.runtime.Workers()
		resp.Inflight = a.runtime.Inflight()
	}

	if a.isDraining() {
		resp.Status, resp.Reason = "draining", "shutting down"
		writeJSON(w, a.log, http.StatusServiceUnavailable, resp)
		return
	}
	if err := a.store.Ping(r.Context()); err != nil {
		resp.Status, resp.Reason = "unavailable", "store unavailable"
		a.log.Error("readiness check failed", "err", err)
		writeJSON(w, a.log, http.StatusServiceUnavailable, resp)
		return
	}
	depth, err := a.store.QueueDepth(r.Context(), a.clock.Now())
	if err != nil {
		resp.Status, resp.Reason = "unavailable", "store unavailable"
		writeJSON(w, a.log, http.StatusServiceUnavailable, resp)
		return
	}
	resp.QueueDepth = depth
	writeJSON(w, a.log, http.StatusOK, resp)
}
