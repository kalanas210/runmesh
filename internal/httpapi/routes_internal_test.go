package httpapi

import "testing"

// TestEveryRouteStatesItsScope walks the routing table and fails on a route
// that is public without being one of the probes.
//
// The table makes authorisation auditable; this makes it auditED. The failure
// it exists for is not a wrong scope — the matrix test in scopes_test.go
// catches those — but a route added with the zero value, which is
// indistinguishable from "deliberately public" to everything except a rule that
// names the deliberate ones.
func TestEveryRouteStatesItsScope(t *testing.T) {
	t.Parallel()

	publicByDesign := map[string]bool{
		"GET /api/v1/health": true,
		"GET /api/v1/ready":  true,
	}

	a := &API{}
	seen := make(map[string]bool)
	for _, rt := range a.routes() {
		if seen[rt.pattern] {
			t.Errorf("route %q is registered twice", rt.pattern)
		}
		seen[rt.pattern] = true

		if rt.handler == nil {
			t.Errorf("route %q has no handler", rt.pattern)
		}
		if rt.scope == scopePublic && !publicByDesign[rt.pattern] {
			t.Errorf("route %q requires no scope. If that is intended, add it to "+
				"publicByDesign here; otherwise give it one.", rt.pattern)
		}
		if rt.scope != scopePublic && publicByDesign[rt.pattern] {
			t.Errorf("route %q is listed as a public probe but demands scope %q",
				rt.pattern, rt.scope)
		}
	}
	for pattern := range publicByDesign {
		if !seen[pattern] {
			t.Errorf("probe %q is no longer registered", pattern)
		}
	}
}
