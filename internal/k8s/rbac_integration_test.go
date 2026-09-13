package k8s_test

import (
	"os"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/k8s"
)

// integrationClient connects to the cluster RUNMESH_TEST_KUBECONTEXT names, and
// skips the test when it names none. Every integration test in this package
// starts here.
func integrationClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	kubecontext := os.Getenv("RUNMESH_TEST_KUBECONTEXT")
	if kubecontext == "" {
		t.Skip("set RUNMESH_TEST_KUBECONTEXT (e.g. kind-runmesh) to run the Kubernetes integration tests")
	}
	client, err := k8s.NewClient(os.Getenv("RUNMESH_TEST_KUBECONFIG"), kubecontext)
	if err != nil {
		t.Fatalf("connecting to %s: %v", kubecontext, err)
	}
	return client
}

// TestIntegrationTheRoleRefusesWhatItShould holds deploy/kubernetes/10-rbac.yaml
// to what its comments promise. The test of an RBAC policy is what it refuses,
// and a fake clientset has no authorizer to ask, so this asks the real one: one
// SubjectAccessReview per question, for RunMesh's own ServiceAccount and for the
// one task pods run as.
//
// The allowed rows are the executor's whole working set. If one of them fails,
// the manifests are probably not applied:
//
//	kubectl apply -f deploy/kubernetes/00-namespaces.yaml -f deploy/kubernetes/10-rbac.yaml
func TestIntegrationTheRoleRefusesWhatItShould(t *testing.T) {
	client := integrationClient(t)

	ctx, cancel := clock.System().WithTimeout(t.Context(), time.Minute)
	defer cancel()

	const (
		runmeshSA = "system:serviceaccount:runmesh:runmesh"
		taskSA    = "system:serviceaccount:runmesh-tasks:runmesh-task"
		tasks     = "runmesh-tasks"
	)
	checks := []struct {
		name  string
		user  string
		attrs authorizationv1.ResourceAttributes
		want  bool
	}{
		// What the executor needs.
		{"runmesh creates Jobs for tasks", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "create", Group: "batch", Resource: "jobs"}, true},
		{"runmesh deletes the Jobs it made", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "delete", Group: "batch", Resource: "jobs"}, true},
		{"runmesh finds a Job's pod", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "list", Resource: "pods"}, true},
		{"runmesh reads a task's result from its log", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "get", Resource: "pods", Subresource: "log"}, true},

		// What it must not have.
		{"runmesh cannot create Jobs in its own namespace", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: "runmesh", Verb: "create", Group: "batch", Resource: "jobs"}, false},
		{"runmesh cannot bulk-delete Jobs", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "deletecollection", Group: "batch", Resource: "jobs"}, false},
		{"runmesh cannot create bare pods", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "create", Resource: "pods"}, false},
		{"runmesh cannot exec into a running task", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "create", Resource: "pods", Subresource: "exec"}, false},
		{"runmesh cannot read Secrets", runmeshSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "get", Resource: "secrets"}, false},
		{"runmesh cannot touch nodes", runmeshSA,
			authorizationv1.ResourceAttributes{Verb: "list", Resource: "nodes"}, false},

		// The identity untrusted workloads run as holds nothing at all.
		{"a task pod cannot list pods", taskSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "list", Resource: "pods"}, false},
		{"a task pod cannot create Jobs", taskSA,
			authorizationv1.ResourceAttributes{Namespace: tasks, Verb: "create", Group: "batch", Resource: "jobs"}, false},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			// A ServiceAccount's requests also carry these groups, and a grant to
			// any of them would count, so the review has to name them too.
			namespace := strings.Split(c.user, ":")[2]
			review := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:               c.user,
					Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
					ResourceAttributes: &c.attrs,
				},
			}
			got, err := client.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("SubjectAccessReview: %v", err)
			}
			if got.Status.Allowed != c.want {
				t.Errorf("allowed = %v, want %v (reason: %q)", got.Status.Allowed, c.want, got.Status.Reason)
			}
		})
	}
}
