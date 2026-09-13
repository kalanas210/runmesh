package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/tools"
)

// This file is this package's replacement for client_golang's promlint, and
// ADR 0012 concedes that losing promlint is the real cost of not taking the
// library. What comes back is better in one specific way: the NAME rules also
// run at registration, so an illegal name is a panic in run() at boot rather
// than a red build somebody discovers after the fact. These tests assert the
// rules against the real, fully wired metric set, which is the thing promlint
// would have been pointed at anyway.

// productionSet builds the registry exactly as run() does, from the real tool
// registry and the real route table. Anything else would be testing a
// vocabulary that does not ship.
func productionSet(t *testing.T) (*Registry, *Set) {
	t.Helper()
	r := NewRegistry(clock.NewFake(epoch))
	registry := tools.Builtins(tools.Options{
		EnableTestTools: true,
		TaskImage:       "runmesh/task:dev",
		PythonImage:     "runmesh/python:dev",
	})
	s := NewSet(r, Vocabulary{
		Tools:           registry.Names(),
		Routes:          httpapi.RoutePatterns(),
		StreamingRoutes: httpapi.StreamingRoutePatterns(),
		Version:         "test",
		GoVersion:       "go1.26.0",
	})
	return r, s
}

// TestEveryCounterEndsInTotal. The suffix is what tells a reader — and a
// linter, and a human writing a query — that rate() is the right operator.
func TestEveryCounterEndsInTotal(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	for _, name := range r.Names() {
		if r.typeOf(name) != typeCounter {
			continue
		}
		if !strings.HasSuffix(name, "_total") {
			t.Errorf("counter %q does not end in _total", name)
		}
	}
}

// TestNoGaugeEndsInTotal is the same rule from the other side, and it is the
// one that actually catches something: _total on a gauge invites rate() over a
// series that can DECREASE, and rate() over a decrease is read as a counter
// reset, which produces a large positive number out of nowhere.
func TestNoGaugeEndsInTotal(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	for _, name := range r.Names() {
		if r.typeOf(name) != typeGauge {
			continue
		}
		if strings.HasSuffix(name, "_total") {
			t.Errorf("gauge %q ends in _total; rate() over it would read every "+
				"decrease as a counter reset", name)
		}
	}
}

// TestEveryDurationIsSeconds. Prometheus base units are seconds and bytes, and
// a metric named _milliseconds is not merely unidiomatic — it is the mistake
// that survives review, because the series exists, the graph is smooth, and
// every threshold on it is wrong by a factor of a thousand.
//
// The structural half of this rule is in histogram.go: ObserveDuration is the
// only form the engine uses, so the seconds conversion lives in one place and
// no call site can export milliseconds under a _seconds name.
func TestEveryDurationIsSeconds(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	banned := []string{"_ms", "_millis", "_milliseconds", "_micros", "_nanos", "_ns"}
	for _, name := range r.Names() {
		for _, suffix := range banned {
			if strings.HasSuffix(name, suffix) {
				t.Errorf("%q is not in seconds; Prometheus base units are seconds and bytes", name)
			}
		}
		// Every histogram in this binary measures a duration, so every one of
		// them must say so.
		if r.typeOf(name) == typeHistogram && !strings.HasSuffix(name, "_seconds") {
			t.Errorf("histogram %q does not end in _seconds", name)
		}
	}
}

