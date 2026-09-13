package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestAFailingModelIsNeverAnInternalError.
//
// A planning model fails in ways a caller has to tell apart: its goal was
// refused, a quota ran out, or the model this deployment names no longer
// exists. Every one of them used to reach the caller as "500 internal error",
// with the reason only in the server log — which is how a retired model name
// looked exactly like a RunMesh bug on 2026-09-13.
//
// Each case runs against both endpoints, because both map failures through the
// same function and a second mapping would be a second place for this to rot.
func TestAFailingModelIsNeverAnInternalError(t *testing.T) {
	t.Parallel()

	const upstream = "text only the provider wrote"
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		issue  string
		// retryAfter is the exact header value, and "" means the header must be
		// absent: telling a caller to wait for something waiting cannot fix is
		// worse than saying nothing.
		retryAfter string
	}{
		{
			name:   "refused",
			err:    errors.Join(planner.ErrModelRefused, runmesh.Fatal("gemini_prompt_blocked", upstream)),
			status: http.StatusUnprocessableEntity, code: "unprocessable", issue: "gemini_prompt_blocked",
		},
		{
			name:   "rate limited",
			err:    errors.Join(planner.ErrModelRateLimited, runmesh.RetryIn(90*time.Second, "gemini_rate_limited", upstream)),
			status: http.StatusTooManyRequests, code: "resource_exhausted", issue: "gemini_rate_limited",
			retryAfter: "90",
		},
		{
			name:   "unusable",
			err:    errors.Join(planner.ErrModelUnusable, runmesh.Fatal("gemini_model_not_found", upstream)),
			status: http.StatusServiceUnavailable, code: "unavailable", issue: "gemini_model_not_found",
		},
		{
			name:   "transient",
			err:    runmesh.Retry("unclassified", upstream),
			status: http.StatusServiceUnavailable, code: "unavailable", issue: "unclassified",
			retryAfter: "1",
		},
	}

	for _, tc := range cases {
		for _, path := range []string{"/api/v1/plans", "/api/v1/goals"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				t.Parallel()

				f := newFixture(t, withPlanner(&stubPlanner{
					err: tc.err,
					result: planner.Result{Trace: planner.Trace{
						Model:    "scripted",
						Attempts: []planner.Attempt{{N: 1}},
					}},
				}))

				rec := f.do(http.MethodPost, path, `{"goal":"anything"}`, nil)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d. Body: %s", rec.Code, tc.status, rec.Body.String())
				}
				if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
					t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
				}

				var body struct {
					Error struct {
						Code    string           `json:"code"`
						Message string           `json:"message"`
						Details []runmesh.Detail `json:"details"`
					} `json:"error"`
					Trace planner.Trace `json:"trace"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decoding: %v", err)
				}
				if body.Error.Code != tc.code {
					t.Errorf("code = %q, want %q", body.Error.Code, tc.code)
				}
				if len(body.Error.Details) != 1 || body.Error.Details[0].Issue != tc.issue {
					t.Errorf("details = %+v, want the provider's code %q: it is what tells an "+
						"operator which setting to look at", body.Error.Details, tc.issue)
				}
				if strings.Contains(body.Error.Message, upstream) {
					t.Error("the provider's own message reached the caller; it belongs in the log")
				}
				if len(body.Trace.Attempts) != 1 {
					t.Errorf("the response carries %d attempts, want the trace: a failed "+
						"planning call has to be diagnosable", len(body.Trace.Attempts))
				}
			})
		}
	}
}
