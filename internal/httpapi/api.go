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
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
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
	// StoreName appears in the readiness body: "memory" now, "postgres" later.
	StoreName string
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
	maxQueueDepth int
	durable       bool
	storeName     string

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

	a := &API{
		store:         d.Store,
		tools:         d.Tools,
		runtime:       d.Runtime,
		clock:         d.Clock,
		log:           d.Log.With("component", "httpapi"),
		limits:        d.Limits,
		defaults:      d.Defaults,
		maxQueueDepth: d.MaxQueueDepth,
		durable:       d.Durable,
		storeName:     d.StoreName,
		draining:      make(chan struct{}),
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
		Logger(a.log, a.clock),
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
		{"GET /api/v1/tools", config.ScopeJobsRead, a.listTools},

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
func (a *API) listTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.log, http.StatusOK, toolsResponse{Tools: a.tools.Descriptors()})
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
