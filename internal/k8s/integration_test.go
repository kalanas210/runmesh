package k8s_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/k8s"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// These run against a REAL cluster, and they exist for the things a fake
// clientset structurally cannot check: that the API server accepts the spec
// this package generates, that a pod really starts, that a container's stdout
// really comes back through the logs subresource, and that the RBAC Role grants
// what the executor needs and nothing more.
//
// They skip without RUNMESH_TEST_KUBECONTEXT, so `go test ./...` still works on
// a machine with no cluster — the same bargain the PostgreSQL suite makes, for
// the same reason.
//
//	kind create cluster --config deploy/kind/cluster.yaml
//	kubectl apply -f deploy/kubernetes/00-namespaces.yaml -f deploy/kubernetes/10-rbac.yaml
//	docker build -f deploy/docker/task.Dockerfile -t runmesh/task:dev .
//	kind load docker-image runmesh/task:dev --name runmesh
//	RUNMESH_TEST_KUBECONTEXT=kind-runmesh go test ./internal/k8s/ -run Integration

const testTaskImage = "runmesh/task:dev"

func integrationConfig(t *testing.T) (k8s.Config, *k8s.Executor) {
	t.Helper()

	kubecontext := os.Getenv("RUNMESH_TEST_KUBECONTEXT")
	if kubecontext == "" {
		t.Skip("set RUNMESH_TEST_KUBECONTEXT (e.g. kind-runmesh) to run the Kubernetes integration tests")
	}
	image := os.Getenv("RUNMESH_TEST_TASK_IMAGE")
	if image == "" {
		image = testTaskImage
	}

	client, err := k8s.NewClient(os.Getenv("RUNMESH_TEST_KUBECONFIG"), kubecontext)
	if err != nil {
		t.Fatalf("connecting to %s: %v", kubecontext, err)
	}

	cfg := k8s.Config{
		Namespace:          "runmesh-tasks",
		Image:              image,
		TaskServiceAccount: "runmesh-task",
		// Short, because these tests wait for real pods and a slow poll is
		// just added wall-clock.
		PollInterval: 250 * time.Millisecond,
		// Short, so a leaked workload from a failing test is reclaimed quickly
		// rather than sitting in the namespace confusing the next run.
		TTLAfterFinished: 30 * time.Second,
		DeadlineGrace:    30 * time.Second,
		MaxLogBytes:      1 << 20,
		CleanupTimeout:   15 * time.Second,
		Owner:            "integration-test",
	}
	exec, err := k8s.NewExecutor(client, cfg, clock.System(),
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return cfg, exec
}

func integrationInput(tool, stepID string, params string, attempt int) tools.Input {
	jobID := runmesh.NewID("job_", time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	in := tools.Input{
		JobID:          jobID,
		StepID:         stepID,
		Attempt:        attempt,
		AttemptID:      runmesh.AttemptID(jobID, stepID, attempt),
		IdempotencyKey: runmesh.IdempotencyKey(jobID, stepID),
		Tool:           tool,
		Limits: tools.Limits{
			Timeout:        2 * time.Minute,
			MaxAttempts:    3,
			CPU:            "200m",
			Memory:         "64Mi",
			MaxOutputBytes: 1 << 16,
		},
	}
	if params != "" {
		in.Params = json.RawMessage(params)
	}
	return in
}

// TestIntegrationRunsAStepInAPod is the Week-3 acceptance test: a step goes in
// as a lease and comes back as a result, having actually executed in an
// isolated workload on a real cluster.
func TestIntegrationRunsAStepInAPod(t *testing.T) {
	_, exec := integrationConfig(t)

	ctx, cancel := clock.System().WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	in := integrationInput("echo", "greet", `{"msg":"hello from a pod"}`, 1)
	out, err := exec.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The result came out of the container's stdout, through the logs
	// subresource, and parsed. That whole path is what this asserts.
	if len(out.Result) == 0 {
		t.Fatal("no result came back from the pod")
	}
	var got struct {
		Echo    map[string]any `json:"echo"`
		JobID   string         `json:"job_id"`
		StepID  string         `json:"step_id"`
		Attempt int            `json:"attempt"`
	}
	if err := json.Unmarshal(out.Result, &got); err != nil {
		t.Fatalf("the result is not the shape the task image documents: %v (%s)", err, out.Result)
	}
	if got.JobID != in.JobID || got.StepID != in.StepID || got.Attempt != in.Attempt {
		t.Errorf("the pod reported job=%s step=%s attempt=%d, want %s/%s/%d",
			got.JobID, got.StepID, got.Attempt, in.JobID, in.StepID, in.Attempt)
	}
	if got.Echo["msg"] != "hello from a pod" {
		t.Errorf("params did not reach the container: %v", got.Echo)
	}
	if out.Meta["k8s_job"] == "" {
		t.Error("the outcome does not name the workload that produced it")
	}
}

// TestIntegrationClassifiesARealFailure: a container that exits non-zero must
// become a TERMINAL outcome, not a retryable one. Under an at-least-once
// contract, silently retrying what nobody classified is how a side-effecting
// tool runs three times.
func TestIntegrationClassifiesARealFailure(t *testing.T) {
	_, exec := integrationConfig(t)

	ctx, cancel := clock.System().WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// fail_times=5 with attempt=1 means this attempt exits non-zero.
	in := integrationInput("fail", "boom", `{"fail_times":5}`, 1)
	_, err := exec.Execute(ctx, in)

	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v, want a classified *runmesh.ToolError", err)
	}
	if te.Retryable {
		t.Errorf("a container that exited non-zero was classified retryable: %v", te)
	}
}

// TestIntegrationLabelsAreQueryable proves the identity story end to end: the
// Kubernetes object name is a lossy hash, so an operator has to be able to find
// a workload by the RunMesh ids they actually have.
func TestIntegrationLabelsAreQueryable(t *testing.T) {
	cfg, exec := integrationConfig(t)

	ctx, cancel := clock.System().WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	in := integrationInput("echo", "labelled", `{}`, 1)
	if _, err := exec.Execute(ctx, in); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	client, err := k8s.NewClient(os.Getenv("RUNMESH_TEST_KUBECONFIG"), os.Getenv("RUNMESH_TEST_KUBECONTEXT"))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	list, err := client.BatchV1().Jobs(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelJobID + "=" + in.JobID,
	})
	if err != nil {
		t.Fatalf("listing by label: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("selecting on %s=%s found %d Jobs, want 1",
			k8s.LabelJobID, in.JobID, len(list.Items))
	}
	job := list.Items[0]
	if got := job.Annotations[k8s.AnnAttemptID]; got != in.AttemptID {
		t.Errorf("annotation %s = %q, want the untruncated %q",
			k8s.AnnAttemptID, got, in.AttemptID)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("the API server holds a Job whose backoffLimit is not 0")
	}
	// Left to TTL rather than deleted, so its logs survive for debugging.
	t.Cleanup(func() {
		cctx, ccancel := clock.WithWriteDeadline(context.Background(), 30*time.Second)
		defer ccancel()
		_ = client.BatchV1().Jobs(cfg.Namespace).Delete(cctx, job.Name, metav1.DeleteOptions{})
	})
}

