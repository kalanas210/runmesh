// Package metrics is RunMesh's telemetry: counters, gauges and
// explicit-bucket histograms built on atomics, and the Prometheus text
// exposition format written by hand.
//
// WHY BY HAND. ADR 0012 argues it in full; the short version is the test ADR
// 0007 set. What can only github.com/prometheus/client_golang do? Serialise a
// format specified in about one page, over three data structures that are
// roughly 250 lines of atomic.Uint64 — no protocol, no negotiation, no
// cryptography. And can it be contained? No: prometheus.Counter,
// prometheus.Labels and *prometheus.Registry would appear in engine, in
// httpapi and in cmd/server, which is the same objection 0007 raised when it
// rejected pgxpool. pgx earned its place because swapping it is changing one
// string; a metrics client cannot make that claim.
//
// THE CONTAINMENT PROPERTY. This is the only package in the repository that
// knows the exposition format exists. Every consumer sees an interface it
// declared itself: engine.Observer, httpapi.Gatherer, httpapi.HTTPObserver.
// Nothing here registers itself in an init(), and there is no default
// registry to reach for — a *Registry is constructed in cmd/server/run.go and
// injected downwards like every other dependency.
//
// FAILURE MODE. A bug in this writer makes /metrics return a body Prometheus
// refuses to parse: `up` goes to 0 and a scrape error appears in Prometheus's
// own log within one interval. It has exactly one possible cause, it is
// visible immediately, and it is on a read-only endpoint that cannot affect a
// claim, a lease, a heartbeat or an outcome. Nothing in the execution path
// sits on top of it.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/kalanas210/runmesh/internal/clock"
)

// Opts is the identity of one metric family: the name Prometheus will key on,
// and the help string an operator reads in a dashboard's metric browser.
//
// There is no Namespace/Subsystem/ConstLabels triple as in client_golang. Every
// name in this process is spelled out in full in set.go, which is the one table
// where a metric name, a help string, a label vocabulary or a bucket slice is
// decided — the same separation of policy from mechanism this codebase applies
// to Classify and Backoff.
type Opts struct {
	Name string
	Help string
}

// Registry holds every metric family and renders them.
//
// The mutex is taken in write mode only during wiring, when run() constructs
// the Set, and in read mode on every scrape. Registration after boot is legal
// but nothing does it: the whole point of a closed vocabulary is that the shape
// of the exposition is fixed before the listener opens.
type Registry struct {
	mu       sync.RWMutex
	families []*family
	names    map[string]bool
	// vecs is the registration order of every labelled family, kept sorted by
	// metric name. It exists so runmesh_metrics_label_rejected_total can be one
	// family with one sample per labelled metric, with a vocabulary that is
	// closed by construction: it is exactly the set of vecs that were
	// registered.
	vecs []*vec

	clk clock.Clock
	// scrape times the PREVIOUS render. A histogram cannot contain the duration
	// of the render that is emitting it, so the observation made at the end of
	// WriteTo is read by the next scrape — which is what client_golang's own
	// promhttp_metric_handler_request_duration_seconds does, for the same
	// reason.
	scrape *Histogram
}

// NewRegistry builds an empty registry.
//
// It self-registers the two metrics about the metrics subsystem itself, because
// both need something only the registry has: the label-rejection counter needs
// the list of registered vecs (which is what makes its own label vocabulary
// closed), and the scrape-duration histogram needs to bracket WriteTo. Every
// other metric in this binary is named in set.go.
func NewRegistry(clk clock.Clock) *Registry {
	if clk == nil {
		clk = clock.System()
	}
	r := &Registry{names: make(map[string]bool), clk: clk}

	r.register(&family{
		name: "runmesh_metrics_label_rejected_total",
		help: "Label values rejected because they were outside the metric's declared vocabulary, by metric.",
		typ:  typeCounter,
		c:    &labelRejections{reg: r},
	})
	r.scrape = r.Histogram(Opts{
		Name: "runmesh_metrics_scrape_duration_seconds",
		Help: "Time spent rendering the exposition body, as measured by the PREVIOUS scrape.",
	}, bucketsIO)
	return r
}

// family is one metric name, its help text, its type line and the instrument
// that produces its samples.
type family struct {
	name string
	help string
	typ  string
	c    instrument
}

// instrument is the render half of every metric type in this package. A scalar
// implements it directly; a vec implements it by iterating its children and
// prepending their label pairs.
//
// Declaring it here rather than exporting anything keeps the set of things that
// can appear in an exposition body closed: a new metric type is a new type in
// this package, not a plugin point.
type instrument interface {
	// appendSamples writes every sample line of this family, each terminated by
	// a newline. extra is prepended to every line's label set; it is how a vec
	// hands its child the values that identify it.
	appendSamples(dst []byte, name string, extra []labelPair) []byte
	// series reports the maximum number of time series this instrument can ever
	// produce. It is the term SeriesUpperBound sums.
	series() int
}

