package k8s_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/k8s"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// The tests in this file need more than one node, which deploy/kind/cluster.yaml
// provides (one control plane, two workers), and skip on a cluster with fewer
// schedulable nodes.

// TestIntegrationAJobsParallelStepsSpreadAcrossNodes: two steps of one job that
// run at the same time should not share a node, so that losing a node costs the
// job one of them rather than both. The spread is a preference
// (ScheduleAnyway), and with two identical, idle workers the scheduler has no
// reason not to honour it.
func TestIntegrationAJobsParallelStepsSpreadAcrossNodes(t *testing.T) {
	cfg, exec := integrationConfig(t)
	client := integrationClient(t)

	if n := schedulableNodes(t, client); n < 2 {
		t.Skipf("%d schedulable nodes: spreading needs two", n)
	}

	ctx, cancel := clock.System().WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	left := integrationInput("sleep", "left", `{"duration":"60s"}`, 1)
	right := left
	right.StepID = "right"
	right.AttemptID = runmesh.AttemptID(left.JobID, right.StepID, 1)
	right.IdempotencyKey = runmesh.IdempotencyKey(left.JobID, right.StepID)

	done := make(chan struct{}, 2)
	for _, in := range []tools.Input{left, right} {
		go func() {
			_, _ = exec.Execute(ctx, in)
			done <- struct{}{}
		}()
	}

	leftNode := runningPod(ctx, t, client, cfg.Namespace, k8s.JobName(left.AttemptID)).Spec.NodeName
	rightNode := runningPod(ctx, t, client, cfg.Namespace, k8s.JobName(right.AttemptID)).Spec.NodeName

	// Cancelling makes the executor delete both workloads; wait for it, so this
	// test leaves nothing running for the next one.
	cancel()
	<-done
	<-done

	if leftNode == rightNode {
		t.Fatalf("both steps of one job ran on %s: the topology spread on %s did not apply",
			leftNode, k8s.LabelJobID)
	}
}

// TestIntegrationADrainedNodesStepRunsElsewhere is node failure the way
// `kubectl drain` delivers it: the node is cordoned, the task pod on it is
// evicted through the Eviction API, and the step must come back as a retryable
// workload_lost whose next attempt runs on a different node.
func TestIntegrationADrainedNodesStepRunsElsewhere(t *testing.T) {
	cfg, exec := integrationConfig(t)
	client := integrationClient(t)

	if n := schedulableNodes(t, client); n < 2 {
		t.Skipf("%d schedulable nodes: draining one needs another to move to", n)
	}

	ctx, cancel := clock.System().WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	first := integrationInput("sleep", "drained", `{"duration":"120s"}`, 1)
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, first)
		done <- err
	}()

	pod := runningPod(ctx, t, client, cfg.Namespace, k8s.JobName(first.AttemptID))
	node := pod.Spec.NodeName

	// kubectl drain, in its two API calls: cordon the node, then evict.
	setUnschedulable(ctx, t, client, node, true)
	t.Cleanup(func() {
		cctx, cancel := clock.System().WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		setUnschedulable(cctx, t, client, node, false)
	})
	eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: cfg.Namespace}}
	if err := client.PolicyV1().Evictions(cfg.Namespace).Evict(ctx, eviction); err != nil {
		t.Fatalf("evicting %s from %s: %v", pod.Name, node, err)
	}

	err := <-done
	var te *runmesh.ToolError
	if !errors.As(err, &te) || !te.Retryable || te.Code != runmesh.CodeWorkloadLost {
		t.Fatalf("an evicted step returned %v; want a retryable %q, since nothing in "+
			"the task failed", err, runmesh.CodeWorkloadLost)
	}

	// The retry, as the engine would run it: the same step's next attempt.
	second := first
	second.Attempt = 2
	second.AttemptID = runmesh.AttemptID(first.JobID, first.StepID, 2)
	second.Params = json.RawMessage(`{"duration":"1s"}`)
	if _, err := exec.Execute(ctx, second); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	retried := jobPod(ctx, t, client, cfg.Namespace, k8s.JobName(second.AttemptID))
	if retried.Spec.NodeName == node {
		t.Fatalf("the retry ran on %s, the node that was drained", node)
	}
}

// schedulableNodes counts the nodes a task pod could land on: not cordoned, and
// not tainted against scheduling, which rules out kind's control plane.
func schedulableNodes(t *testing.T, client *kubernetes.Clientset) int {
	t.Helper()
	nodes, err := client.CoreV1().Nodes().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	n := 0
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable {
			continue
		}
		usable := true
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				usable = false
			}
		}
		if usable {
			n++
		}
	}
	return n
}

// runningPod waits until the Job's pod is Running, and returns it.
func runningPod(ctx context.Context, t *testing.T, client *kubernetes.Clientset, namespace, jobName string) *corev1.Pod {
	t.Helper()
	for {
		list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
		})
		if err == nil {
			for i := range list.Items {
				if list.Items[i].Status.Phase == corev1.PodRunning {
					return &list.Items[i]
				}
			}
		}
		if err := clock.System().Sleep(ctx, 250*time.Millisecond); err != nil {
			t.Fatalf("the pod of %s never reached Running", jobName)
		}
	}
}

// jobPod returns the pod a finished Job ran in.
func jobPod(ctx context.Context, t *testing.T, client *kubernetes.Clientset, namespace, jobName string) *corev1.Pod {
	t.Helper()
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
	})
	if err != nil || len(list.Items) == 0 {
		t.Fatalf("finding the pod of %s: %v (%d pods)", jobName, err, len(list.Items))
	}
	return &list.Items[0]
}

// setUnschedulable cordons or uncordons a node, as `kubectl cordon` does.
func setUnschedulable(ctx context.Context, t *testing.T, client *kubernetes.Clientset, node string, on bool) {
	t.Helper()
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, on)
	if _, err := client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType,
		[]byte(patch), metav1.PatchOptions{}); err != nil {
		t.Errorf("setting unschedulable=%t on %s: %v", on, node, err)
	}
}
