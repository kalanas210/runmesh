// Package k8s executes a step as a Kubernetes Job.
//
// It implements tools.Executor and nothing else, which is the point: the
// dispatcher, the worker pool, lease handling, heartbeats, classification and
// the state machine do not change at all between running a tool in-process and
// running it in a pod. That seam was declared in Week 1 and this package is
// the test of whether it was real.
//
// The division of labour, from ADR 0003: RunMesh owns retries, Kubernetes owns
// isolation and lifecycle. Everything here follows from that sentence —
// backoffLimit: 0, restartPolicy: Never, and an activeDeadlineSeconds that is
// deliberately looser than RunMesh's own timeout so the two never race.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Config is everything the executor needs that is not part of an attempt.
type Config struct {
	// Namespace holds the task workloads. One namespace is what the
	// least-privilege Role is scoped to; see deploy/kubernetes/rbac.yaml.
	Namespace string

	// Image is the fallback when a tool descriptor names none.
	Image string

	// TaskServiceAccount is the identity task pods run as. It is deliberately
	// NOT the account RunMesh itself uses, and its token is not mounted.
	TaskServiceAccount string

	TTLAfterFinished time.Duration
	DeadlineGrace    time.Duration

	// PollInterval is how often the Job's status is read. See waitForTerminal
	// for why this polls rather than watches.
	PollInterval time.Duration

	// MaxLogBytes caps what is read back from a container's logs.
	MaxLogBytes int

	// CleanupTimeout bounds the deletes that run after an attempt's own
	// context has already been cancelled.
	CleanupTimeout time.Duration

	// Owner labels the workloads this process created.
	Owner string
}

