package metrics

import (
	"strings"
	"testing"
)

// These tests exist because of one specific defect, and it is worth stating the
// shape of it before the assertions.
//
// vec.materialise() builds a child's map key by folding the label values
// together with 0x1F between them, and it used `if prefix == ""` to decide
// whether it was on the FIRST label and therefore should not write a separator.
// The seed key is also "" — so an empty first or middle label VALUE looked
// exactly like "nothing appended yet", the separator was skipped, and the key
// came out with fewer segments than the vec has labels. vec.appendSamples then
// split that key, walked v.labels by index, and published the surviving value
// under the WRONG label name with the remaining dimensions simply gone.
//
// That is not an abstract concern: runmesh_build_info declares
// version and go_version with one value each, and the version is stamped by
// -ldflags. A build linked without the flag exported
//
//	runmesh_build_info{version="",go_version="go1.26.0"} 1
//	runmesh_build_info{version="go1.26.0"} 0
//
// where the second line claims the Go toolchain is the RunMesh version. Two
// series for one build, one of them a confident lie, and nothing failed.

// TestEmptyLabelValueKeepsEveryDimension is the direct regression, in the shape
// runmesh_build_info actually has: a two-label family whose first value is
// empty.
func TestEmptyLabelValueKeepsEveryDimension(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	g := r.GaugeVec(Opts{
		Name: "test_build_info",
		Help: "Always 1; the labels are the payload.",
	},
		Label{Name: "version", Values: []string{""}},
		Label{Name: "go_version", Values: []string{"go1.26.0"}},
	)
	g.With("", "go1.26.0").Set(1)

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["test_build_info"]
	if f == nil {
		t.Fatal("test_build_info is missing entirely")
	}

	// Exactly one series. The bug produced two: the pre-materialised cell under
	// the truncated key, plus the one With() created under the correct key.
	if len(f.samples) != 1 {
		t.Fatalf("test_build_info exported %d series for a one-value-per-label "+
			"vocabulary, want 1:\n%s", len(f.samples), samplesOf(f))
	}
	s := f.samples[0]
	if len(s.labels) != 2 {
		t.Fatalf("the sample carries %d labels, want 2 — a dimension was dropped: %v",
			len(s.labels), s.labels)
	}
	if s.labels["version"] != "" {
		t.Errorf(`version=%q, want "" — the go_version value was published under `+
			`the version label`, s.labels["version"])
	}
	if s.labels["go_version"] != "go1.26.0" {
		t.Errorf("go_version=%q, want go1.26.0", s.labels["go_version"])
	}
	if s.value != 1 {
		t.Errorf("value = %v, want 1", s.value)
	}
}

// TestEmptyLabelValueInTheMiddleKeepsEveryDimension is the defect at a
// position no two-label test can reach.
//
// The trigger is precisely "the key accumulated so far is still empty", so the
// middle label loses its separator only when the values before it were also
// empty — which is why the first dimension here declares BOTH "" and "a". Of
// the four declared cells, the two whose first value is empty came out with a
// missing separator: {"", "m", "z"} rendered as a single segment pair
// {first="m", middle="z"} with `last` gone, and {"", "", "z"} collapsed to
// {first="z"} alone. The two cells starting at "a" were always correct, which
// is what made this so easy to miss — half the family looked right.
func TestEmptyLabelValueInTheMiddleKeepsEveryDimension(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	r.CounterVec(Opts{
		Name: "test_middle_total",
		Help: "Three dimensions; the first two admit the empty string.",
	},
		Label{Name: "first", Values: []string{"", "a"}},
		Label{Name: "middle", Values: []string{"", "m"}},
		Label{Name: "last", Values: []string{"z"}},
	)

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["test_middle_total"]
	if f == nil {
		t.Fatal("test_middle_total is missing entirely")
	}

	// The declared cross product is 2 x 2 x 1, and every cell must be present
	// exactly once with all three dimensions intact.
	for _, want := range []map[string]string{
		{"first": "", "middle": "", "last": "z"},
		{"first": "", "middle": "m", "last": "z"},
		{"first": "a", "middle": "", "last": "z"},
		{"first": "a", "middle": "m", "last": "z"},
	} {
		if _, ok := f.sample("test_middle_total", want); !ok {
			t.Errorf("the cell %v is missing or mislabelled:\n%s", want, samplesOf(f))
		}
	}
	if len(f.samples) != 4 {
		t.Errorf("exported %d series against a declared cross product of 4:\n%s",
			len(f.samples), samplesOf(f))
	}
}

