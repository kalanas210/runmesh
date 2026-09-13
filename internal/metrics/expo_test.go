package metrics

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
)

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(clock.NewFake(epoch))
}

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if int(n) != buf.Len() {
		t.Fatalf("WriteTo reported %d bytes, wrote %d", n, buf.Len())
	}
	return buf.String()
}

// TestExpositionRoundTrips renders the registry and reads it back with a
// parser that knows nothing about the writer, then asserts every family and
// every sample reproduces the value the typed instrument holds.
//
// This is the test that retires the hand-rolling risk. A golden file proves the
// bytes are stable; only a parse proves they MEAN what the counters say — and a
// body that means something else is not a partial failure, it is `up=0` and
// every dashboard blank at once.
func TestExpositionRoundTrips(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	attempts := r.CounterVec(Opts{Name: "runmesh_attempts_total", Help: "attempts"},
		Label{Name: "tool", Values: []string{"echo", "sleep"}},
		Label{Name: "state", Values: []string{"SUCCEEDED", "FAILED"}},
	)
	depth := r.Gauge(Opts{Name: "runmesh_queue_depth", Help: "claimable steps"})
	dur := r.Histogram(Opts{Name: "runmesh_step_seconds", Help: "step duration"},
		[]float64{0.1, 1, 10})
	plain := r.Counter(Opts{Name: "runmesh_boots_total", Help: "boots"})

	attempts.With("echo", "SUCCEEDED").Add(7)
	attempts.With("sleep", "FAILED").Inc()
	// Outside the vocabulary on both axes: it must land in the shared "other"
	// cell rather than creating a series.
	attempts.With("rm -rf /", "MELTED").Add(3)
	depth.Set(42)
	plain.Add(5)
	for _, v := range []float64{0.05, 0.5, 5, 500} {
		dur.Observe(v)
	}

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("the writer emitted something the parser rejects: %v", err)
	}

	tests := []struct {
		name   string
		family string
		sample string
		labels map[string]string
		want   float64
	}{
		{"counter", "runmesh_boots_total", "runmesh_boots_total", nil, 5},
		{"gauge", "runmesh_queue_depth", "runmesh_queue_depth", nil, 42},
		{"vec cell", "runmesh_attempts_total", "runmesh_attempts_total",
			map[string]string{"tool": "echo", "state": "SUCCEEDED"}, 7},
		// Two out-of-vocabulary values collapsed into ONE shared cell rather
		// than creating a series each. That is the whole cardinality trade.
		{"vec other cell", "runmesh_attempts_total", "runmesh_attempts_total",
			map[string]string{"tool": "other", "state": "other"}, 3},
		// Pre-materialised, so an untouched cell is a zero rather than an
		// absence — which is a different alert from no-data.
		{"vec untouched cell", "runmesh_attempts_total", "runmesh_attempts_total",
			map[string]string{"tool": "sleep", "state": "SUCCEEDED"}, 0},
		{"histogram count", "runmesh_step_seconds", "runmesh_step_seconds_count", nil, 4},
		{"histogram sum", "runmesh_step_seconds", "runmesh_step_seconds_sum", nil, 505.55},
		{"histogram le=0.1", "runmesh_step_seconds", "runmesh_step_seconds_bucket",
			map[string]string{"le": "0.1"}, 1},
		{"histogram le=1", "runmesh_step_seconds", "runmesh_step_seconds_bucket",
			map[string]string{"le": "1"}, 2},
		{"histogram le=+Inf", "runmesh_step_seconds", "runmesh_step_seconds_bucket",
			map[string]string{"le": "+Inf"}, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := families[tc.family]
			if !ok {
				t.Fatalf("family %q is missing from the body", tc.family)
			}
			labels := tc.labels
			if labels == nil {
				labels = map[string]string{}
			}
			s, ok := f.sample(tc.sample, labels)
			if !ok {
				t.Fatalf("sample %s%v is missing from family %q", tc.sample, labels, tc.family)
			}
			if math.Abs(s.value-tc.want) > 1e-9 {
				t.Errorf("%s%v = %v, want %v", tc.sample, labels, s.value, tc.want)
			}
		})
	}

	// The typed values are the source of truth; the parse must agree with them.
	if got := attempts.With("echo", "SUCCEEDED").Value(); got != 7 {
		t.Errorf("Counter.Value = %d, want 7", got)
	}
	if got := attempts.Rejected(); got != 1 {
		t.Errorf("rejected = %d, want 1 (one observation used two out-of-vocabulary values)", got)
	}

	types := map[string]string{
		"runmesh_boots_total":    "counter",
		"runmesh_attempts_total": "counter",
		"runmesh_queue_depth":    "gauge",
		"runmesh_step_seconds":   "histogram",
	}
	for name, want := range types {
		if got := families[name].typ; got != want {
			t.Errorf("TYPE %s = %q, want %q", name, got, want)
		}
	}
}