func (c *Config) setDefaults() {
	if c.Namespace == "" {
		c.Namespace = "runmesh-tasks"
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = 5 * time.Minute
	}
	if c.DeadlineGrace <= 0 {
		c.DeadlineGrace = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.MaxLogBytes <= 0 {
		c.MaxLogBytes = 1 << 20
	}
	if c.CleanupTimeout <= 0 {
		c.CleanupTimeout = 15 * time.Second
	}
}

// Executor creates one Kubernetes Job per attempt, waits for it, and reads its
// result back out of the container's logs.
type Executor struct {
	client kubernetes.Interface
	cfg    Config
	clock  clock.Clock
	log    *slog.Logger
}

var _ tools.Executor = (*Executor)(nil)

// NewExecutor builds an executor over an existing clientset.
//
// It takes kubernetes.Interface rather than *kubernetes.Clientset so the whole
// package is testable against client-go's fake clientset — the create, adopt,
// wait, fail and cancel paths are all exercised without a cluster, which
// leaves the integration test free to prove the things only a real API server
// can prove.
func NewExecutor(client kubernetes.Interface, cfg Config, clk clock.Clock, log *slog.Logger) (*Executor, error) {
	if client == nil {
		return nil, errors.New("k8s: a Kubernetes client is required")
	}
	if clk == nil {
		clk = clock.System()
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.setDefaults()
	return &Executor{client: client, cfg: cfg, clock: clk, log: log.With("component", "k8s")}, nil
}

// Execute runs one attempt as a Job and returns its result.
//
// The shape is deliberately the same as the in-process executor's: every
// failure it produces is a CLASSIFIED *runmesh.ToolError, so engine.Classify
// never has to guess and an unclassified error never silently becomes a
// terminal failure.
func (e *Executor) Execute(ctx context.Context, in tools.Input) (tools.Output, error) {
	spec, err := BuildJob(e.cfg, in, e.cfg.Owner)
	if err != nil {
		return tools.Output{}, err
	}
	name := spec.Name
	log := e.log.With("attempt_id", in.AttemptID, "k8s_job", name, "namespace", e.cfg.Namespace)

	jobs := e.client.BatchV1().Jobs(e.cfg.Namespace)
	created, err := jobs.Create(ctx, spec, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		// The create was accepted and its response was lost. Because JobName is
		// derived from the AttemptID, the retry recognises its own work and
		// adopts it — rather than starting a second workload for one attempt,
		// which is what a random name would have done.
		log.Info("adopting an existing Job for this attempt")
		if created, err = jobs.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return tools.Output{}, classifyAPIError(err, "reading the existing Job")
		}
	case err != nil:
		return tools.Output{}, classifyAPIError(err, "creating the Job")
	}
	_ = created

	// Cleanup runs on a DETACHED context, because the most important time to
	// delete a workload is exactly when this attempt's context has just been
	// cancelled. Using the dead context would leave the pod running with
	// nobody watching it — the orphan this is here to prevent.
	defer func() {
		if ctx.Err() == nil {
			// Finished on its own terms: leave it to ttlSecondsAfterFinished
			// so its logs survive a few minutes for anyone debugging.
			return
		}
		cctx, cancel := clock.WithWriteDeadline(ctx, e.cfg.CleanupTimeout)
		defer cancel()
		if err := e.deleteJob(cctx, name); err != nil {
			log.Warn("could not delete the Job after cancellation", "err", err)
		}
	}()

	job, err := e.waitForTerminal(ctx, name)
	if err != nil {
		return tools.Output{}, err
	}

	// Logs are read even when the Job failed: a failing task's result line
	// often says why, and it is the only diagnostic that survives TTL cleanup.
	result, logErr := e.collectResult(ctx, in, name)

	if failure := jobFailure(job); failure != nil {
		return tools.Output{}, failure
	}
	if logErr != nil {
		return tools.Output{}, logErr
	}
	return tools.Output{
		Result: result,
		Meta:   map[string]string{"k8s_job": name, "namespace": e.cfg.Namespace},
	}, nil
}

// waitForTerminal polls the Job until it succeeds, fails, or the context ends.
//
// Polling rather than watching, and that is a considered choice rather than a
// shortcut. A watch on ONE short-lived object needs resourceVersion handling
// and re-establishment on every expiry or connection drop — real code, with
// real failure modes, whose reward here is sub-second latency on a step that
// takes seconds to schedule a pod. The poll interval is configurable, the
// worst-case error is that much added latency, and there is nothing to get
// wrong when the connection blips. An informer earns its keep when many
// objects are watched by many consumers; this is one object watched by one
// goroutine that is about to exit.
func (e *Executor) waitForTerminal(ctx context.Context, name string) (*batchv1.Job, error) {
	jobs := e.client.BatchV1().Jobs(e.cfg.Namespace)
	for {
		job, err := jobs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// Somebody deleted the Job out from under us — an operator, or a
			// namespace cleanup. Retryable: the step genuinely did not run, and
			// a fresh attempt is the correct response.
			return nil, runmesh.Retry(runmesh.CodeUnclassified,
				"the Job disappeared before it finished")
		case err != nil:
			return nil, classifyAPIError(err, "reading the Job")
		}
		if terminal(job) {
			return job, nil
		}
		if err := e.clock.Sleep(ctx, e.cfg.PollInterval); err != nil {
			// The context ended. Which KIND of ending it was — a step timeout,
			// a cancellation, a drain — is the engine's to decide; it holds the
			// heartbeat directive and the shutdown signal, and the worker
			// records why it stopped BEFORE cancelling this context. Guessing
			// here is how a cancelled step gets classified as a retryable
			// failure.
			return nil, ctx.Err()
		}
	}
}

// terminal reports whether the Job has stopped for good.
//
// It reads the CONDITIONS rather than only the counters, because a Job killed
// by activeDeadlineSeconds reports Failed with a distinct reason, and telling
// "the task failed" from "Kubernetes killed the task for running too long"
// changes what an operator does next.
func terminal(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
		return true
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobFailed:
			return true
		}
	}
	return false
}

// jobFailure turns a failed Job into a classified error, or returns nil if it
// succeeded.
func jobFailure(job *batchv1.Job) error {
	if job == nil {
		return nil
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Reason == "DeadlineExceeded" {
				// Kubernetes killed it for exceeding activeDeadlineSeconds.
				// That deadline is the BACKSTOP — RunMesh's own timeout is
				// shorter and should have fired first — so reaching it means
				// RunMesh was not watching, which is a retryable situation
				// rather than a fault in the task.
				return runmesh.Retry(runmesh.CodeTimeout,
					"Kubernetes stopped the workload at its backstop deadline")
			}
			return runmesh.Fatal(runmesh.CodeContractBroken,
				"the workload failed: %s: %s", c.Reason, c.Message)
		}
	}
	if job.Status.Failed > 0 {
		// A non-zero exit with no condition yet. Terminal by default, which is
		// the same rule the in-process executor applies to an unclassified
		// tool error: under an at-least-once contract, silently retrying an
		// error nobody classified is how a side-effecting tool runs three
		// times.
		return runmesh.Fatal(runmesh.CodeContractBroken,
			"the task container exited non-zero")
	}
	return nil
}

