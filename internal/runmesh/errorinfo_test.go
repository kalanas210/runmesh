package runmesh_test

import (
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestErrorInfoCloneSharesNoExit: stores hand out clones so that an event a
// subscriber mutates cannot rewrite a stored step's error. A shallow copy would
// quietly break that the moment ErrorInfo grew a pointer.
func TestErrorInfoCloneSharesNoExit(t *testing.T) {
	t.Parallel()

	orig := &runmesh.ErrorInfo{
		Code: runmesh.CodeTaskExited,
		Exit: &runmesh.ExitInfo{Code: 1, Reason: "Error", LogTail: "boom"},
	}
	c := orig.Clone()
	c.Exit.LogTail = "rewritten"

	if orig.Exit.LogTail != "boom" {
		t.Fatal("a clone shares its Exit with the original")
	}
	if (*runmesh.ErrorInfo)(nil).Clone() != nil {
		t.Fatal("cloning a nil ErrorInfo must stay nil")
	}
}
