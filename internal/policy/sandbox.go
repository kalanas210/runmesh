package policy

import (
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Sandbox binds the policy engine to a registry, so the rest of the codebase
// can ask "what may this tool NAME do" without carrying both.
//
// It is the single object the engine, the API and the Week-5 planner are all
// handed. That matters more than it sounds: three call sites resolving policy
// three slightly different ways is how a plan gets accepted by the API and then
// refused by the worker, or — much worse — accepted by both and executed with an
// envelope neither of them checked.
type Sandbox struct {
	registry tools.Registry
	engine   *Engine
	// byName is built once. Descriptors() walks and sorts the whole registry,
	// and Limits is on the dispatch path of every single attempt; doing that
	// walk per attempt would be a map lookup rewritten as a linear scan with
	// allocations.
	byName map[string]tools.Descriptor
}

// NewSandbox pairs a registry with a policy.
func NewSandbox(registry tools.Registry, engine *Engine) Sandbox {
	s := Sandbox{registry: registry, engine: engine, byName: map[string]tools.Descriptor{}}
	for _, d := range registry.Descriptors() {
		s.byName[d.Name] = d
	}
	return s
}

// Limits resolves the envelope for one attempt.
//
// An unregistered tool is CodeToolUnknown rather than a policy error: the
// registry is the allowlist, and a name that is not in it never reached a
// policy decision at all. Keeping the two distinct is what lets an operator
// tell "I never installed that tool" from "I switched that tool off".
func (s Sandbox) Limits(tool string, req Request) (tools.Limits, error) {
	d, ok := s.descriptor(tool)
	if !ok {
		return tools.Limits{}, runmesh.Fatal(runmesh.CodeToolUnknown,
			"tool %q is not registered", tool)
	}
	return s.engine.Resolve(d, req)
}

// Allows is the submit-time gate. It is called once per step by the API, before
// anything is persisted, so a plan that names a switched-off tool is a 400 that
// says which switch — rather than a job that runs three attempts and dies.
func (s Sandbox) Allows(tool string) error {
	d, ok := s.descriptor(tool)
	if !ok {
		return runmesh.Fatal(runmesh.CodeToolUnknown, "tool %q is not registered", tool)
	}
	return s.engine.Allows(d)
}

// Descriptors is the effective catalogue: every registered tool, described as
// it will actually behave in THIS deployment, with the refused ones marked.
func (s Sandbox) Descriptors() []tools.Descriptor {
	all := s.registry.Descriptors()
	out := make([]tools.Descriptor, 0, len(all))
	for _, d := range all {
		out = append(out, s.engine.Effective(d))
	}
	return out
}

// Registry exposes the underlying registry for the allowlist predicate plan
// validation takes.
func (s Sandbox) Registry() tools.Registry { return s.registry }

// Mode reports where tools run in this deployment.
func (s Sandbox) Mode() tools.ExecutionMode { return s.engine.Mode() }

func (s Sandbox) descriptor(tool string) (tools.Descriptor, bool) {
	d, ok := s.byName[tool]
	return d, ok
}

// Passthrough is the sandbox used when no policy is configured: it grants
// exactly what was asked for.
//
// It exists so a unit test can build an engine without a policy, and it is NOT
// what the server wires up — cmd/server always constructs a real one, because a
// runtime whose security envelope depends on whether somebody remembered to
// pass a dependency is a runtime with no security envelope.
type Passthrough struct{}

func (Passthrough) Limits(_ string, req Request) (tools.Limits, error) {
	return tools.Limits{
		Timeout:        req.Timeout,
		MaxAttempts:    req.MaxAttempts,
		MaxOutputBytes: req.MaxOutputBytes,
	}, nil
}

// compile-time proof that the two implementations agree on the shape the
// engine depends on.
var (
	_ limiter = Sandbox{}
	_ limiter = Passthrough{}
)

type limiter interface {
	Limits(tool string, req Request) (tools.Limits, error)
}
