package runmesh_test

import (
	"encoding/json"
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// legalStepEdges and legalJobEdges are written out longhand, independently of
// the production tables.
//
// Duplicating the data is normally a test smell; here it is the point. The
// production table IS the policy, so a test that derived its expectation from
// it would pass for any typo. Written by hand, the test is the specification
// and a typo in the map becomes a failure rather than a silent change to what
// the runtime is allowed to do.
var legalStepEdges = map[string]bool{
	// A step is created QUEUED. It can be claimed, or cancelled before it runs.
	"QUEUED->SCHEDULED": true,
	"QUEUED->CANCELLED": true,

	// Claimed but not yet executing: it may start, be given back, settle
	// without ever running (cancelled between claim and start), or fail.
	"SCHEDULED->RUNNING":   true,
	"SCHEDULED->QUEUED":    true, // Release: drain, or a lease given up
	"SCHEDULED->RETRYING":  true,
	"SCHEDULED->SUCCEEDED": true,
	"SCHEDULED->FAILED":    true,
	"SCHEDULED->TIMED_OUT": true,
	"SCHEDULED->CANCELLED": true,

	// Executing.
	"RUNNING->SUCCEEDED": true,
	"RUNNING->FAILED":    true,
	"RUNNING->TIMED_OUT": true,
	"RUNNING->CANCELLED": true,
	"RUNNING->RETRYING":  true,
	"RUNNING->QUEUED":    true, // Release: the drain deadline expired

	// Waiting out a backoff. It becomes claimable again, or is cancelled.
	"RETRYING->SCHEDULED": true,
	"RETRYING->CANCELLED": true,

	// Everything else is illegal. In particular every transition OUT of a
	// terminal state: terminal states are absorbing.
}

var legalJobEdges = map[string]bool{
	"QUEUED->RUNNING":   true,
	"QUEUED->SUCCEEDED": true,
	"QUEUED->FAILED":    true,
	"QUEUED->TIMED_OUT": true,
	"QUEUED->CANCELLED": true,

	"RUNNING->SUCCEEDED": true,
	"RUNNING->FAILED":    true,
	"RUNNING->TIMED_OUT": true,
	"RUNNING->CANCELLED": true,
}

func TestCanStepWalksEveryPair(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, from := range runmesh.AllStates {
		for _, to := range runmesh.AllStates {
			checked++
			key := from.String() + "->" + to.String()
			want := legalStepEdges[key]
			if got := runmesh.CanStep(from, to); got != want {
				t.Errorf("CanStep(%s) = %v, want %v", key, got, want)
			}
		}
	}
	if checked != len(runmesh.AllStates)*len(runmesh.AllStates) {
		t.Fatalf("walked %d pairs, want %d", checked, len(runmesh.AllStates)*len(runmesh.AllStates))
	}
}

func TestCanJobWalksEveryPair(t *testing.T) {
	t.Parallel()
	for _, from := range runmesh.AllStates {
		for _, to := range runmesh.AllStates {
			key := from.String() + "->" + to.String()
			want := legalJobEdges[key]
			if got := runmesh.CanJob(from, to); got != want {
				t.Errorf("CanJob(%s) = %v, want %v", key, got, want)
			}
		}
	}
}

// TestTerminalStatesAreAbsorbing states the rule that makes every other guard
// safe: once a step or job is terminal, nothing may move it.
func TestTerminalStatesAreAbsorbing(t *testing.T) {
	t.Parallel()
	for _, from := range runmesh.AllStates {
		if !from.Terminal() {
			continue
		}
		for _, to := range runmesh.AllStates {
			if runmesh.CanStep(from, to) {
				t.Errorf("CanStep(%s->%s) is allowed; terminal states must be absorbing", from, to)
			}
			if runmesh.CanJob(from, to) {
				t.Errorf("CanJob(%s->%s) is allowed; terminal states must be absorbing", from, to)
			}
		}
	}
}

func TestStateClassification(t *testing.T) {
	t.Parallel()
	terminal := map[runmesh.State]bool{
		runmesh.Succeeded: true, runmesh.Failed: true,
		runmesh.Cancelled: true, runmesh.TimedOut: true,
	}
	active := map[runmesh.State]bool{runmesh.Scheduled: true, runmesh.Running: true}

	for _, s := range runmesh.AllStates {
		if got := s.Terminal(); got != terminal[s] {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, terminal[s])
		}
		if got := s.Active(); got != active[s] {
			t.Errorf("%s.Active() = %v, want %v", s, got, active[s])
		}
		if s.Terminal() && s.Active() {
			t.Errorf("%s is both terminal and active", s)
		}
	}
}

func TestStateJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range runmesh.AllStates {
		if s == runmesh.Unknown {
			continue // UNKNOWN is a zero value, never a wire value
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %s: %v", s, err)
		}
		var got runmesh.State
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != s {
			t.Errorf("round trip of %s produced %s", s, got)
		}
	}

	for _, bad := range []string{`"UNKNOWN"`, `"queued"`, `"PENDING"`, `""`, `"BLOCKED"`} {
		var got runmesh.State
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("unmarshalling %s succeeded as %s; unknown states must be rejected", bad, got)
		}
	}
}

// TestEveryStateHasAWireName guards against adding a state to the enum without
// deciding what it is called on the wire — which would otherwise surface as a
// silent "UNKNOWN" in the dashboard.
func TestEveryStateHasAWireName(t *testing.T) {
	t.Parallel()
	for _, s := range runmesh.AllStates {
		if s == runmesh.Unknown {
			continue
		}
		name := s.String()
		if name == "UNKNOWN" {
			t.Errorf("state %d has no wire name", uint8(s))
			continue
		}
		back, err := runmesh.ParseState(name)
		if err != nil {
			t.Errorf("ParseState(%q): %v", name, err)
			continue
		}
		if back != s {
			t.Errorf("ParseState(%q) = %s, want %s", name, back, s)
		}
	}
}