// TestIntegrationCancellationDeletesTheWorkload. A cancelled step whose pod
// keeps running is the orphan the whole cleanup design exists to prevent, and
// it is invisible unless somebody looks at the namespace.
func TestIntegrationCancellationDeletesTheWorkload(t *testing.T) {
	cfg, exec := integrationConfig(t)

	client, err := k8s.NewClient(os.Getenv("RUNMESH_TEST_KUBECONFIG"), os.Getenv("RUNMESH_TEST_KUBECONTEXT"))
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	in := integrationInput("sleep", "long", `{"duration":"120s"}`, 1)
	name := k8s.JobName(in.AttemptID)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, e := exec.Execute(ctx, in); done <- e }()

	// Wait until the Job exists, then cancel the way a drain or an operator
	// cancel would.
	deadline := clock.System().After(90 * time.Second)
	created := false
	for !created {
		select {
		case <-deadline:
			cancel()
			t.Fatal("the Job never appeared")
		default:
		}
		if _, err := client.BatchV1().Jobs(cfg.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			created = true
			break
		}
		_ = clock.System().Sleep(ctx, 250*time.Millisecond)
	}
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute returned %v, want context.Canceled", err)
	}

	// The delete runs on a detached context, so it should already have landed.
	gone := false
	for range 40 {
		_, err := client.BatchV1().Jobs(cfg.Namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil && strings.Contains(err.Error(), "not found") {
			gone = true
			break
		}
		_ = clock.System().Sleep(context.Background(), 250*time.Millisecond)
	}
	if !gone {
		t.Fatalf("Job %s still exists after cancellation; its pod is an orphan", name)
	}
}
