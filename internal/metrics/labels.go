package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Label is a metric dimension AND the vocabulary it is permitted to take.
//
// THIS IS THE WHOLE CARDINALITY DESIGN, and it is the one axis on which
// hand-rolling beats client_golang rather than merely matching it.
// GetMetricWithLabelValues will mint a new time series for any string it is
// ever handed; that is the standard production cardinality explosion, and the
// library offers no structural defence against it — a job id reaching a label
// is an OOM, discovered in production.
//
// Every label vocabulary in RunMesh is closed at wiring time. Tool names come
// from tools.Registry.Names(), states from runmesh.AllStates, stop reasons from
// the five engine.Stop values, error codes from the runmesh.Code* constants,
// routes from httpapi.RoutePatterns(). So a dimension is declared here together
// with the values it may take, and two things follow:
//
//   - A value outside the set does not create a series. It resolves to a shared
//     "other" child and increments runmesh_metrics_label_rejected_total{metric},
//     so a cardinality mistake surfaces as a metric an operator can alert on
//     rather than as a process that dies at 3am.
//   - The maximum number of time series this binary can export is the product
//     of the vocabularies, summed over the families: a constant, computable at
//     boot, asserted by TestSeriesBudget. "The upper bound on our series count
//     is a number in a test" is a stronger claim than any client_golang
//     deployment can make, and it is available only because the vocabulary is
//     closed — a property of this domain, not of any library.
//
// The one genuinely open axis is tool-supplied error codes: a tool may emit its
// own. That is exactly what "other" is for.
type Label struct {
	Name   string
	Values []string
}

// otherValue is the shared child every out-of-vocabulary value collapses into.
// It is spelled as a value rather than dropped so that the series still exists
// and still sums — losing the observation entirely would make
// rate(attempts_total) disagree with rate(attempt_failures_total) for a reason
// nobody could find.
const otherValue = "other"

// childSeparator joins a child's label values into its map key. 0x1f is the
// ASCII unit separator: it cannot appear in any value this process produces,
// and even if it did, the values themselves are validated against a closed
// vocabulary before they are joined, so two distinct children cannot collide on
// one key.
const childSeparator = "\x1f"

// preMaterialiseLimit is the cross-product size below which every child is
// created at boot.
//
// Below it, an absent series reads as ZERO rather than as no-data, and those
// are different alerts: "no step has failed with this code" and "this exporter
// has never been scraped" should not look the same on a dashboard. Above it,
// pre-materialising would spend boot time and memory on series that will never
// be observed, so children are created lazily on first observation — still
// within the declared bound, because a lazily created child is still one of the
// cross product's cells.
const preMaterialiseLimit = 64

// vec is a family with labels: a read-mostly map from a joined label key to the
// child instrument that carries that series.
//
// The lock is an RWMutex taken in READ mode on every observation. That is the
// hot path — an observation from settle runs on a goroutine holding a worker
// pool capacity token — so the common case is a read lock plus a map lookup and
// nothing else. The write lock is taken only when a child is created, which for
// a pre-materialised vec is never.
type vec struct {
	metric  string
	labels  []Label
	allowed []map[string]bool
	// bound is the size of the cross product of the EFFECTIVE vocabularies
	// (declared values plus "other"), which is the number of children that can
	// ever exist.
	bound int
	// perChild is how many time series one child produces: 1 for a counter or
	// gauge, len(bounds)+3 for a histogram.
	perChild int

	newChild func() instrument

	mu       sync.RWMutex
	children map[string]instrument

	// rejected counts observations whose label values were outside the declared
	// vocabulary. Rendered by the registry as
	// runmesh_metrics_label_rejected_total{metric=...}.
	rejected atomic.Uint64
}

