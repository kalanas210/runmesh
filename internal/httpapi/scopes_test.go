package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/httpapi"
)

// scopedFixture builds a server whose only key carries exactly the given
// scopes, so a test can ask what that key can and cannot reach.
func scopedFixture(t *testing.T, scopes ...config.Scope) *fixture {
	t.Helper()
	set := make(map[config.Scope]bool, len(scopes))
	for _, s := range scopes {
		set[s] = true
	}
	return newFixture(t, func(d *httpapi.Deps) {
		d.APIKeys = map[[32]byte]config.APIKey{
			config.KeyDigest(apiKey): {ID: "scoped", Scopes: set},
		}
		// A planner, so the planning routes reach their handlers when the
		// scope allows it. Without one they would answer 501 for every scope,
		// and the matrix would report green for a route it never exercised.
		d.Planner = &stubPlanner{result: sampleResult()}
	})
}

// TestScopeMatrix walks every route against every scope.
//
// A table rather than a handful of spot checks, because the failure mode of
// scoped auth is not "the check is wrong" — it is "somebody added a route and
// nobody noticed it was reachable by a read-only key". Enumerating the whole
// matrix means a new route with the wrong scope fails here.
func TestScopeMatrix(t *testing.T) {
	t.Parallel()

	// Each request is one that would SUCCEED with sufficient scope, so a 403
	// can only come from the scope check rather than from a bad request.
	type call struct {
		name    string
		method  string
		target  string
		body    string
		granted config.Scope
	}
	calls := []call{
		{"submit", http.MethodPost, "/api/v1/jobs",
			`{"name":"j","steps":[{"id":"a","tool":"echo"}]}`, config.ScopeJobsWrite},
		{"list", http.MethodGet, "/api/v1/jobs", "", config.ScopeJobsRead},
		{"get", http.MethodGet, "/api/v1/jobs/job_missing", "", config.ScopeJobsRead},
		{"events", http.MethodGet, "/api/v1/jobs/job_missing/events", "", config.ScopeJobsRead},
		{"cancel", http.MethodPost, "/api/v1/jobs/job_missing/cancel", "", config.ScopeJobsCancel},
		{"tools", http.MethodGet, "/api/v1/tools", "", config.ScopeJobsRead},
		// Both planning routes need jobs.write, the dry run included: it
		// executes nothing but it spends model tokens, and a capability that
		// costs money is a write however little it changes.
		{"plan", http.MethodPost, "/api/v1/plans", `{"goal":"x"}`, config.ScopeJobsWrite},
		{"goal", http.MethodPost, "/api/v1/goals", `{"goal":"x"}`, config.ScopeJobsWrite},
	}

	for _, c := range calls {
		for _, held := range config.AllScopes {
			t.Run(c.name+"/"+string(held), func(t *testing.T) {
				t.Parallel()
				f := scopedFixture(t, held)
				rec := f.do(c.method, c.target, c.body, nil)

				allowed := held == c.granted || held == config.ScopeAdmin
				if allowed && rec.Code == http.StatusForbidden {
					t.Fatalf("%s with %s = 403, want it allowed", c.name, held)
				}
				if !allowed {
					if rec.Code != http.StatusForbidden {
						t.Fatalf("%s with only %s = %d, want 403", c.name, held, rec.Code)
					}
					env := decodeEnvelope(t, rec)
					if env.Error.Code != httpapi.CodePermissionDenied {
						t.Errorf("code = %q, want %q", env.Error.Code, httpapi.CodePermissionDenied)
					}
					// An already-authenticated caller learns what it would need.
					// Authentication failures stay uninformative; authorisation
					// failures do not have to be.
					if !strings.Contains(env.Error.Message, string(c.granted)) {
						t.Errorf("message %q does not name the missing scope %q",
							env.Error.Message, c.granted)
					}
				}
			})
		}
	}
}

// TestForbiddenIsNotUnauthorised: the two are different answers to different
// questions, and conflating them makes a client retry for ever with a
// credential that will never work.
func TestForbiddenIsNotUnauthorised(t *testing.T) {
	t.Parallel()

	f := scopedFixture(t, config.ScopeJobsRead)

	rec := f.do(http.MethodPost, "/api/v1/jobs",
		`{"name":"j","steps":[{"id":"a","tool":"echo"}]}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a valid key without jobs.write = %d, want 403", rec.Code)
	}

	rec = f.do(http.MethodGet, "/api/v1/jobs", "", map[string]string{"Authorization": "Bearer wrong-key-value"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an invalid key = %d, want 401", rec.Code)
	}
}

// TestProbesStayPublicUnderScopes: readiness and liveness must not need a
// credential, or a load balancer cannot take an instance out of rotation.
func TestProbesStayPublicUnderScopes(t *testing.T) {
	t.Parallel()

	f := scopedFixture(t) // a key with no scopes at all
	for _, path := range []string{"/api/v1/health", "/api/v1/ready"} {
		rec := f.do(http.MethodGet, path, "", map[string]string{"Authorization": ""})
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s = %d without a credential, want it public", path, rec.Code)
		}
	}
}

// TestExemptionIsExactNotPrefix: the public paths used to be matched by prefix,
// which would have exempted any future path that merely started with one of
// them. Deriving the exemption from the route table made it exact; this holds
// that, because "/api/v1/readyz" quietly needing no key is precisely the kind
// of thing nobody notices.
func TestExemptionIsExactNotPrefix(t *testing.T) {
	t.Parallel()

	f := scopedFixture(t, config.ScopeJobsRead)
	rec := f.do(http.MethodGet, "/api/v1/readyz", "", map[string]string{"Authorization": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/readyz without a credential = %d, want 401: only the exact "+
			"probe paths are exempt", rec.Code)
	}
}