// TestExpositionMatchesGolden is the half a parser cannot check: that a human
// has read these exact bytes once, against the spec.
//
// It is driven by a *clock.Fake so the scrape-duration histogram observes a
// zero and the body is byte-stable. Regenerate with RUNMESH_UPDATE_GOLDEN=1
// AFTER reading the diff, never before — a golden file regenerated to make a
// test pass records the bug instead of catching it.
func TestExpositionMatchesGolden(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	r.Counter(Opts{Name: "runmesh_boots_total", Help: "Times this process started."}).Add(3)
	r.Gauge(Opts{Name: "runmesh_workers", Help: "Configured worker pool size."}).Set(8)
	r.GaugeFunc(Opts{Name: "runmesh_queue_depth", Help: "Claimable steps."},
		func() float64 { return 12 })
	r.CounterFunc(Opts{Name: "runmesh_events_dropped_total", Help: "Dropped events."},
		func() uint64 { return 1 })

	outcomes := r.CounterVec(Opts{
		Name: "runmesh_outcomes_total",
		Help: `Outcomes, by tool. Backslash \ and "quote" survive a help line unescaped-ish.`,
	}, Label{Name: "tool", Values: []string{"echo", "report_generate"}})
	outcomes.With("echo").Add(9)
	outcomes.With("report_generate").Inc()

	h := r.HistogramVec(Opts{Name: "runmesh_step_seconds", Help: "Step duration."},
		[]float64{0.005, 0.1, 2.5, 600}, Label{Name: "tool", Values: []string{"echo"}})
	h.With("echo").Observe(0.004)
	h.With("echo").Observe(0.2)
	h.With("echo").ObserveDuration(3 * time.Second)
	h.With("echo").Observe(9999)

	got := render(t, r)
	path := filepath.Join("testdata", "exposition.golden")

	if os.Getenv("RUNMESH_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Fatal("golden file rewritten; re-run without RUNMESH_UPDATE_GOLDEN")
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("the exposition body changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The golden bytes must also still parse, or the file records a mistake.
	if _, err := parseExposition(got); err != nil {
		t.Errorf("the golden body does not parse: %v", err)
	}
}

// TestHistogramInfBucketEqualsCount pins the invariant the design gets for
// free: _count is never stored, it IS the +Inf cumulative, so the two cannot
// disagree. A histogram whose +Inf differs from its _count parses cleanly and
// makes every quantile over it silently wrong.
func TestHistogramInfBucketEqualsCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		observations []float64
	}{
		{"empty", nil},
		{"all under the first bound", []float64{0.0001, 0.0002}},
		{"straddling every bound", []float64{0.0001, 0.05, 0.5, 3, 900}},
		{"all over the last bound", []float64{1e6, 1e9}},
		{"negatives clamp into bucket zero", []float64{-1, -0.5, 0.001}},
		{"exactly on a bound is inclusive", []float64{0.005, 0.1, 2.5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRegistry(t)
			h := r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"},
				[]float64{0.005, 0.1, 2.5, 600})
			for _, v := range tc.observations {
				h.Observe(v)
			}

			families, err := parseExposition(render(t, r))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			f := families["runmesh_x_seconds"]
			inf, ok := f.sample("runmesh_x_seconds_bucket", map[string]string{"le": "+Inf"})
			if !ok {
				t.Fatal("no le=\"+Inf\" bucket; the family is invalid without one")
			}
			count, ok := f.sample("runmesh_x_seconds_count", map[string]string{})
			if !ok {
				t.Fatal("no _count")
			}
			if inf.value != count.value {
				t.Errorf("+Inf = %v but _count = %v; they are the same number by construction",
					inf.value, count.value)
			}
			if want := float64(len(tc.observations)); count.value != want {
				t.Errorf("_count = %v, want %v", count.value, want)
			}
			if got := float64(h.Count()); got != count.value {
				t.Errorf("Histogram.Count() = %v but the body says %v", got, count.value)
			}
		})
	}
}

