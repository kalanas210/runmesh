package httpapi_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/httpapi"
)

// stubGatherer is a fixed exposition body. It satisfies httpapi.Gatherer, which
// is the whole of what this package knows about metrics — the one-method
// interface is what keeps internal/httpapi free of internal/metrics, and a test
// double for it is three lines.
type stubGatherer string

func (s stubGatherer) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, string(s))
	return int64(n), err
}

type brokenGatherer struct{}

func (brokenGatherer) WriteTo(io.Writer) (int64, error) {
	return 0, errors.New("registry exploded")
}

// panickingGatherer writes a plausible prefix and THEN panics, which is the
// shape a real registry fails in: a collector halfway through the exposition.
type panickingGatherer struct{}

func (panickingGatherer) WriteTo(w io.Writer) (int64, error) {
	n, _ := io.WriteString(w, "# HELP runmesh_up 1\n")
	_ = n
	panic("a collector exploded mid-exposition")
}

const sampleExposition = "# HELP runmesh_up 1\n# TYPE runmesh_up gauge\nrunmesh_up 1\n"

func TestMetricsServesTheExpositionContentType(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *httpapi.Deps) { d.Gatherer = stubGatherer(sampleExposition) })

	rec := f.do(http.MethodGet, "/api/v1/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The version parameter is not decorative: a scraper reads it to choose a
	// parser, and a body served as application/json or as bare text/plain is a
	// body Prometheus may refuse or may misread.
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the 0.0.4 exposition type", got)
	}
	// A cached exposition is a counter that appears to have stopped.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if rec.Body.String() != sampleExposition {
		t.Errorf("body = %q, want the gatherer's bytes verbatim", rec.Body.String())
	}
}

// TestMetricsRenderFailureIsACleanEnvelope pins the reason the handler buffers
// before it writes. A render that fails must produce the ordinary JSON error
// envelope with NOTHING of the exposition already on the wire — because a
// half-written text body with a JSON object spliced into it is not a partial
// scrape, it is a scrape Prometheus rejects wholesale.
func TestMetricsRenderFailureIsACleanEnvelope(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *httpapi.Deps) { d.Gatherer = brokenGatherer{} })

	rec := f.do(http.MethodGet, "/api/v1/metrics", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the JSON envelope", ct)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != httpapi.CodeInternal {
		t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodeInternal)
	}
	if strings.Contains(rec.Body.String(), "# HELP") {
		t.Error("the error body carries exposition text; the handler wrote before it knew it could")
	}
}

// TestMetricsPanicMidRenderIsACleanEnvelope is the other half of why this
// handler buffers, and it is the half the comment above metrics() used to get
// wrong.
//
// Recover does NOT write its envelope unconditionally — middleware.go returns
// early once the recorder has a status, and TestPanicMidStreamDoesNotSpliceJSON
// pins that. So buffering is not a defence against Recover splicing JSON into a
// text body; it is what keeps this handler on the safe side of Recover's check.
// A collector that panics halfway through the exposition panics into a
// bytes.Buffer, the ResponseWriter is still untouched, and Recover can therefore
// still do the useful thing: a clean 500 envelope with no exposition text in it.
// Stream the exposition instead and the same panic arrives with a 200 and a
// text/plain header already committed, and Recover — correctly — falls silent,
// leaving the scraper a truncated document that parses.
func TestMetricsPanicMidRenderIsACleanEnvelope(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *httpapi.Deps) { d.Gatherer = panickingGatherer{} })

	rec := f.do(http.MethodGet, "/api/v1/metrics", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: nothing had been written, so Recover could "+
			"still choose a status", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the JSON envelope", ct)
	}
	if env := decodeEnvelope(t, rec); env.Error.Code != httpapi.CodeInternal {
		t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodeInternal)
	}
	if strings.Contains(rec.Body.String(), "# HELP") {
		t.Error("the half-rendered exposition reached the response body; the panic " +
			"has to be contained by the buffer, not spliced into a JSON envelope")
	}
}

// TestMetricsWithoutARegistryIsUnimplemented: 501, not 404. "This server does
// not do that" and "you got the URL wrong" are different answers, and the
// codebase already makes that distinction for the planner.
func TestMetricsWithoutARegistryIsUnimplemented(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // no Gatherer

	rec := f.do(http.MethodGet, "/api/v1/metrics", "", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error.Code != httpapi.CodeUnimplemented {
		t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodeUnimplemented)
	}
}

// TestRoutePatternsCoversEveryRoute keeps the metric label vocabulary and the
// mux from drifting apart. RoutePatterns is the ONLY source of the `route`
// label's permitted values, so a pattern missing from it becomes an "other"
// bucket on every dashboard — silently, because the request still succeeds.
func TestRoutePatternsCoversEveryRoute(t *testing.T) {
	t.Parallel()

	patterns := httpapi.RoutePatterns()
	if len(patterns) == 0 {
		t.Fatal("RoutePatterns is empty")
	}
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		if seen[p] {
			t.Errorf("RoutePatterns lists %q twice", p)
		}
		seen[p] = true
		// Every pattern carries its method, which is why the metric label is
		// the route alone and not route plus method.
		if !strings.Contains(p, " ") {
			t.Errorf("pattern %q carries no method", p)
		}
	}
	for _, want := range []string{
		"GET /api/v1/metrics", "GET /api/v1/health", "GET /api/v1/ready",
		"POST /api/v1/jobs", "GET /api/v1/jobs/{id}",
	} {
		if !seen[want] {
			t.Errorf("RoutePatterns is missing %q", want)
		}
	}
}
