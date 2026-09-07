package config

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is one capability an API key may carry.
//
// The vocabulary is deliberately tiny. A scope should name something an
// operator would actually want to hand out separately — and the three that
// pass that test are "may read", "may submit work" and "may stop work". A
// dashboard needs the first. A planner needs the first two. Only a human
// needs the third, and giving a runaway agent the ability to cancel every job
// in the fleet is not a capability anybody meant to grant.
type Scope string

const (
	// ScopeJobsRead covers every GET: the job list, one job, its timeline, and
	// the tool registry.
	ScopeJobsRead Scope = "jobs.read"
	// ScopeJobsWrite covers submitting a plan.
	ScopeJobsWrite Scope = "jobs.write"
	// ScopeJobsCancel covers requesting cancellation. Separate from write on
	// purpose: submitting work and stopping somebody else's are different
	// authorities.
	ScopeJobsCancel Scope = "jobs.cancel"
	// ScopeAdmin implies every other scope. It exists so an operator key does
	// not have to be re-issued each time a scope is added.
	ScopeAdmin Scope = "admin"
)

// AllScopes is the whole vocabulary, in the order it is reported to an
// operator who mistypes one.
var AllScopes = []Scope{ScopeJobsRead, ScopeJobsWrite, ScopeJobsCancel, ScopeAdmin}

func validScope(s Scope) bool {
	for _, v := range AllScopes {
		if v == s {
			return true
		}
	}
	return false
}

// APIKey is one configured credential: what to call it in a log line, and what
// it is allowed to do. The secret itself is never stored — only its digest, as
// the map key.
type APIKey struct {
	// ID is for log correlation and revocation. It is never a credential and
	// it is safe to print.
	ID string

	Scopes map[Scope]bool

	// Unscoped records a key configured with no scope list at all, which is
	// granted everything.
	//
	// That default is a deliberate compromise. Least privilege would say an
	// unscoped key gets nothing — but this variable already exists in every
	// deployment from Week 1, and silently demoting every running key to
	// useless on upgrade is a worse failure than a permissive default. So the
	// old syntax keeps working, and the flag exists so the server can say at
	// boot exactly which keys are unrestricted.
	Unscoped bool
}

// Allows reports whether this key may perform an operation.
func (k APIKey) Allows(want Scope) bool {
	if k.Unscoped || k.Scopes[ScopeAdmin] {
		return true
	}
	return k.Scopes[want]
}

// ScopeList renders the key's scopes for a log line, sorted so the output is
// stable between boots.
func (k APIKey) ScopeList() []string {
	if k.Unscoped {
		return []string{"*"}
	}
	out := make([]string, 0, len(k.Scopes))
	for s := range k.Scopes {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// parseAPIKeyEntry parses one `id[:scope+scope]=secret` entry.
//
// The separators are chosen so the whole variable stays a single shell-safe
// word: entries split on commas, so scopes cannot; and the value may not
// contain a second `=` before the secret, so scopes hang off the id with a
// colon. `dev=key` from Week 1 parses unchanged, as an unscoped key.
func parseAPIKeyEntry(entry string, index int) (APIKey, string, error) {
	left, secret, named := strings.Cut(entry, "=")
	if !named {
		// A bare key with no id at all. Accepted, because the id exists for log
		// correlation rather than for authentication — but naming keys is what
		// lets an operator revoke one without guessing which.
		return APIKey{ID: fmt.Sprintf("key%d", index), Unscoped: true},
			strings.TrimSpace(left), nil
	}
	secret = strings.TrimSpace(secret)

	id, scopeSpec, scoped := strings.Cut(left, ":")
	id = strings.TrimSpace(id)
	if id == "" {
		return APIKey{}, secret, fmt.Errorf("entry %d has an empty key id", index)
	}
	if !scoped {
		return APIKey{ID: id, Unscoped: true}, secret, nil
	}

	key := APIKey{ID: id, Scopes: make(map[Scope]bool)}
	for _, raw := range strings.Split(scopeSpec, "+") {
		s := Scope(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		if !validScope(s) {
			return APIKey{}, secret, fmt.Errorf(
				"key %q requests unknown scope %q; valid scopes are %s",
				id, s, joinScopes(AllScopes))
		}
		key.Scopes[s] = true
	}
	if len(key.Scopes) == 0 {
		return APIKey{}, secret, fmt.Errorf(
			"key %q names no scopes; omit the colon to grant all of them, "+
				"or list some of %s", id, joinScopes(AllScopes))
	}
	return key, secret, nil
}

func joinScopes(scopes []Scope) string {
	parts := make([]string, len(scopes))
	for i, s := range scopes {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}