// TestSecondsCountersAreNotBridgedThroughIntegers is the rule that catches a
// metric which looks entirely healthy and is not.
//
// Counter and counterFunc are integral on purpose — counter.go argues it: this
// runtime counts events, and a float counter invites somebody to add 0.1 to
// something whose rate() is then meaningless. A cumulative DURATION is the
// exception, and go_cpu_gc_seconds_total was the proof: it was registered with
// CounterFunc over /cpu/classes/gc/total:cpu-seconds, so
// uint64(src.float(...)) threw away every fractional CPU-second and the series
// read a flat 0 on any process that had not spent a whole second collecting
// garbage. The name was right, the TYPE line was right, the help string
// promised CPU seconds, and the panel was a lie.
//
// A type switch is the honest test here because the truncation is invisible in
// the rendered body — 0 is a legal value for a counter. What is checkable is
// the BRIDGE: a _seconds_total family must not be sourced through an accessor
// whose type cannot carry a fraction.
func TestSecondsCountersAreNotBridgedThroughIntegers(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := 0
	for _, f := range r.families {
		if f.typ != typeCounter || !strings.HasSuffix(f.name, "_seconds_total") {
			continue
		}
		seen++
		switch f.c.(type) {
		case counterFunc, *Counter:
			t.Errorf("%q is a cumulative duration sourced through a uint64 counter; "+
				"every fractional second is floored away and the series reads 0 "+
				"until a whole second has accumulated. Register it with "+
				"CounterFloatFunc — the exposition TYPE stays `counter`, so rate() "+
				"is unaffected.", f.name)
		}
	}
	if seen == 0 {
		t.Fatal("no _seconds_total counter is registered; this rule would be vacuous " +
			"and would not have caught go_cpu_gc_seconds_total")
	}
}

