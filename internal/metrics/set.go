package metrics

import (
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Set is every instrument this binary exports, and this file is the ONE table
// where a metric name, a help string, a label vocabulary or a bucket slice is
// spelled.
//
// That separation is the same one this codebase already applies to Classify and
// to Backoff: the mechanism (a counter on an atomic, a linear bucket scan) is
// general and lives in the other files; the POLICY — what to measure, in what
// unit, cut by which dimensions, with which boundaries — is a table somebody
// can read top to bottom and argue with. Changing what RunMesh measures means
// changing this file and nothing else.
//
// WHY THIS FILE IMPORTS internal/engine. Set implements engine.Observer, which
// is declared by its consumer in internal/engine/observer.go and names
// engine.Stop and runmesh.State in its AttemptOutcome. The alternative is an
// adapter in cmd/server that re-types nine method calls into nine plain-typed
// ones, and that adapter would be a place for the two vocabularies to drift
// apart silently. Set is not a general-purpose metrics library — it is
// RunMesh's telemetry policy, and naming RunMesh's own types in it costs
// nothing. The dependency is strictly one way: internal/engine does not import
// this package and cannot, because it declares the interface itself.
type Set struct {
	clk clock.Clock

	// ---- dispatcher and claiming
	claims        *CounterVec
	claimReq      *Counter
	claimRet      *Counter
	claimDuration *Histogram
	wakeups       *CounterVec
	handoff       *Histogram

	// ---- attempts
	attempts        *CounterVec
	attemptFailures *CounterVec
	attemptDuration *HistogramVec
	toolDuration    *HistogramVec
	dispatchWait    *HistogramVec

	// ---- leases
	heartbeats    *CounterVec
	leasesExpired *CounterVec
	sweeps        *CounterVec
	sweepDuration *Histogram

	// ---- adaptive concurrency
	concurrencyAdjustments *CounterVec

	// ---- store
	storeDuration *HistogramVec
	storeErrors   *CounterVec

	// ---- http
	httpRequests *CounterVec
	httpDuration *HistogramVec
	httpBytes    *CounterVec

	// ---- scrape-time sources, bound after the engine, store and API exist
	runtimeSrc holder[Runtime]
	storeSrc   holder[StoreSource]
	streamSrc  holder[Streams]
	depth      *depthCache

	// streamingRoutes are the route patterns whose connections are long-lived,
	// and therefore the ones whose elapsed time is a connection lifetime rather
	// than a service latency. See RequestFinished.
	streamingRoutes map[string]bool

	// statusNames maps an HTTP status onto its label value without allocating
	// on the request path. The vocabulary is closed for the same reason every
	// other one here is: this API's own writeError switch decides the whole set.
	statusNames map[int]string
}

// Compile-time proof that the Set is the engine's observer. Without it, a
// method added to engine.Observer would fail at the engine.Deps literal in
// cmd/server/run.go, which is a confusing place to learn that this file is the
// one that has to change.
var _ engine.Observer = (*Set)(nil)

// Vocabulary is the closed label alphabet, gathered in run() from the places
// that already own it — the tool registry and the HTTP route table. Passing it
// in rather than importing those packages is what keeps this package free of
// internal/tools and internal/httpapi, and what makes the series bound a
// property of the wiring rather than of a hard-coded list that can go stale.
type Vocabulary struct {
	Tools  []string
	Routes []string
	// StreamingRoutes is the subset of Routes whose handlers hold a connection
	// open. It comes from httpapi.StreamingRoutePatterns() for the same reason
	// Routes comes from httpapi.RoutePatterns(): the API owns the fact, so a
	// route that becomes long-lived cannot be forgotten here.
	StreamingRoutes []string
	// Version and GoVersion label runmesh_build_info. Empty values are fine and
	// render as empty label values; they are not a reason to fail a boot — a
	// server that refused to start because -ldflags was forgotten would be
	// trading availability for the spelling of an informational label. Note what
	// an empty value actually costs on the other side: Prometheus treats an empty
	// label value as equivalent to the label being absent, so an unstamped build
	// is stored as runmesh_build_info{go_version="..."} and cannot be grouped by
	// version. labels.go argues the decision in full.
	Version   string
	GoVersion string
}

// Bucket boundaries. All three are compile-time constants, which is half of why
// `le` can never change its formatting between two scrapes — see appendFloat.
var (
	// bucketsStep spans an in-process echo (single-digit milliseconds) through
	// a container cold start (seconds) to the ten-minute step timeout ceiling.
	// 30 is on a boundary deliberately: the default lease TTL is 30s, and the
	// question after an incident is how many attempts outran their lease.
	bucketsStep = []float64{.005, .025, .1, .5, 1, 2.5, 5, 10, 30, 60, 300, 600}

	// bucketsWait is for queueing: the time a step spends between being claimed
	// and being executed. Finer at the bottom than bucketsStep because a
	// millisecond of hand-off latency and a second of it are different
	// diagnoses.
	bucketsWait = []float64{.001, .005, .025, .1, .5, 1, 2.5, 5, 10, 30, 60}

	// bucketsIO is for a single round trip to the store or a single HTTP
	// request. Anything past five seconds here is already a timeout, so the tail
	// buys nothing that the +Inf bucket does not.
	bucketsIO = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

	// bucketsSched re-buckets Go's own scheduling latency histogram. See
	// runtimecol.go for why re-bucketing rather than passing Go's buckets
	// through.
	bucketsSched = []float64{1e-6, 1e-5, 1e-4, 1e-3, 1e-2, 1e-1, 1}
)

// Label vocabularies that are not derived from the wiring.
var (
	claimOutcomes  = []string{"ok", "empty", "error"}
	wakeReasons    = []string{"hint", "tick", "shutdown"}
	heartbeatOut   = []string{"ok", "lease_lost", "cancel", "error"}
	sweepOutcomes  = []string{"ok", "error"}
	reclaimedState = []string{runmesh.Queued.String(), runmesh.Failed.String()}
	sizeDirections = []string{"grow", "shrink"}

	storeOps = []string{
		"claim", "start", "heartbeat", "finish", "release", "expire_leases", "queue_depth",
	}
	storeErrorKinds = []string{"lease_lost", "conflict", "not_found", "duplicate", "closed", otherValue}

	// errorCodes is every code the RUNTIME guarantees. A tool may emit its own,
	// and those resolve to "other" — which is the one genuinely open axis in
	// this whole vocabulary, and exactly what "other" exists for.
	errorCodes = []string{
		runmesh.CodeTimeout, runmesh.CodeCancelled, runmesh.CodeLeaseLost,
		runmesh.CodeWorkloadLost, runmesh.CodeTaskExited, runmesh.CodeTaskOOMKilled,
		runmesh.CodeAbandoned, runmesh.CodePanic, runmesh.CodeEnginePanic,
		runmesh.CodeUnclassified, runmesh.CodeContractBroken, runmesh.CodeToolUnknown,
		runmesh.CodeOutputTooLarge, runmesh.CodeDepFailed, runmesh.CodeShutdown,
		runmesh.CodeToolDenied, runmesh.CodeToolNotSandboxed, runmesh.CodeNetworkDenied,
		runmesh.CodePolicyViolation,
	}

	// httpStatuses is every status internal/httpapi's classifyError switch and
	// its handlers can produce, plus the two the mux itself answers. A status
	// outside the set lands in "other", which is a bug worth seeing rather than
	// a new series.
	httpStatuses = []int{200, 201, 202, 400, 401, 403, 404, 405, 409, 413, 422, 429, 500, 501, 503}
)

// NewSet registers every metric and returns the object the engine, the API and
// the store decorator all observe through.
//
// Registration panics on a bad name, a duplicate, a missing help string or a
// non-ascending bucket slice, which means all of those are boot failures in
// run() rather than a dashboard that is quietly missing a panel on a running
// server.
func NewSet(r *Registry, v Vocabulary) *Set {
	tools := withOther(v.Tools)
	routes := withOther(v.Routes)
	// routeOf() answers "unmatched" for a request the mux did not route, and
	// that is a real label value rather than a mistake: a 404 flood is a thing
	// an operator wants to see, and giving it its own cell keeps it out of the
	// per-route series.
	routes = append(routes, "unmatched")

	states := make([]string, 0, len(runmesh.AllStates))
	for _, s := range runmesh.AllStates {
		states = append(states, s.String())
	}
	stops := []string{
		engine.StopNone.String(), engine.StopTimeout.String(), engine.StopCancel.String(),
		engine.StopLost.String(), engine.StopShutdown.String(),
	}
	statuses := make([]string, 0, len(httpStatuses))
	statusNames := make(map[int]string, len(httpStatuses))
	for _, code := range httpStatuses {
		name := itoa(code)
		statuses = append(statuses, name)
		statusNames[code] = name
	}

	streaming := make(map[string]bool, len(v.StreamingRoutes))
	for _, rt := range v.StreamingRoutes {
		streaming[rt] = true
	}
	// The duration histogram gets its OWN, narrower route vocabulary: every
	// route except the long-lived ones. Leaving them in and merely declining to
	// observe them would export a series pinned at zero for ever, which reads
	// as "nothing ever hits this route" rather than as "this route is measured
	// elsewhere" — and a permanently-empty series is how a reader concludes the
	// instrumentation is broken. RequestFinished has the matching guard, so a
	// streaming route never resolves to the `other` child either.
	timedRoutes := make([]string, 0, len(routes))
	for _, rt := range routes {
		if !streaming[rt] {
			timedRoutes = append(timedRoutes, rt)
		}
	}

	s := &Set{clk: r.clk, statusNames: statusNames, streamingRoutes: streaming}

	// ------------------------------------------------------------ dispatcher
	s.claims = r.CounterVec(Opts{
		Name: "runmesh_claims_total",
		Help: "Claim round trips the dispatcher made, by outcome (ok, empty, error).",
	}, Label{Name: "outcome", Values: claimOutcomes})

	// Requested and returned are two counters rather than one gauge because the
	// interesting quantity is the RATIO over time: a returned/requested that
	// falls towards zero is a pool bigger than the queue, and one pinned at 1 is
	// a queue the pool cannot keep up with. A gauge would only ever show the
	// last claim.
	s.claimReq = r.Counter(Opts{
		Name: "runmesh_claim_leases_requested_total",
		Help: "Leases the dispatcher asked the store for, summed over every claim.",
	})
	s.claimRet = r.Counter(Opts{
		Name: "runmesh_claim_leases_returned_total",
		Help: "Leases the store actually returned, summed over every claim.",
	})
	s.claimDuration = r.Histogram(Opts{
		Name: "runmesh_claim_duration_seconds",
		Help: "Time one Store.Claim round trip took.",
	}, bucketsIO)

	// This is what says whether Store.Ready() hints earn their keep against
	// plain polling: a hint/tick ratio near zero means the hint channel is
	// carrying nothing and the poll interval is the whole latency story.
	s.wakeups = r.CounterVec(Opts{
		Name: "runmesh_dispatcher_wakeups_total",
		Help: "Times the dispatcher woke from an idle park, by what woke it.",
	}, Label{Name: "on", Values: wakeReasons})

	// Unlabelled on purpose: pool saturation is a property of the pool, not of
	// any one tool, and a per-tool cut would only ever be read summed.
	s.handoff = r.Histogram(Opts{
		Name: "runmesh_dispatch_handoff_seconds",
		Help: "Time between a step being claimed and a worker taking it off the lease channel.",
	}, bucketsWait)

	// ---------------------------------------------------------------- steps
	//
	// There is deliberately NO runmesh_step_retries_scheduled_total: it is
	// exactly attempts_total{state="RETRYING"}, and a second counter for a
	// number that is already derivable is a second thing to keep in agreement.
	s.attempts = r.CounterVec(Opts{
		Name: "runmesh_step_attempts_total",
		Help: "Attempts that reached a decided outcome, by tool, resulting state and why the step stopped.",
	},
		Label{Name: "tool", Values: tools},
		Label{Name: "state", Values: states},
		Label{Name: "stop", Values: stops},
	)
	s.attemptFailures = r.CounterVec(Opts{
		Name: "runmesh_step_attempt_failures_total",
		Help: "Attempts that carried an error, by tool and error code. Codes outside the runtime's own vocabulary are counted as other.",
	},
		Label{Name: "tool", Values: tools},
		Label{Name: "code", Values: errorCodes},
	)

	// attempt_duration and tool_duration are both here on purpose, and the
	// DIFFERENCE between them is the point: attempt duration brackets
	// Store.Start, every heartbeat and the settle write as well as the tool,
	// while tool duration brackets Executor.Execute and nothing else. When a
	// p95 rises, the gap between the two is what says whether the engine or the
	// tool got slower — which is the single question Week 6 exists to answer.
	s.attemptDuration = r.HistogramVec(Opts{
		Name: "runmesh_step_attempt_duration_seconds",
		Help: "Wall time from a worker starting an attempt to writing its outcome, by tool.",
	}, bucketsStep, Label{Name: "tool", Values: tools})
	s.toolDuration = r.HistogramVec(Opts{
		Name: "runmesh_tool_duration_seconds",
		Help: "Wall time inside Executor.Execute only, by tool.",
	}, bucketsStep, Label{Name: "tool", Values: tools})

	// Under Kubernetes this is pod pending time, which is why it is per-tool:
	// one tool pulling a cold image is invisible in an aggregate.
	s.dispatchWait = r.HistogramVec(Opts{
		Name: "runmesh_step_dispatch_wait_seconds",
		Help: "Time from a step being claimed to it actually starting, by tool.",
	}, bucketsWait, Label{Name: "tool", Values: tools})

	// --------------------------------------------------------------- leases
	s.heartbeats = r.CounterVec(Opts{
		Name: "runmesh_heartbeats_total",
		Help: "Heartbeat round trips, by tool and what the store answered.",
	},
		Label{Name: "tool", Values: tools},
		Label{Name: "outcome", Values: heartbeatOut},
	)
	s.leasesExpired = r.CounterVec(Opts{
		Name: "runmesh_leases_expired_total",
		Help: "Leases the reconciler reclaimed, by the state the step was moved to (QUEUED means it will retry, FAILED means its budget was spent).",
	}, Label{Name: "new_state", Values: reclaimedState})
	s.sweeps = r.CounterVec(Opts{
		Name: "runmesh_reconcile_sweeps_total",
		Help: "Expired-lease sweeps the reconciler ran, by outcome.",
	}, Label{Name: "outcome", Values: sweepOutcomes})
	s.sweepDuration = r.Histogram(Opts{
		Name: "runmesh_reconcile_duration_seconds",
		Help: "Time one Store.ExpireLeases sweep took.",
	}, bucketsIO)

	// ------------------------------------------------------ adaptive concurrency
	//
	// Not a gauge: the LEVEL is already runmesh_workers (see gauges.go), and a
	// gauge fed from the same events this counts would be the two-paths-to-
	// one-number drift this file's own doc warns about. What this adds is the
	// thing a level cannot show — the RATE a real move happens at, cut by
	// direction — which is the hint/tick split runmesh_dispatcher_wakeups_total
	// already makes for the same reason.
	s.concurrencyAdjustments = r.CounterVec(Opts{
		Name: "runmesh_concurrency_adjustments_total",
		Help: "Adaptive-sizing decisions that actually moved the worker pool, by direction. runmesh_workers is the level this is the rate of.",
	}, Label{Name: "direction", Values: sizeDirections})

	// ---------------------------------------------------------------- store
	s.storeDuration = r.HistogramVec(Opts{
		Name: "runmesh_store_operation_duration_seconds",
		Help: "Time one store call took, by operation.",
	}, bucketsIO, Label{Name: "op", Values: storeOps})
	s.storeErrors = r.CounterVec(Opts{
		Name: "runmesh_store_operation_errors_total",
		Help: "Store calls that returned an error, by operation and sentinel.",
	},
		Label{Name: "op", Values: storeOps},
		Label{Name: "kind", Values: storeErrorKinds},
	)

	// ----------------------------------------------------------------- http
	//
	// The label is the matched ROUTE and not route plus method, because
	// routeOf() already returns the pattern WITH the method in it
	// ("GET /api/v1/jobs/{id}"). A separate method label would double the
	// series and carry no information the route does not already have.
	s.httpRequests = r.CounterVec(Opts{
		Name: "runmesh_http_requests_total",
		Help: "HTTP requests served, by matched route pattern and status.",
	},
		Label{Name: "route", Values: routes},
		Label{Name: "status", Values: statuses},
	)
	//
	// STREAMING ROUTES ARE EXCLUDED FROM THE DURATION HISTOGRAM, and the help
	// string says so because a reader finding an empty series is owed the
	// reason. The elapsed time this histogram is fed comes from Logger's
	// deferred func, which runs when the handler RETURNS — for
	// GET /api/v1/jobs/{id}/stream that is when the dashboard is closed, so the
	// observation would be a tab lifetime measured in hours, landing in the same
	// buckets as a four-millisecond readiness probe. One open dashboard would
	// move the p99 of the whole API. Nothing fails; the graph is simply a lie,
	// which is the worst kind of instrumentation bug. The count of those
	// connections is exported as runmesh_http_streams_active instead.
	s.httpDuration = r.HistogramVec(Opts{
		Name: "runmesh_http_request_duration_seconds",
		Help: "Time to serve one HTTP request, by matched route pattern. Long-lived streaming routes are excluded: their elapsed time is a connection lifetime, not a service latency, and runmesh_http_streams_active reports them instead.",
	}, bucketsIO, Label{Name: "route", Values: timedRoutes})
	s.httpBytes = r.CounterVec(Opts{
		Name: "runmesh_http_response_bytes_total",
		Help: "Response body bytes written, by matched route pattern.",
	}, Label{Name: "route", Values: routes})

	// --------------------------------------------------- scrape-time gauges
	s.depth = newDepthCache(r.clk, &s.storeSrc)
	s.registerGauges(r)

	// ----------------------------------------------------------- build info
	//
	// The Prometheus idiom: a gauge pinned at 1 whose LABELS are the payload,
	// so `runmesh_build_info` joined against any other series tells a dashboard
	// which build produced it. The vocabulary is one value per label, which is
	// the degenerate but entirely real case of a closed set.
	build := r.GaugeVec(Opts{
		Name: "runmesh_build_info",
		Help: "Always 1. The labels carry the build: the version stamped at link time and the Go toolchain.",
	},
		Label{Name: "version", Values: []string{v.Version}},
		Label{Name: "go_version", Values: []string{v.GoVersion}},
	)
	build.With(v.Version, v.GoVersion).Set(1)

	RegisterRuntime(r)
	return s
}

// ------------------------------------------------------------ engine.Observer
//
// Every method below runs on an ENGINE goroutine — the dispatcher, or a worker
// holding one of the pool's capacity tokens. Each one is a handful of atomic
// operations and at most one map read under a read lock, and that is a
// contract rather than an accident: see the doc comment on engine.Observer.

// Claimed records one Store.Claim round trip.
func (s *Set) Claimed(requested, returned int, d time.Duration, err error) {
	outcome := "ok"
	switch {
	case err != nil:
		outcome = "error"
	case returned == 0:
		outcome = "empty"
	}
	s.claims.With(outcome).Inc()
	s.claimDuration.ObserveDuration(d)
	if requested > 0 {
		s.claimReq.Add(uint64(requested))
	}
	if returned > 0 {
		s.claimRet.Add(uint64(returned))
	}
}

// Dispatched records a lease reaching a worker.
func (s *Set) Dispatched(tool string, queueWait time.Duration) {
	s.handoff.ObserveDuration(queueWait)
}

// DispatcherIdle records what woke the dispatcher out of an idle park.
func (s *Set) DispatcherIdle(wokeOn string) { s.wakeups.With(wokeOn).Inc() }

// AttemptStarted records a step moving from claimed to running.
func (s *Set) AttemptStarted(tool string, dispatchWait time.Duration) {
	s.dispatchWait.With(tool).ObserveDuration(dispatchWait)
}

// ToolExecuted records Executor.Execute alone.
func (s *Set) ToolExecuted(tool string, d time.Duration) {
	s.toolDuration.With(tool).ObserveDuration(d)
}

// AttemptSettled records a decided outcome. It is called on every exit path of
// settle, including the ones where nothing was persisted, because the number
// this counts is what the worker DECIDED — which is the same number the
// "step settled" log line reports, and keeping the two in agreement is worth
// more than excluding a store write that failed (store_operation_errors_total
// already counts that, by operation).
func (s *Set) AttemptSettled(o engine.AttemptOutcome) {
	s.attempts.With(o.Tool, o.State.String(), o.Stop.String()).Inc()
	s.attemptDuration.With(o.Tool).ObserveDuration(o.Duration)
	if o.Code != "" {
		s.attemptFailures.With(o.Tool, o.Code).Inc()
	}
}

// Heartbeat records one heartbeat round trip and what it returned.
func (s *Set) Heartbeat(tool, outcome string) { s.heartbeats.With(tool, outcome).Inc() }

// LeaseReclaimed records one reclaimed lease, cut by whether the step will
// retry or has spent its budget.
func (s *Set) LeaseReclaimed(newState runmesh.State) {
	s.leasesExpired.With(newState.String()).Inc()
}

// SweepFinished records one reconciler sweep.
func (s *Set) SweepFinished(reclaimed int, d time.Duration, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.sweeps.With(outcome).Inc()
	s.sweepDuration.ObserveDuration(d)
}

// ConcurrencyAdjusted records one adaptive-sizing decision. The engine only
// ever calls this when the target actually moved (see engine.Observer's
// doc), so there is no "unchanged" direction to label here.
func (s *Set) ConcurrencyAdjusted(from, to int) {
	direction := "grow"
	if to < from {
		direction = "shrink"
	}
	s.concurrencyAdjustments.With(direction).Inc()
}

// ------------------------------------------------------------- other seams

// StoreOperation records one store call. kind is "" on success and otherwise
// the sentinel the error matched; cmd/server/store_metrics.go does the
// errors.Is discrimination, because that is the only file in the codebase that
// names both engine.Store and httpapi.Store.
func (s *Set) StoreOperation(op string, d time.Duration, kind string) {
	s.storeDuration.With(op).ObserveDuration(d)
	if kind != "" {
		s.storeErrors.With(op, kind).Inc()
	}
}

// RequestFinished satisfies httpapi.HTTPObserver. It is called from inside the
// EXISTING deferred func in Logger, from the same recorded status, byte count
// and elapsed time the log line uses — which is what makes it structurally
// impossible for the log line and the metric to disagree about one request.
func (s *Set) RequestFinished(route string, status int, bytes int64, d time.Duration) {
	name, ok := s.statusNames[status]
	if !ok {
		name = otherValue
	}
	s.httpRequests.With(route, name).Inc()
	// The exclusion, applied. Everything else about a streaming request is
	// ordinary and is counted normally — it was one request, it had a status,
	// it wrote bytes — but its ELAPSED TIME is a connection lifetime, because
	// the deferred func that produced it runs when the handler returns and a
	// stream handler returns when somebody closes a browser tab. Observing that
	// alongside a four-millisecond readiness probe is how one open dashboard
	// moves the p99 of the whole API. runmesh_http_streams_active reports those
	// connections instead.
	if !s.streamingRoutes[route] {
		s.httpDuration.With(route).ObserveDuration(d)
	}
	if bytes > 0 {
		s.httpBytes.With(route).Add(uint64(bytes))
	}
}

// withOther copies a vocabulary and appends the catch-all, so a caller cannot
// hand us a slice it later mutates and so "other" is present even for a
// vocabulary that forgot it.
func withOther(values []string) []string {
	out := make([]string, 0, len(values)+1)
	out = append(out, values...)
	for _, v := range out {
		if v == otherValue {
			return out
		}
	}
	return append(out, otherValue)
}

// itoa renders a small non-negative int without importing strconv into the
// policy table. Statuses are three digits; anything else is a programming
// error and renders as "other" before it reaches here.
func itoa(n int) string {
	if n < 0 {
		return otherValue
	}
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
