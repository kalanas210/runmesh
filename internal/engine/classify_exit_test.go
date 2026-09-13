package engine_test

import (
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestClassifyKeepsHowATaskExited: the exit a sandboxed executor recorded is
// the reason the step failed, so it has to survive into the persisted error
// rather than stop at the ToolError.
func TestClassifyKeepsHowATaskExited(t *testing.T) {
	t.Parallel()

	exit := &runmesh.ExitInfo{Code: 3, Reason: "Error", LogTail: "KeyError: 'region'"}
	err := runmesh.Fatal(runmesh.CodeTaskExited, "the task exited with code 3")
	err.Exit = exit

	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}
	got := engine.Classify(engine.StopNone, err, 0, 3, b)
	if got.State != runmesh.Failed || got.Error == nil {
		t.Fatalf("disposition = %+v, want FAILED with an error", got)
	}
	if got.Error.Exit == nil || *got.Error.Exit != *exit {
		t.Errorf("error.exit = %+v, want %+v", got.Error.Exit, exit)
	}
}