func newVec(metric string, labels []Label, newChild func() instrument) *vec {
	if len(labels) == 0 {
		panic(fmt.Sprintf("metrics: %q was registered as a vec with no labels; "+
			"use the unlabelled constructor instead", metric))
	}
	v := &vec{
		metric:   metric,
		labels:   labels,
		allowed:  make([]map[string]bool, len(labels)),
		bound:    1,
		newChild: newChild,
		children: make(map[string]instrument),
	}
	for i, l := range labels {
		if err := validName(l.Name); err != nil {
			panic(fmt.Sprintf("metrics: %q: label %v", metric, err))
		}
		if len(l.Values) == 0 {
			panic(fmt.Sprintf("metrics: %q declares label %q with no permitted values; "+
				"a dimension whose vocabulary is not closed is the cardinality "+
				"explosion this type exists to make impossible", metric, l.Name))
		}
		// An empty VOCABULARY is a wiring mistake and panics, just above. An
		// empty VALUE inside a vocabulary is deliberately allowed, and the
		// distinction is worth arguing because it is not obvious.
		//
		// Prometheus's data model treats an empty label value as equivalent to
		// the label being ABSENT — the scraper drops it — so
		// runmesh_build_info{version="",go_version="go1.26.0"} is stored as a
		// series carrying only go_version. The dimension is degraded, not
		// aliased: every cell of a vec supplies a value for every declared
		// label, so two distinct cells can never collapse onto one stored
		// identity, and nothing that was countable is lost. What is lost is the
		// ability to group by that label, which is the honest cost of an
		// unstamped build.
		//
		// REJECTED: panicking here instead. The only empty value this binary can
		// produce is an unstamped runmesh_build_info version, and the version is
		// carried by -ldflags. Panicking would mean `go build ./cmd/server` with
		// the linker flags forgotten yields a binary that cannot boot — a server
		// refusing to start over the spelling of a telemetry label it exports
		// for information only. set.go says an empty version is not a reason to
		// fail a boot, and that is the right call; this is the code that makes
		// it true, since before the materialise fix above an empty value
		// silently mislabelled the series instead.
		set := make(map[string]bool, len(l.Values)+1)
		for _, val := range l.Values {
			set[val] = true
		}
		set[otherValue] = true
		v.allowed[i] = set
		v.bound *= len(set)
	}
	v.perChild = newChild().series()

	if v.bound <= preMaterialiseLimit {
		v.materialise()
	}
	return v
}

// materialise creates every cell of the DECLARED cross product, so an
// unobserved series renders as 0 instead of being absent.
//
// The "other" cells are deliberately NOT created here, even though they are
// counted in the bound. "other" is not a value this system produces in normal
// operation — it is the sentinel for a value nobody declared — so a series for
// it appearing at all is itself the signal, and a pre-materialised zero would
// bury that signal among len(labels) dead rows in every single family. The
// declared cells get the opposite treatment for the opposite reason: a tool
// that has never failed should read as zero failures, not as no-data, because
// those are different alerts.
//
// The iteration sentinel here is the label INDEX, never the accumulated key's
// content, and that distinction is the whole of a real bug. The obvious
// spelling — `if prefix == "" { first label }` — is wrong because the seed key
// is also "", so it cannot tell "no label has been appended yet" from "the
// label appended was the empty string". A vec whose first or middle label
// value was legitimately empty therefore got a key with a MISSING separator;
// appendSamples then split that key into fewer segments than there are labels,
// paired the surviving value with the wrong label name and dropped the rest.
// runmesh_build_info is exactly that shape, and it published the Go version
// under version= the moment a build shipped without -ldflags.
func (v *vec) materialise() {
	keys := []string{""}
	for i, l := range v.labels {
		values := make([]string, 0, len(l.Values))
		seen := make(map[string]bool, len(l.Values))
		for _, val := range l.Values {
			if val == otherValue || seen[val] {
				continue
			}
			seen[val] = true
			values = append(values, val)
		}
		sort.Strings(values)
		next := make([]string, 0, len(keys)*len(values))
		for _, prefix := range keys {
			for _, val := range values {
				if i == 0 {
					next = append(next, val)
					continue
				}
				next = append(next, prefix+childSeparator+val)
			}
		}
		keys = next
	}
	for _, k := range keys {
		v.children[k] = v.newChild()
	}
}