// TestEveryLabelValueEmpty is the degenerate case, and it is the one where the
// broken key collapsed to the empty string entirely: three labels, one segment,
// two dimensions gone.
func TestEveryLabelValueEmpty(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	r.CounterVec(Opts{
		Name: "test_all_empty_total",
		Help: "Every declared value is the empty string.",
	},
		Label{Name: "a", Values: []string{""}},
		Label{Name: "b", Values: []string{""}},
		Label{Name: "c", Values: []string{""}},
	)

	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["test_all_empty_total"]
	if f == nil {
		t.Fatal("test_all_empty_total is missing entirely")
	}
	if len(f.samples) != 1 {
		t.Fatalf("exported %d series, want 1:\n%s", len(f.samples), samplesOf(f))
	}
	if len(f.samples[0].labels) != 3 {
		t.Errorf("the sample carries %d labels, want 3: %v",
			len(f.samples[0].labels), f.samples[0].labels)
	}
}

// TestEmptyLabelValueObservesIntoThePreMaterialisedCell closes the other half:
// the key materialise() writes and the key with() builds must be the SAME key,
// or an observation lands in a second cell and the family exports one series
// per code path. The counts here are the evidence — a zero alongside a one is
// what the bug looked like in the body.
func TestEmptyLabelValueObservesIntoThePreMaterialisedCell(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	cv := r.CounterVec(Opts{
		Name: "test_observed_total",
		Help: "Observed through the empty-valued cell.",
	},
		Label{Name: "version", Values: []string{""}},
		Label{Name: "go_version", Values: []string{"go1.26.0"}},
	)
	cv.With("", "go1.26.0").Inc()
	cv.With("", "go1.26.0").Inc()

	if got := cv.Rejected(); got != 0 {
		t.Errorf("an in-vocabulary empty value was counted as rejected %d times", got)
	}
	if n := len(cv.v.children); n != 1 {
		t.Fatalf("the vec holds %d children for a one-cell vocabulary, want 1; "+
			"materialise() and with() disagree on the key", n)
	}
	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["test_observed_total"]
	s, ok := f.sample("test_observed_total", map[string]string{"version": "", "go_version": "go1.26.0"})
	if !ok {
		t.Fatalf("the observed series is missing:\n%s", samplesOf(f))
	}
	if s.value != 2 {
		t.Errorf("value = %v, want 2: the increments landed in a different cell than "+
			"the one that renders", s.value)
	}
}

// TestMaterialisedKeysRoundTripToTheirLabels asserts the invariant directly,
// across every vec the production wiring registers plus the empty-valued shapes
// above: a child's key, split on the separator, yields exactly one segment per
// declared label. This is the property appendSamples now verifies before it
// indexes v.labels, and it is the one the old materialise() broke.
func TestMaterialisedKeysRoundTripToTheirLabels(t *testing.T) {
	t.Parallel()

	r, _ := productionSet(t)

	// Plus the shapes the shipped vocabulary does not contain today: an empty
	// value in each position, so the invariant is tested and not merely
	// observed to hold.
	r.CounterVec(Opts{Name: "zz_empty_first_total", Help: "h"},
		Label{Name: "a", Values: []string{"", "x"}},
		Label{Name: "b", Values: []string{"y"}},
	)
	r.CounterVec(Opts{Name: "zz_empty_middle_total", Help: "h"},
		Label{Name: "a", Values: []string{"x"}},
		Label{Name: "b", Values: []string{"", "y"}},
		Label{Name: "c", Values: []string{"z"}},
	)

	for _, v := range r.vecs {
		v.mu.RLock()
		for key := range v.children {
			if n := len(strings.Split(key, childSeparator)); n != len(v.labels) {
				t.Errorf("%s: key %q splits into %d segments against %d declared "+
					"labels; appendSamples would pair a value with the wrong label name",
					v.metric, key, n, len(v.labels))
			}
		}
		v.mu.RUnlock()
	}
}

// samplesOf renders a family's series for a failure message. A diff of label
// sets is what makes a mislabelling failure readable at all.
func samplesOf(f *parsedFamily) string {
	var b strings.Builder
	for _, s := range f.samples {
		b.WriteString("  ")
		b.WriteString(s.name)
		b.WriteString(" ")
		for k, v := range s.labels {
			b.WriteString(k + "=" + `"` + v + `" `)
		}
		b.WriteString("\n")
	}
	return b.String()
}