const (
	typeCounter   = "counter"
	typeGauge     = "gauge"
	typeHistogram = "histogram"
)

// register adds a family, panicking on anything that is a wiring mistake.
//
// Panicking is the right answer for exactly the reason http.ServeMux panics on
// a duplicate pattern: every one of these is a mistake in run()'s construction
// sequence, it is deterministic, and it is discovered on the first boot. The
// alternative — returning an error that the wiring ignores, or degrading at
// scrape time — turns a five-second boot failure into a dashboard that is
// quietly missing a panel on a running server.
func (r *Registry) register(f *family) {
	if err := validName(f.name); err != nil {
		panic(fmt.Sprintf("metrics: %v", err))
	}
	if f.help == "" {
		panic(fmt.Sprintf("metrics: metric %q has no help string; "+
			"every metric must say what it measures, in the exposition itself", f.name))
	}
	if f.typ == typeCounter && !hasSuffix(f.name, "_total") {
		panic(fmt.Sprintf("metrics: counter %q must end in _total", f.name))
	}
	if f.typ == typeGauge && hasSuffix(f.name, "_total") {
		panic(fmt.Sprintf("metrics: gauge %q must not end in _total; "+
			"_total means a monotonically increasing counter and rate() over a "+
			"gauge that can decrease produces silent nonsense", f.name))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[f.name] {
		panic(fmt.Sprintf("metrics: metric %q is registered twice; "+
			"Prometheus rejects an entire scrape whose document carries two "+
			"HELP lines for one name", f.name))
	}
	r.names[f.name] = true
	r.families = append(r.families, f)
	// Sorted on insert rather than on render: a scrape must not pay for an
	// ordering that is fixed at boot, and WriteTo holding only a read lock means
	// it cannot sort in place anyway.
	sort.Slice(r.families, func(i, j int) bool { return r.families[i].name < r.families[j].name })
}

// registerVec records a labelled family so the label-rejection counter can
// enumerate it. Called with r.mu NOT held.
func (r *Registry) registerVec(v *vec) {
	r.mu.Lock()
	r.vecs = append(r.vecs, v)
	sort.Slice(r.vecs, func(i, j int) bool { return r.vecs[i].metric < r.vecs[j].metric })
	r.mu.Unlock()
}

// Counter registers an unlabelled counter.
func (r *Registry) Counter(o Opts) *Counter {
	c := &Counter{}
	r.register(&family{name: o.Name, help: o.Help, typ: typeCounter, c: c})
	return c
}

// CounterFunc registers a counter whose value is read at scrape time from an
// existing source.
//
// This is the shape every derived counter in this binary uses, and the reason
// is stated at length in gauges.go: a value that is MIRRORED into a registry
// can drift from its source the first time somebody adds an early return on one
// path and not the other. A value that is READ from its source cannot.
func (r *Registry) CounterFunc(o Opts, f func() uint64) {
	r.register(&family{name: o.Name, help: o.Help, typ: typeCounter, c: counterFunc(f)})
}

// CounterFloatFunc registers a monotonic counter whose source is fractional.
//
// Use it for a cumulative DURATION and for nothing else. Counters of events
// stay integral — counter.go argues both halves — and TestSecondsCountersAreNotBridgedThroughIntegers
// enforces the rule from the other side: a _seconds_total family sourced
// through an integer accessor silently floors its value to zero.
func (r *Registry) CounterFloatFunc(o Opts, f func() float64) {
	r.register(&family{name: o.Name, help: o.Help, typ: typeCounter, c: counterFloatFunc(f)})
}

// CounterVec registers a counter with a closed label vocabulary. See labels.go
// for what "closed" buys.
func (r *Registry) CounterVec(o Opts, labels ...Label) *CounterVec {
	v := newVec(o.Name, labels, func() instrument { return &Counter{} })
	r.register(&family{name: o.Name, help: o.Help, typ: typeCounter, c: v})
	r.registerVec(v)
	return &CounterVec{v: v}
}

// Gauge registers an unlabelled gauge.
func (r *Registry) Gauge(o Opts) *Gauge {
	g := &Gauge{}
	r.register(&family{name: o.Name, help: o.Help, typ: typeGauge, c: g})
	return g
}

// GaugeFunc registers a gauge read at scrape time. Everything this runtime
// gauges — workers, inflight, idle capacity, queue depth — already exists
// somewhere else, so nothing is mirrored and nothing can drift.
func (r *Registry) GaugeFunc(o Opts, f func() float64) {
	r.register(&family{name: o.Name, help: o.Help, typ: typeGauge, c: gaugeFunc(f)})
}

// GaugeVec registers a labelled gauge. The one use in this binary is
// runmesh_build_info, whose labels are a single fixed version and Go version —
// which is exactly the shape a closed vocabulary of one value describes.
func (r *Registry) GaugeVec(o Opts, labels ...Label) *GaugeVec {
	v := newVec(o.Name, labels, func() instrument { return &Gauge{} })
	r.register(&family{name: o.Name, help: o.Help, typ: typeGauge, c: v})
	r.registerVec(v)
	return &GaugeVec{v: v}
}

// Histogram registers an unlabelled histogram over the given upper bounds.
func (r *Registry) Histogram(o Opts, bounds []float64) *Histogram {
	h := newHistogram(o.Name, bounds)
	r.register(&family{name: o.Name, help: o.Help, typ: typeHistogram, c: h})
	return h
}

// HistogramVec registers a labelled histogram. Every child shares the same
// bounds, which is what makes a sum across the label dimension meaningful.
func (r *Registry) HistogramVec(o Opts, bounds []float64, labels ...Label) *HistogramVec {
	checkBounds(o.Name, bounds)
	v := newVec(o.Name, labels, func() instrument { return newHistogram(o.Name, bounds) })
	r.register(&family{name: o.Name, help: o.Help, typ: typeHistogram, c: v})
	r.registerVec(v)
	return &HistogramVec{v: v}
}

// SeriesUpperBound is the maximum number of time series this registry can ever
// export. It is a constant, computable at boot, because every label vocabulary
// in this process is closed at wiring time — see labels.go. TestSeriesBudget
// asserts it against a named ceiling, which is the cardinality proof no
// client_golang deployment can make.
func (r *Registry) SeriesUpperBound() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, f := range r.families {
		n += f.c.series()
	}
	return n
}

