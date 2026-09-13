package engine_test

import (
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestClassifyEndsAReportedTimeoutAsTimedOut: a timeout can reach Classify as a
// ToolError rather than as StopTimeout. The Kubernetes backstop deadline is
// one, reported by the executor after RunMesh's own deadline failed to fire.
// Its last attempt must still end TIMED_OUT, as the engine's own timeout does,
// so the job rollup says "this timed out" whichever clock noticed.
func TestClassifyEndsAReportedTimeoutAsTimedOut(t *testing.T) {
	t.Parallel()

	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}
	backstop := runmesh.Retry(runmesh.CodeTimeout, "Kubernetes stopped the workload at its backstop deadline")

	if got := engine.Classify(engine.StopNone, backstop, 0, 3, b); got.State != runmesh.Retrying {
		t.Errorf("with attempts left: state = %s, want RETRYING", got.State)
	}

	got := engine.Classify(engine.StopNone, backstop, 2, 3, b)
	if got.State != runmesh.TimedOut || !got.CountFail {
		t.Errorf("on the last attempt: state = %s (counted=%v), want TIMED_OUT and counted", got.State, got.CountFail)
	}
	if got.Error == nil || got.Error.Code != runmesh.CodeTimeout {
		t.Errorf("error = %+v, want code %q", got.Error, runmesh.CodeTimeout)
	}

	// Only a timeout gets that treatment: any other code on its last attempt is
	// still FAILED.
	other := runmesh.Retry("upstream_503", "gateway")
	if got := engine.Classify(engine.StopNone, other, 2, 3, b); got.State != runmesh.Failed {
		t.Errorf("a non-timeout on the last attempt: state = %s, want FAILED", got.State)
	}
}
