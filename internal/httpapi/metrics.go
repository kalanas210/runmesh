package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// Gatherer renders the metrics exposition body.
//
// One method, declared HERE, for exactly the reason Runtime and Policy are
// declared here: it keeps internal/httpapi from importing internal/metrics, so
// neither package can reach into the other and a test satisfies this with a
// closure over a string. It is also the whole of the reversibility claim in ADR
// 0012 — swapping the hand-rolled registry for client_golang's Gather means
// satisfying this one method, not rewriting a handler.
type Gatherer interface {
	WriteTo(w io.Writer) (int64, error)
}

// metricsContentType is the exposition format's own content type. The version
// parameter is not decorative: a scraper reads it to pick a parser, and
// omitting it makes Prometheus guess.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// metrics is GET /api/v1/metrics.
//
// IT RENDERS INTO A BUFFER FIRST, AND ONLY THEN TOUCHES THE ResponseWriter.
// That is not a style preference; it is the only shape that survives this
// package's existing error contract. writeError does not report a failure it is
// handed a status for — it CLASSIFIES the error and picks the status itself,
// after the fact, which it can only do while the header is still unsent. So a
// handler that streamed the exposition straight out would reach its first
// failure having already committed to 200 and to a text/plain body, with no way
// left to say anything else: the only remaining options are a truncated
// document that looks valid, or a JSON envelope spliced into the middle of a
// text one. Prometheus does not reject the offending line; it rejects the WHOLE
// document, sets up=0, and every dashboard goes blank at once. Buffering means a
// render that fails does so before a single byte is written and gets a clean 500
// envelope, and a render that succeeds produces a body that is complete before
// the first byte leaves.
//
// The panic case needs no argument of its own here, because Recover in
// middleware.go declines to write its envelope once anything has been written
// — see its own comment for why. That makes buffering the thing that keeps this
// handler on the side of that check rather than a defence against crossing it:
// a panic inside a.gatherer.WriteTo happens while nothing is written, so
// Recover can still produce the 500 this route wants.
//
// Cache-Control: no-store because every value here is read at scrape time and a
// cached exposition is a counter that appears to stop.
func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	if a.gatherer == nil {
		writeError(w, r, a.log, errNoMetrics)
		return
	}

	var buf bytes.Buffer
	if _, err := a.gatherer.WriteTo(&buf); err != nil {
		writeError(w, r, a.log, err)
		return
	}

	w.Header().Set("Content-Type", metricsContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		// The client hung up mid-body. There is nothing to say to it and
		// nothing to fix, and a scrape that dies half way is already visible to
		// Prometheus as a failed scrape, so this is a Debug line rather than a
		// Warn that would fire once per flaky network blip.
		a.log.Debug("metrics scrape ended early", "err", err)
	}
}

// errNoMetrics is a deployment built without a registry. 501 rather than 404,
// matching errNoPlanner exactly: "this server does not do that" and "you got
// the URL wrong" are different answers and deserve different status codes.
// Nothing in the shipped wiring can produce it — run() always constructs a
// registry — but a test or an embedding that builds an API without one gets an
// honest refusal instead of a panic.
var errNoMetrics = errors.New("httpapi: no metrics registry configured")

// RoutePatterns is the route vocabulary, for whatever needs to know the closed
// set of values the `route` metric label can take.
//
// It walks the same routes() table the mux is built from, so the vocabulary has
// ONE source of truth: a route added to the table without being added here is
// impossible, which is what stops a new endpoint from quietly becoming an
// "other" bucket on every dashboard. It does not include "unmatched" — that is
// a value routeOf() invents for a request the mux did not route, and the metric
// set adds it deliberately rather than inheriting it from a table of real
// routes.
func RoutePatterns() []string {
	rts := (&API{}).routes()
	out := make([]string, 0, len(rts))
	for _, rt := range rts {
		out = append(out, rt.pattern)
	}
	return out
}