// Names lists every registered metric name, sorted. It exists for the
// registration-time lint tests, which are this package's replacement for
// promlint — except they also run at boot, so an illegal name is a panic in
// run() rather than a red build after the fact.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.families))
	for _, f := range r.families {
		out = append(out, f.name)
	}
	return out
}

// typeOf reports the exposition type of a registered metric, for the lint
// tests. It returns "" for a name that is not registered.
func (r *Registry) typeOf(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.families {
		if f.name == name {
			return f.typ
		}
	}
	return ""
}

// WriteTo renders the whole registry as Prometheus text exposition 0.0.4.
//
// Families are emitted in lexical order with EVERY sample of a family
// contiguous, and that is not cosmetic: a parser rejects the entire document —
// not the offending family, the document — if a metric name's HELP line appears
// twice, which is what interleaving two families produces. Sorting at
// registration and rendering in one pass is what makes the property structural
// instead of a thing to remember.
//
// The whole body is written through a bufio.Writer, and each family is built
// into a scratch slice that is reused across families, so a scrape allocates
// once rather than once per sample line.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	started := r.clk.Now()

	bw := bufio.NewWriterSize(w, 16<<10)
	var total int64
	buf := make([]byte, 0, 4<<10)

	r.mu.RLock()
	for _, f := range r.families {
		buf = buf[:0]
		buf = appendHelp(buf, f.name, f.help)
		buf = appendType(buf, f.name, f.typ)
		buf = f.c.appendSamples(buf, f.name, nil)
		n, err := bw.Write(buf)
		total += int64(n)
		if err != nil {
			r.mu.RUnlock()
			return total, err
		}
	}
	r.mu.RUnlock()

	if err := bw.Flush(); err != nil {
		return total, err
	}
	// Observed after the flush, and therefore read by the NEXT scrape. See the
	// comment on Registry.scrape.
	r.scrape.ObserveDuration(r.clk.Since(started))
	return total, nil
}

// labelRejections renders runmesh_metrics_label_rejected_total: one sample per
// labelled family, carrying that family's count of label values that fell
// outside its declared vocabulary.
//
// Its own label vocabulary is closed by construction — it is exactly the set of
// vecs registered — which is the property that makes a metric ABOUT cardinality
// safe from the failure it is watching for.
type labelRejections struct{ reg *Registry }

// appendSamples reads reg.vecs without locking. That is safe and deliberate:
// the only caller is Registry.WriteTo, which already holds reg.mu in read mode,
// and taking it again here would be a re-entrant read that a writer waiting in
// between could deadlock against.
func (l *labelRejections) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	pairs := make([]labelPair, len(extra)+1)
	copy(pairs, extra)
	for _, v := range l.reg.vecs {
		pairs[len(extra)] = labelPair{name: "metric", value: v.metric}
		dst = appendUintSample(dst, name, pairs, v.rejected.Load())
	}
	return dst
}

func (l *labelRejections) series() int { return len(l.reg.vecs) }

// validName enforces the exposition format's own rule for a metric name:
// [a-zA-Z_][a-zA-Z0-9_]*. A name outside it produces a line Prometheus cannot
// parse, and one unparseable line fails the whole scrape.
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("a metric must have a name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("metric name %q is not [a-zA-Z_][a-zA-Z0-9_]*", name)
		}
	}
	return nil
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
