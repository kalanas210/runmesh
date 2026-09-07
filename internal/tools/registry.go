package tools

import (
	"encoding/json"
	"sort"
)

// Registry is a map, not a struct with a mutex, because it is built once
// during wiring and never written again. Concurrent reads of a map that is
// never written need no lock, and saying so in the type is better than
// defending against a write that cannot happen.
type Registry map[string]Tool

// Lookup returns the named tool.
func (r Registry) Lookup(name string) (Tool, bool) {
	t, ok := r[name]
	return t, ok
}

// Has is the allowlist predicate handed to runmesh.Plan.Validate, which is
// what makes "only registered tools may run" a property of submission rather
// than of execution.
func (r Registry) Has(name string) bool {
	_, ok := r[name]
	return ok
}

// Names returns the registered tool names, sorted.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for name := range r {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Descriptors backs GET /api/v1/tools and, in Week 5, the catalogue handed to
// Gemini. A tool that does not implement Describer still appears, with a
// name-only entry: an undocumented tool should be visible and obviously
// undocumented, not invisible.
func (r Registry) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(r))
	for _, name := range r.Names() {
		d := Descriptor{Name: name, Execution: ModeInProcess}
		if desc, ok := r[name].(Describer); ok {
			d = desc.Describe()
			if d.Name == "" {
				d.Name = name
			}
			if d.Execution == "" {
				d.Execution = ModeInProcess
			}
		}
		if d.InputSchema == nil {
			d.InputSchema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, d)
	}
	return out
}

// Validate runs a tool's own submit-time validation, if it has any.
func (r Registry) Validate(name string, params json.RawMessage) error {
	t, ok := r[name]
	if !ok {
		return nil // an unknown tool is Plan.Validate's problem, reported there
	}
	if v, ok := t.(Validator); ok {
		return v.Validate(params)
	}
	return nil
}