// TestEveryMetricHasHelpAndAName re-checks at the registry level what
// registration already panics on, so the rule is asserted rather than merely
// relied upon.
func TestEveryMetricHasHelpAndAName(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	body := render(t, r)
	families, err := parseExposition(body)
	if err != nil {
		t.Fatalf("the fully wired registry emits something unparseable: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("the production registry is empty")
	}
	for name, f := range families {
		if err := validName(name); err != nil {
			t.Errorf("%v", err)
		}
		if strings.TrimSpace(f.help) == "" {
			t.Errorf("%q has an empty help string", name)
		}
		if f.typ == "" {
			t.Errorf("%q has no TYPE line", name)
		}
	}
	// Nothing is named process_* except the one thing that is exact. See the
	// header comment in runtimecol.go: a series named after something it is not
	// is worse than an absent series.
	for name := range families {
		if strings.HasPrefix(name, "process_") && name != "process_start_time_seconds" {
			t.Errorf("%q approximates a process metric from the Go runtime; "+
				"recover it from /proc or omit it, do not fake it", name)
		}
	}
}

// maxSeries is the ceiling this binary's exposition is allowed to reach.
//
// The number is a budget, not a measurement: the real bound with the shipped
// vocabularies is roughly half of it, and the headroom is there so adding one
// tool or one route is not a test failure. Breaching it IS meant to be a
// failure — it means a label vocabulary grew by an order of magnitude, and the
// question to answer before raising this constant is which dimension grew and
// whether it is still closed.
const maxSeries = 3000

// TestSeriesBudget is the cardinality proof, and it is the one claim in ADR
// 0012 that no client_golang deployment can make.
//
// Because every label vocabulary is closed at wiring time, the maximum number
// of time series this process can ever export is a constant computable at boot
// — so "our series count is bounded" is a number in a test rather than an
// assurance. GetMetricWithLabelValues would mint a series for any string it was
// ever handed; there is no equivalent of this test on the other side of that
// decision.
func TestSeriesBudget(t *testing.T) {
	t.Parallel()
	r, _ := productionSet(t)

	bound := r.SeriesUpperBound()
	if bound <= 0 {
		t.Fatalf("SeriesUpperBound = %d", bound)
	}
	if bound > maxSeries {
		t.Fatalf("the series upper bound is %d, over the budget of %d. "+
			"Before raising the budget, find which label vocabulary grew and "+
			"confirm it is still closed at wiring time.", bound, maxSeries)
	}
	t.Logf("series upper bound: %d of %d", bound, maxSeries)
}

// TestTheBoundIsActuallyAnUpperBound: no sequence of observations, however
// hostile, can make the rendered body carry more series than the bound
// promises. This is the half that makes the budget a proof rather than an
// arithmetic exercise — a bound computed from the declared vocabularies is only
// meaningful if out-of-vocabulary values genuinely cannot create a cell.
func TestTheBoundIsActuallyAnUpperBound(t *testing.T) {
	t.Parallel()
	r, s := productionSet(t)
	bound := r.SeriesUpperBound()

	// Every one of these is a value nobody declared, including the shapes a
	// real cardinality incident takes: a job id, a step id, an unbounded tool
	// name and a tool-supplied error code.
	for i := range 500 {
		id := "job_" + itoa(i)
		s.Heartbeat(id, id)
		s.DispatcherIdle(id)
		s.StoreOperation(id, 0, id)
		s.RequestFinished(id, 599+i, 1, 0)
		s.LeaseReclaimed(0)
	}

	body := render(t, r)
	families, err := parseExposition(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	total := 0
	for _, f := range families {
		total += len(f.samples)
	}
	if total > bound {
		t.Fatalf("500 hostile label values produced %d series against a promised "+
			"bound of %d", total, bound)
	}

	// And the rejections were counted rather than swallowed, because a
	// cardinality mistake has to be VISIBLE — the whole trade is that it
	// surfaces as a metric instead of as an OOM.
	rejected, ok := families["runmesh_metrics_label_rejected_total"]
	if !ok {
		t.Fatal("runmesh_metrics_label_rejected_total is missing")
	}
	var sum float64
	for _, smp := range rejected.samples {
		sum += smp.value
	}
	if sum == 0 {
		t.Fatal("2500 out-of-vocabulary label values were silently accepted as 'other' " +
			"with nothing counted; the mistake would be invisible")
	}
}

// TestStreamingRoutesAreNotTimed is the trap this whole exclusion exists for,
// held shut.
//
// The duration histogram is fed from the deferred func inside httpapi's Logger,
// which runs when the HANDLER RETURNS. A stream handler returns when somebody
// closes a browser tab, so its observation is a connection lifetime — minutes,
// or hours — landing in the same buckets as a four-millisecond readiness probe.
// One open dashboard moves the p99 of the entire API, and nothing fails to say
// so: the graph is simply a lie, which is the worst kind of instrumentation
// bug. Everything ELSE about a streaming request is ordinary and is still
// counted; only the elapsed time is refused.
func TestStreamingRoutesAreNotTimed(t *testing.T) {
	t.Parallel()

	r, s := productionSet(t)

	streaming := httpapi.StreamingRoutePatterns()
	if len(streaming) == 0 {
		t.Fatal("the API reports no streaming routes; this exclusion would be vacuous")
	}

	const normal = "GET /api/v1/jobs/{id}/events"
	s.RequestFinished(normal, 200, 512, 4*time.Millisecond)
	for _, route := range streaming {
		// An hour-long connection, which is what a dashboard left open is.
		s.RequestFinished(route, 200, 4096, time.Hour)
	}

	body := render(t, r)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "runmesh_http_request_duration_seconds") {
			continue
		}
		for _, route := range streaming {
			if strings.Contains(line, `route="`+route+`"`) {
				t.Errorf("the duration histogram exports a series for the streaming "+
					"route %q:\n  %s", route, line)
			}
		}
	}

	// The request itself is still counted, and its bytes are still counted. The
	// exclusion is surgical: one observation, not one route.
	for _, route := range streaming {
		if !strings.Contains(body, `runmesh_http_requests_total{route="`+route+`",status="200"} 1`) {
			t.Errorf("the streaming route %q was not counted as a request", route)
		}
		if !strings.Contains(body, `runmesh_http_response_bytes_total{route="`+route+`"} 4096`) {
			t.Errorf("the streaming route %q had its response bytes dropped", route)
		}
	}

	// And the excluded route must NOT have fallen through to the shared "other"
	// child, which would put the hour back into the histogram under a different
	// label and bump the cardinality-mistake counter on every single connection.
	if strings.Contains(body, `runmesh_metrics_label_rejected_total{metric="runmesh_http_request_duration_seconds"} 0`) {
		return // the series exists and is zero, which is what we want
	}
	if strings.Contains(body, `runmesh_metrics_label_rejected_total{metric="runmesh_http_request_duration_seconds"}`) {
		t.Errorf("a streaming route was rejected as an unknown label value rather "+
			"than skipped:\n%s", body)
	}
}
