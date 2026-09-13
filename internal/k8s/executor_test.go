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

// podOf builds the pod a Job's controller would have created, labelled the way
// the executor finds a real one. As built, its task exited non-zero on its own;
// mutate turns it into one of the other stories.
func podOf(jobName string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-x7k2p",
			Namespace: "runmesh-tasks",
			Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: containerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error",
				}},
			}},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
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

// TestExecuteClassifiesFailures. Every failed Job here reads the same from the
// Job alone — BackoffLimitExceeded — so each case says what its pod shows, and
// the pod decides between "the task failed" and "the task was taken away".
func TestExecuteClassifiesFailures(t *testing.T) {
	t.Parallel()

	job := JobName(testInput().AttemptID)
	backoff := failedWith("BackoffLimitExceeded", "Job has reached the specified backoff limit")

	cases := []struct {
		name   string
		status batchv1.JobStatus
		// pod is the Job's pod as the executor finds it; nil means it no
		// longer exists.
		pod         *corev1.Pod
		wantCode    string
		wantRetry   bool
		description string
	}{
		{
			name:      "the task exited non-zero",
			status:    backoff,
			pod:       podOf(job, nil),
			wantCode:  runmesh.CodeContractBroken,
			wantRetry: false,
			description: "terminal by default, the same rule the in-process executor " +
				"applies to an unclassified tool error",
		},
		{
			name:      "Kubernetes hit the backstop deadline",
			status:    failedWith("DeadlineExceeded", "Job was active longer than specified deadline"),
			pod:       podOf(job, nil),
			wantCode:  runmesh.CodeTimeout,
			wantRetry: true,
			description: "the backstop only fires when RunMesh was not watching, " +
				"which says nothing bad about the task",
		},
		{
			name:      "failed with no condition yet",
			status:    batchv1.JobStatus{Failed: 1},
			pod:       podOf(job, nil),
			wantCode:  runmesh.CodeContractBroken,
			wantRetry: false,
		},
		{
			name:   "the kernel killed it for memory",
			status: backoff,
			pod: podOf(job, func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 137
				p.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
			}),
			wantCode:  runmesh.CodeContractBroken,
			wantRetry: false,
			description: "the memory limit is the operator's grant, and another attempt " +
				"meets exactly the same one",
		},
		{
			name:   "its pod was deleted",
			status: backoff,
			pod: podOf(job, func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			}),
			wantCode:  runmesh.CodeWorkloadLost,
			wantRetry: true,
			description: "kubectl delete pod, or a drain: nothing in the task failed, " +
				"it was taken away",
		},
		{
			name:        "its pod is already gone",
			status:      backoff,
			pod:         nil,
			wantCode:    runmesh.CodeWorkloadLost,
			wantRetry:   true,
			description: "TTL cleanup cannot have removed it yet, so something deleted it",
		},
		{
			name:   "its pod was evicted",
			status: backoff,
			pod: podOf(job, func(p *corev1.Pod) {
				p.Status.Reason = "Evicted"
				p.Status.Message = "The node was low on resource: memory."
				p.Status.ContainerStatuses = nil
			}),
			wantCode:    runmesh.CodeWorkloadLost,
			wantRetry:   true,
			description: "pressure on the node is not this task failing",
		},
		{
			name:   "its pod was preempted",
			status: backoff,
			pod: podOf(job, func(p *corev1.Pod) {
				p.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
					Reason: "PreemptionByScheduler",
				}}
			}),
			wantCode:    runmesh.CodeWorkloadLost,
			wantRetry:   true,
			description: "Kubernetes says in so many words that it took the pod away",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var objects []runtime.Object
			if tc.pod != nil {
				// A copy per subtest: the fake's tracker writes to what it is
				// given, and these cases run in parallel.
				objects = append(objects, tc.pod.DeepCopy())
			}
			cs := fake.NewClientset(objects...)
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

// TestExecuteKeepsAFailureTerminalWhenThePodCannotBeRead. A retry needs
// evidence that the pod was taken away, and an API error while looking for it
// is not evidence of anything, so the Job's own verdict stands.
func TestExecuteKeepsAFailureTerminalWhenThePodCannotBeRead(t *testing.T) {
	t.Parallel()

	cs := fake.NewClientset()
	alwaysGet(cs, failedWith("BackoffLimitExceeded", "Job has reached the specified backoff limit"))
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is restarting")
	})

	_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
	te := toolError(t, err)
	if te.Retryable || te.Code != runmesh.CodeContractBroken {
		t.Fatalf("got %q (retryable=%v), want a terminal %q: a pod nobody could read "+
			"was treated as evidence that it was taken away",
			te.Code, te.Retryable, runmesh.CodeContractBroken)
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
	if te.Code != runmesh.CodeWorkloadLost {
		t.Errorf("code = %q, want %q: a deleted Job and a deleted pod are the same "+
			"story, and a dashboard should not have to know two names for it",
			te.Code, runmesh.CodeWorkloadLost)
	}
}