// TestHistogramBucketsAreCumulative is the other classic hand-rolled histogram
// bug: incrementing only the matching bucket and emitting it raw, after which
// histogram_quantile interpolates over counts that go down.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	h := r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"},
		[]float64{0.005, 0.1, 2.5, 600})
	for _, v := range []float64{0.001, 0.001, 0.05, 3, 3, 3, 1e9} {
		h.Observe(v)
	}

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var last float64
	seen := 0
	for _, s := range families["runmesh_x_seconds"].samples {
		if s.name != "runmesh_x_seconds_bucket" {
			continue
		}
		seen++
		if s.value < last {
			t.Errorf("bucket le=%q is %v after %v; cumulative counts must never decrease",
				s.labels["le"], s.value, last)
		}
		last = s.value
	}
	if want := 5; seen != want {
		t.Errorf("saw %d bucket lines, want %d (four bounds plus +Inf)", seen, want)
	}
	if last != 7 {
		t.Errorf("the last cumulative bucket is %v, want 7", last)
	}

	// And the specific numbers, so "monotonic" cannot be satisfied by a bug
	// that puts everything in the last bucket.
	want := map[string]float64{"0.005": 2, "0.1": 3, "2.5": 3, "600": 6, "+Inf": 7}
	for le, v := range want {
		s, ok := families["runmesh_x_seconds"].sample("runmesh_x_seconds_bucket",
			map[string]string{"le": le})
		if !ok {
			t.Fatalf("no bucket le=%q", le)
		}
		if s.value != v {
			t.Errorf("le=%q is %v, want %v", le, s.value, v)
		}
	}
}

// TestHistogramRefusesNaN: a NaN in _sum poisons the family until the process
// restarts. Every rate and every quantile over it is NaN, and there is no way
// to un-poison a monotonic sum.
func TestHistogramRefusesNaN(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	h := r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"}, []float64{1})
	h.Observe(1)
	h.Observe(math.NaN())

	if h.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", h.Dropped())
	}
	if h.Count() != 1 {
		t.Errorf("count = %d, want 1: a refused observation must not be counted", h.Count())
	}
	if math.IsNaN(h.Sum()) {
		t.Fatal("the sum is NaN; the family is poisoned for the life of the process")
	}
	if _, err := parseExposition(render(t, r)); err != nil {
		t.Errorf("body does not parse: %v", err)
	}
}

// TestLabelValueEscaping. No label value this process produces today can
// contain any of these — every one comes from a closed vocabulary stringified
// by house code — but the guarantee worth having is that the writer CANNOT emit
// an unparseable line, because one unparseable line fails the whole scrape.
func TestLabelValueEscaping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{"quote", `say "hello"`},
		{"backslash", `C:\runmesh\data`},
		{"newline", "first\nsecond"},
		{"all three at once", "a\\b\"c\nd"},
		{"comma, which must not split the label set", "a,b"},
		{"brace", "{not a label set}"},
		{"plain", "echo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRegistry(t)
			// Declared as a permitted value, so the vocabulary check does not
			// rewrite it to "other" before the escaping is exercised.
			c := r.CounterVec(Opts{Name: "runmesh_x_total", Help: "x"},
				Label{Name: "tool", Values: []string{tc.value}})
			c.With(tc.value).Add(11)

			body := render(t, r)
			families, err := parseExposition(body)
			if err != nil {
				t.Fatalf("body does not parse: %v\n%s", err, body)
			}
			s, ok := families["runmesh_x_total"].sample("runmesh_x_total",
				map[string]string{"tool": tc.value})
			if !ok {
				t.Fatalf("the value did not survive the round trip\n%s", body)
			}
			if s.value != 11 {
				t.Errorf("value = %v, want 11", s.value)
			}
		})
	}
}

