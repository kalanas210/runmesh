package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestExecuteRecordsHowTheTaskExited: a failed step carries what
// `kubectl describe pod` and `kubectl logs` would have said, because both are
// gone once the Job's TTL reclaims the pod.
func TestExecuteRecordsHowTheTaskExited(t *testing.T) {
	t.Parallel()

	job := JobName(testInput().AttemptID)
	backoff := failedWith("BackoffLimitExceeded", "Job has reached the specified backoff limit")

	t.Run("a non-zero exit", func(t *testing.T) {
		t.Parallel()
		cs := fake.NewClientset(podOf(job, func(p *corev1.Pod) {
			term := p.Status.ContainerStatuses[0].State.Terminated
			term.ExitCode = 3
			term.Message = "reading sales.csv\nKeyError: 'region'\n"
		}))
		alwaysGet(cs, backoff)

		_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
		te := toolError(t, err)
		if te.Code != runmesh.CodeTaskExited || te.Retryable {
			t.Fatalf("got %q (retryable=%v), want a terminal %q", te.Code, te.Retryable, runmesh.CodeTaskExited)
		}
		if te.Message != "the task exited with code 3" {
			t.Errorf("message = %q, want the exit code and nothing the reason Error adds", te.Message)
		}
		want := runmesh.ExitInfo{Code: 3, Reason: "Error", LogTail: "reading sales.csv\nKeyError: 'region'"}
		if te.Exit == nil || *te.Exit != want {
			t.Errorf("exit = %+v, want %+v", te.Exit, want)
		}
	})

	t.Run("an OOM kill names the limit it hit", func(t *testing.T) {
		t.Parallel()
		cs := fake.NewClientset(podOf(job, func(p *corev1.Pod) {
			term := p.Status.ContainerStatuses[0].State.Terminated
			term.ExitCode, term.Reason = 137, "OOMKilled"
			p.Spec.Containers = []corev1.Container{{
				Name: containerName,
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				}},
			}}
		}))
		alwaysGet(cs, backoff)

		_, err := newTestExecutor(t, cs).Execute(t.Context(), testInput())
		te := toolError(t, err)
		if te.Code != runmesh.CodeTaskOOMKilled || te.Retryable {
			t.Fatalf("got %q (retryable=%v), want a terminal %q", te.Code, te.Retryable, runmesh.CodeTaskOOMKilled)
		}
		if !strings.Contains(te.Message, "128Mi") {
			t.Errorf("message = %q, want it to name the 128Mi limit the task hit", te.Message)
		}
		if te.Exit == nil || te.Exit.Code != 137 || te.Exit.Reason != "OOMKilled" {
			t.Errorf("exit = %+v, want code 137 and reason OOMKilled", te.Exit)
		}
	})
}

func TestLogTail(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short output is kept whole, minus the trailing newline", "a\nb\n", 10, "a\nb"},
		{"a cut that lands on a line boundary keeps that line", "first line\nsecond\nthird", 12, "second\nthird"},
		{"a cut mid-line drops the partial line", "first line\nsecond\nthird", 10, "third"},
		{"a single long line keeps its end", "abcdefghij", 4, "ghij"},
		{"a whole character survives", "abcö", 2, "ö"},
		{"half a character does not", "abcö", 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := logTail(tc.in, tc.max); got != tc.want {
				t.Errorf("logTail(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