// collectResult reads the pod's logs and extracts the result line.
func (e *Executor) collectResult(ctx context.Context, in tools.Input, jobName string) (json.RawMessage, error) {
	pod, err := e.podFor(ctx, jobName)
	if err != nil {
		return nil, err
	}
	if pod == "" {
		// The Job finished but its pod is already gone — TTL, eviction, or a
		// node that went away. Not an error in itself: a step whose container
		// exited 0 without a result line is legitimate.
		return nil, nil
	}

	limit := in.Limits.MaxOutputBytes
	if limit <= 0 || limit > e.cfg.MaxLogBytes {
		limit = e.cfg.MaxLogBytes
	}

	req := e.client.CoreV1().Pods(e.cfg.Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName,
		// One byte past the cap so ParseResult can tell a stream that ended
		// from one that was cut off, and report the difference.
		LimitBytes: ptrTo(int64(limit) + 1),
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, classifyAPIError(err, "reading container logs")
	}
	defer func() { _ = stream.Close() }()

	result, err := ParseResult(stream, limit)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// podFor finds the pod a Job created.
//
// The selector is batch.kubernetes.io/job-name, the modern label; job-name is
// its deprecated alias and is matched too so an older cluster still works.
func (e *Executor) podFor(ctx context.Context, jobName string) (string, error) {
	for _, selector := range []string{
		"batch.kubernetes.io/job-name=" + jobName,
		"job-name=" + jobName,
	} {
		list, err := e.client.CoreV1().Pods(e.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			Limit:         10,
		})
		if err != nil {
			return "", classifyAPIError(err, "listing the Job's pods")
		}
		if len(list.Items) > 0 {
			// backoffLimit: 0 means at most one pod, so the first is the only.
			return list.Items[0].Name, nil
		}
	}
	return "", nil
}

// deleteJob removes a Job and the pods it owns.
//
// PropagationPolicy is Background rather than the default Orphan: deleting a
// Job and leaving its pod running is how a "cancelled" step keeps consuming a
// node, and it is invisible until somebody looks at the namespace.
func (e *Executor) deleteJob(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationBackground
	err := e.client.BatchV1().Jobs(e.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// classifyAPIError decides whether a Kubernetes API failure is worth another
// attempt. Getting this wrong in the permissive direction is expensive: an
// RBAC mistake that looks retryable burns the whole budget of every step in
// the fleet before anybody sees the real cause.
func classifyAPIError(err error, doing string) error {
	if err == nil {
		return nil
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		// A missing Role rule. No number of retries will grant it, and failing
		// fast puts the RBAC error in front of somebody immediately.
		return runmesh.Fatal(runmesh.CodeContractBroken,
			"%s: refused by Kubernetes RBAC: %v", doing, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The spec RunMesh generated is wrong. That is a bug here, not a blip.
		return runmesh.Fatal(runmesh.CodeContractBroken, "%s: %v", doing, err)
	case apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err),
		apierrors.IsTimeout(err), apierrors.IsInternalError(err),
		apierrors.IsServiceUnavailable(err), apierrors.IsConflict(err):
		return runmesh.Retry(runmesh.CodeUnclassified, "%s: %v", doing, err)
	default:
		// Network-shaped failures reach here. Retryable: the API server being
		// briefly unreachable says nothing about the step.
		return runmesh.Retry(runmesh.CodeUnclassified, "%s: %v", doing, err)
	}
}

// marshalDeps renders upstream results for the container's environment.
func marshalDeps(deps map[string]json.RawMessage) (string, error) {
	b, err := json.Marshal(deps)
	if err != nil {
		return "", fmt.Errorf("k8s: encoding dependency results: %w", err)
	}
	return string(b), nil
}