// TestBoundFormattingIsStable. `le` is part of a series' IDENTITY: a boundary
// that formats one way in one scrape and another way in the next does not
// change a number, it retires one time series and starts another.
func TestBoundFormattingIsStable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   float64
		want string
	}{
		{0.005, "0.005"},
		{0.025, "0.025"},
		{1, "1"},
		{2.5, "2.5"},
		{600, "600"},
		{1e-6, "1e-06"},
		{0.1, "0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			got := formatBound(tc.in)
			if got != tc.want {
				t.Fatalf("formatBound(%v) = %q, want %q", tc.in, got, tc.want)
			}
			// The round trip is what actually matters: a formatting that does
			// not read back as the same float64 is a boundary that has moved.
			if again := formatBound(tc.in); again != got {
				t.Fatalf("formatBound is not deterministic: %q then %q", got, again)
			}
		})
	}
}

// TestRegistrationRejectsWiringMistakes. Every one of these is a mistake in
// run()'s construction sequence, so every one has to be a panic at boot rather
// than a dashboard quietly missing a panel on a running server — the same
// reason http.ServeMux panics on a duplicate pattern.
func TestRegistrationRejectsWiringMistakes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func(*Registry)
	}{
		{"an illegal metric name", func(r *Registry) {
			r.Counter(Opts{Name: "runmesh.attempts-total", Help: "x"})
		}},
		{"a name starting with a digit", func(r *Registry) {
			r.Counter(Opts{Name: "1_total", Help: "x"})
		}},
		{"a duplicate name", func(r *Registry) {
			r.Counter(Opts{Name: "runmesh_x_total", Help: "x"})
			r.Counter(Opts{Name: "runmesh_x_total", Help: "x"})
		}},
		{"no help string", func(r *Registry) {
			r.Counter(Opts{Name: "runmesh_x_total"})
		}},
		{"a counter that does not end in _total", func(r *Registry) {
			r.Counter(Opts{Name: "runmesh_x", Help: "x"})
		}},
		{"a gauge that does end in _total", func(r *Registry) {
			r.Gauge(Opts{Name: "runmesh_x_total", Help: "x"})
		}},
		{"descending bucket bounds", func(r *Registry) {
			r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"}, []float64{1, 0.5})
		}},
		{"a duplicated bucket bound", func(r *Registry) {
			r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"}, []float64{1, 1})
		}},
		{"an explicit +Inf bound", func(r *Registry) {
			r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"},
				[]float64{1, math.Inf(1)})
		}},
		{"no bucket bounds at all", func(r *Registry) {
			r.Histogram(Opts{Name: "runmesh_x_seconds", Help: "x"}, nil)
		}},
		{"a label with no vocabulary", func(r *Registry) {
			r.CounterVec(Opts{Name: "runmesh_x_total", Help: "x"}, Label{Name: "tool"})
		}},
		{"a vec with no labels", func(r *Registry) {
			r.CounterVec(Opts{Name: "runmesh_x_total", Help: "x"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("%s was accepted; it must fail the boot", tc.name)
				}
			}()
			tc.fn(newTestRegistry(t))
		})
	}
}

// TestOtherCellsAreLazyAndDeclaredCellsAreNot pins the asymmetry, because it is
// a deliberate decision rather than an accident of the implementation.
//
// A declared cell is pre-materialised so that "this tool has never failed"
// reads as zero rather than as no-data — those are different alerts. An "other"
// cell is NOT, because "other" is the sentinel for a value nobody declared: the
// series appearing at all is the signal, and a pre-materialised zero in every
// family would bury it.
func TestOtherCellsAreLazyAndDeclaredCellsAreNot(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	c := r.CounterVec(Opts{Name: "runmesh_x_total", Help: "x"},
		Label{Name: "tool", Values: []string{"echo", "sleep"}})

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["runmesh_x_total"]
	if len(f.samples) != 2 {
		t.Fatalf("a freshly registered vec rendered %d samples, want the 2 declared cells", len(f.samples))
	}
	for _, tool := range []string{"echo", "sleep"} {
		if _, ok := f.sample("runmesh_x_total", map[string]string{"tool": tool}); !ok {
			t.Errorf("declared cell tool=%q is absent; it must read as zero, not as no-data", tool)
		}
	}

	c.With("something_nobody_declared").Inc()
	families, err = parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := families["runmesh_x_total"].sample("runmesh_x_total", map[string]string{"tool": "other"})
	if !ok {
		t.Fatal("the out-of-vocabulary observation did not land anywhere")
	}
	if s.value != 1 {
		t.Errorf("other = %v, want 1", s.value)
	}
	if c.Rejected() != 1 {
		t.Errorf("rejected = %d, want 1", c.Rejected())
	}
}
