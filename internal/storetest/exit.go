package storetest

import (
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// testErrorExitRoundTrips: how a task container stopped is part of a failed
// step's error, and it must come back out of the store exactly as it went in —
// through a jsonb column in PostgreSQL, and through a clone in memory.
func testErrorExitRoundTrips(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	l := mustClaim(t, s, epoch, 1)[0]
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := runmesh.ExitInfo{Code: 137, Reason: "OOMKilled", LogTail: "loading rows\nKilled"}
	err := s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Failed, CountFail: true,
		Error: &runmesh.ErrorInfo{
			Code:    runmesh.CodeTaskOOMKilled,
			Message: "the task was killed at its memory limit of 128Mi",
			Attempt: 1,
			Exit:    &want,
		},
		StartedAt: epoch, EndedAt: epoch,
	})
	if err != nil {
		t.Fatalf("Finish(FAILED): %v", err)
	}

	j, err := s.Job(t.Context(), "job_a")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	got := j.Step("a").Error
	if got == nil || got.Exit == nil {
		t.Fatalf("stored error = %+v, want one that says how the task exited", got)
	}
	if *got.Exit != want {
		t.Errorf("stored exit = %+v, want %+v", *got.Exit, want)
	}
}