// with resolves label values to a child, creating it if necessary.
//
// It NEVER returns nil and it NEVER panics. Both matter: the call sites are the
// dispatcher and workers holding pool capacity, and a nil dereference or a panic
// there would turn a telemetry mistake into a shrunken worker pool or a lost
// attempt. A wrong number of values, or a value outside the vocabulary, is
// normalised to "other" and counted — the observation still lands, in a cell an
// operator can see.
func (v *vec) with(values []string) instrument {
	// The key is built into a STACK array and the map is indexed with
	// map[string(bytes)], which the compiler lowers to a lookup that does not
	// copy the bytes into a heap string. That is the whole reason this function
	// allocates nothing on the hot path, and it is why the obvious
	// strings.Builder version was rejected: an allocation per observation is an
	// allocation per settled attempt, on a goroutine holding a worker pool
	// capacity token. TestObserveIsAllocationFree pins it.
	//
	// A key longer than the scratch array still works — append grows it on the
	// heap — so the size is a performance bound, not a correctness one. 192
	// bytes is far past the longest key this vocabulary can produce.
	var scratch [192]byte
	key, rejected := v.appendKey(scratch[:0], values)
	if rejected {
		v.rejected.Add(1)
	}

	v.mu.RLock()
	c, ok := v.children[string(key)]
	v.mu.RUnlock()
	if ok {
		return c
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	// Interning the key here, on the miss path only, is the one allocation this
	// function can make — and for a pre-materialised vec it never happens at
	// all.
	interned := string(key)
	if c, ok := v.children[interned]; ok {
		return c
	}
	c = v.newChild()
	v.children[interned] = c
	return c
}

// appendKey normalises the supplied values against the declared vocabularies
// and joins them, reporting whether anything had to be replaced.
//
// A value outside the vocabulary, a missing value and a surplus value are all
// the same kind of mistake — a caller and a declaration that disagree — so all
// three normalise to "other" and all three are counted. None of them panics:
// the callers are engine goroutines, and a telemetry bug must not be able to
// take down an attempt.
func (v *vec) appendKey(dst []byte, values []string) ([]byte, bool) {
	rejected := len(values) != len(v.labels)
	for i := range v.labels {
		if i > 0 {
			dst = append(dst, childSeparator...)
		}
		val := otherValue
		if i < len(values) {
			val = values[i]
		}
		if !v.allowed[i][val] {
			val = otherValue
			rejected = true
		}
		dst = append(dst, val...)
	}
	return dst, rejected
}

// appendSamples renders every child, in sorted key order so the bytes are
// stable between scrapes and a golden file is meaningful.
func (v *vec) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	v.mu.RLock()
	keys := make([]string, 0, len(v.children))
	for k := range v.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]labelPair, len(extra), len(extra)+len(v.labels))
	copy(pairs, extra)
	for _, k := range keys {
		child := v.children[k]
		parts := strings.Split(k, childSeparator)
		// Belt and braces over the key round trip. Every key is built by
		// appendKey or by materialise, and both write exactly len(v.labels)
		// segments — but the index-vs-content bug fixed in materialise proves the
		// invariant is breakable, and the version that indexed v.labels blindly
		// turned that breakage into a DOCUMENT: it paired a surviving value with
		// whatever label name happened to sit at that index and dropped the
		// remaining dimensions, so the exposition confidently published one
		// label's value under another label's name. A scrape must not panic
		// either — /metrics is read-only, but a 500 there costs every dashboard
		// at once — so a key that does not round trip costs its own cell and
		// nothing else. An absent series is recoverable; a mislabelled one is
		// read and believed. TestMaterialisedKeysRoundTripToTheirLabels holds the
		// invariant itself.
		if len(parts) != len(v.labels) {
			continue
		}
		pairs = pairs[:len(extra)]
		for i, val := range parts {
			pairs = append(pairs, labelPair{name: v.labels[i].Name, value: val})
		}
		dst = child.appendSamples(dst, name, pairs)
	}
	v.mu.RUnlock()
	return dst
}

func (v *vec) series() int { return v.bound * v.perChild }
