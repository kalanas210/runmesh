// Package runmesh is the domain. It depends on the standard library and
// nothing else: no clock, no IO, no logging, no store. Every other package
// imports it; it imports none of them.
package runmesh

import "fmt"

// State is the lifecycle state of a job or of a single step. These are exactly
// the eight lifecycle states. There is deliberately no BLOCKED
// or PENDING state: "dependencies unsatisfied" is a query predicate (see
// ClaimRequest), never a stored value, so there is nothing that can drift out
// of sync with reality.
type State uint8

const (
	Unknown   State = iota
	Queued          // step: exists; claimable once every dependency is SUCCEEDED
	Scheduled       // step: leased by a worker, tool not yet invoked
	Running         // step: tool executing
	Retrying        // step: failed retryably, waiting out backoff
	Succeeded
	Failed
	Cancelled
	TimedOut
)

// AllStates lets tests enumerate the domain exhaustively.
var AllStates = []State{
	Unknown, Queued, Scheduled, Running, Retrying,
	Succeeded, Failed, Cancelled, TimedOut,
}

// String is a switch rather than an indexed table so that inserting a state
// cannot silently reorder the wire vocabulary.
func (s State) String() string {
	switch s {
	case Queued:
		return "QUEUED"
	case Scheduled:
		return "SCHEDULED"
	case Running:
		return "RUNNING"
	case Retrying:
		return "RETRYING"
	case Succeeded:
		return "SUCCEEDED"
	case Failed:
		return "FAILED"
	case Cancelled:
		return "CANCELLED"
	case TimedOut:
		return "TIMED_OUT"
	default:
		return "UNKNOWN"
	}
}

var stateByName = map[string]State{
	"QUEUED":    Queued,
	"SCHEDULED": Scheduled,
	"RUNNING":   Running,
	"RETRYING":  Retrying,
	"SUCCEEDED": Succeeded,
	"FAILED":    Failed,
	"CANCELLED": Cancelled,
	"TIMED_OUT": TimedOut,
}

// ParseState is the inverse of String. It rejects "UNKNOWN" as well as
// anything unrecognised: UNKNOWN is a zero value, never a wire value.
func ParseState(s string) (State, error) {
	if v, ok := stateByName[s]; ok {
		return v, nil
	}
	return Unknown, fmt.Errorf("runmesh: unknown state %q", s)
}

// Terminal is an explicit switch, not a comparison against the enum ordering,
// so reordering the constants cannot silently change what "terminal" means.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, TimedOut:
		return true
	default:
		return false
	}
}

// Active reports the states in which a worker holds a lease on the step.
func (s State) Active() bool { return s == Scheduled || s == Running }

func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *State) UnmarshalText(b []byte) error {
	v, err := ParseState(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// stepEdges and jobEdges ARE the state machine, expressed as data so a table
// test can walk every (from, to) pair against an independently written
// expectation matrix. A typo here becomes a test failure rather than a silent
// policy change.
//
// Job SCHEDULED is reserved for Week 3 (admitted by the policy engine but no
// capacity yet) and has no writer in Week 1 — an unused constant is better
// than a state nothing produces.
var stepEdges = map[State][]State{
	Queued:    {Scheduled, Cancelled},
	Scheduled: {Running, Queued, Retrying, Succeeded, Failed, TimedOut, Cancelled},
	Running:   {Queued, Retrying, Succeeded, Failed, TimedOut, Cancelled},
	Retrying:  {Scheduled, Cancelled},
}

var jobEdges = map[State][]State{
	Queued:  {Running, Succeeded, Failed, TimedOut, Cancelled},
	Running: {Succeeded, Failed, TimedOut, Cancelled},
}

// CanStep reports whether a step may move from -> to. Terminal states are
// absorbing: they appear as no map key, so every transition out of one is
// rejected.
func CanStep(from, to State) bool { return hasEdge(stepEdges, from, to) }

// CanJob reports whether a job may move from -> to.
func CanJob(from, to State) bool { return hasEdge(jobEdges, from, to) }

func hasEdge(edges map[State][]State, from, to State) bool {
	for _, s := range edges[from] {
		if s == to {
			return true
		}
	}
	return false
}
