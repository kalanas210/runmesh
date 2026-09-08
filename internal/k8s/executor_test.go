package k8s

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// These run against client-go's fake clientset, which is why the executor
// takes kubernetes.Interface. Create, adopt, wait, fail and cancel are all
// exercised here without a cluster — so the integration test against real kind
// is free to prove only the things a fake cannot: that the spec is one the API
// server accepts, that a pod really runs, and that logs really come back.

func newTestExecutor(t *testing.T, cs *fake.Clientset) *Executor {
	t.Helper()
	cfg := testConfig()
	cfg.PollInterval = time.Millisecond
	cfg.CleanupTimeout = 2 * time.Second
	e, err := NewExecutor(cs, cfg, clock.System(),
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return e
}

func jobWithStatus(name string, status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "runmesh-tasks"},
		Status:     status,
	}
}

func succeeded() batchv1.JobStatus {
	return batchv1.JobStatus{
		Succeeded:  1,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
	}
}

func failedWith(reason, message string) batchv1.JobStatus {
	return batchv1.JobStatus{
		Failed: 1,
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: reason, Message: message,
		}},
	}
}

// alwaysGet makes every Get of a Job return the given status, so the wait loop
// is driven deterministically rather than by whatever a controller would have
// done — there is no controller behind a fake clientset.
func alwaysGet(cs *fake.Clientset, status batchv1.JobStatus) {
	cs.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, jobWithStatus(a.(k8stesting.GetAction).GetName(), status), nil
	})
}

func toolError(t *testing.T, err error) *runmesh.ToolError {
	t.Helper()
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want a classified *runmesh.ToolError. The engine's "+
			"classifier must never have to guess", err, err)
	}
	return te
}

func TestExecuteSucceeds(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	alwaysGet(cs, succeeded())

	out, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Meta["k8s_job"] == "" {
		t.Error("the outcome does not record which workload ran it")
	}

	jobs, err := cs.BatchV1().Jobs("runmesh-tasks").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("%d Jobs exist, want exactly 1", len(jobs.Items))
	}
	// Left for ttlSecondsAfterFinished rather than deleted, so its logs survive
	// a few minutes for anyone debugging.
	for _, a := range cs.Actions() {
		if a.Matches("delete", "jobs") {
			t.Error("a successful attempt deleted its own Job; TTL should reclaim it")
		}
	}
}

// TestExecuteAdoptsAnExistingJob is the reason JobName is deterministic. If the
// API server accepts a create and the response is lost, the retry must
// recognise its own work — a random name would have started a second workload
// for one attempt, and both would have run.
func TestExecuteAdoptsAnExistingJob(t *testing.T) {
	t.Parallel()

	in := testInput()
	name := JobName(in.AttemptID)

	cs := fake.NewClientset(jobWithStatus(name, batchv1.JobStatus{}))
	alwaysGet(cs, succeeded())

	if _, err := newTestExecutor(t, cs).Execute(t.Context(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	jobs, err := cs.BatchV1().Jobs("runmesh-tasks").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("%d Jobs exist after adoption, want exactly 1: one attempt must "+
			"never produce two workloads", len(jobs.Items))
	}
}

func TestExecuteClassifiesFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		status      batchv1.JobStatus
		wantCode    string
		wantRetry   bool
		description string
	}{
		{
			name:      "the task exited non-zero",
			status:    failedWith("BackoffLimitExceeded", "Job has reached the specified backoff limit"),
			wantCode:  runmesh.CodeContractBroken,
			wantRetry: false,
			description: "terminal by default, the same rule the in-process executor " +
				"applies to an unclassified tool error",
		},
		{
			name:      "Kubernetes hit the backstop deadline",
			status:    failedWith("DeadlineExceeded", "Job was active longer than specified deadline"),
			wantCode:  runmesh.CodeTimeout,
			wantRetry: true,
			description: "the backstop only fires when RunMesh was not watching, " +
				"which says nothing bad about the task",
		},
		{
			name:      "failed with no condition yet",
			status:    batchv1.JobStatus{Failed: 1},
			wantCode:  runmesh.CodeContractBroken,
			wantRetry: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs := fake.NewClientset()
			alwaysGet(cs, tc.status)

			_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
			te := toolError(t, err)
			if te.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", te.Code, tc.wantCode, tc.description)
			}
			if te.Retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v (%s)", te.Retryable, tc.wantRetry, tc.description)
			}
		})
	}
}

// TestExecuteDeletesTheWorkloadWhenCancelled: the most important moment to
// delete a workload is exactly when this attempt's context has just been
// cancelled, which is also the moment the obvious implementation cannot,
// because it would be using the dead context to do it.
func TestExecuteDeletesTheWorkloadWhenCancelled(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	alwaysGet(cs, batchv1.JobStatus{Active: 1}) // never finishes on its own

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		// Long enough for the create and at least one poll. Through the clock,
		// not time.After: internal/clock's purity test bans the direct call in
		// tests too, and it is right to — a deadline no test can drive is the
		// thing that ban exists to prevent.
		<-clock.System().After(50 * time.Millisecond)
		cancel()
	}()

	_, err := newTestExecutor(t, cs).Execute(ctx, testInput())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled. Naming the KIND of stop is "+
			"the engine's job, not the executor's", err)
	}

	var deleted bool
	for _, a := range cs.Actions() {
		if a.Matches("delete", "jobs") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("the Job was not deleted after cancellation; the pod would keep " +
			"running with nobody watching it")
	}
}

// TestExecuteFailsFastOnRBAC. Getting this wrong in the permissive direction is
// expensive: a missing Role rule that looks retryable burns the whole retry
// budget of every step in the fleet before anyone sees the real cause.
func TestExecuteFailsFastOnRBAC(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "batch", Resource: "jobs"}, "rm-x",
			errors.New(`jobs.batch is forbidden: User "system:serviceaccount:runmesh:runmesh" cannot create resource "jobs"`))
	})

	_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
	te := toolError(t, err)
	if te.Retryable {
		t.Fatalf("an RBAC refusal was classified retryable (%v); no number of "+
			"retries grants a missing Role rule", te)
	}
}

// TestExecuteRetriesTransientAPIFailures is the other half of that judgement:
// the API server being briefly unavailable says nothing about the step.
func TestExecuteRetriesTransientAPIFailures(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("slow down", 1)
	})

	_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
	te := toolError(t, err)
	if !te.Retryable {
		t.Fatalf("a rate-limited API call was classified terminal (%v)", te)
	}
}

// TestExecuteRetriesADisappearedJob: an operator or a namespace cleanup can
// remove the Job mid-flight. The step genuinely did not run, so another attempt
// is the correct response rather than a terminal failure.
func TestExecuteRetriesADisappearedJob(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	cs.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "batch", Resource: "jobs"},
			a.(k8stesting.GetAction).GetName())
	})

	_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
	te := toolError(t, err)
	if !te.Retryable {
		t.Fatalf("a Job deleted mid-flight was classified terminal (%v)", te)
	}
}
