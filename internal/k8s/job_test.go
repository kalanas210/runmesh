package k8s

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/kalanas210/runmesh/internal/tools"
)

func testInput() tools.Input {
	return tools.Input{
		JobID:          "job_06g71af4wx3vdak8enst952h34",
		StepID:         "analyze",
		Attempt:        2,
		AttemptID:      "job_06g71af4wx3vdak8enst952h34.analyze.2",
		IdempotencyKey: "b1946ac92492d2347c6235b4d2611184",
		Tool:           "csv-summary",
		Params:         json.RawMessage(`{"file":"sales.csv"}`),
		Deps:           map[string]json.RawMessage{"fetch": json.RawMessage(`{"path":"/tmp/x"}`)},
		Limits: tools.Limits{
			Timeout:     30 * time.Second,
			MaxAttempts: 3,
			CPU:         "500m",
			Memory:      "256Mi",
			Image:       "runmesh/task:dev",
		},
	}
}

func testConfig() Config {
	c := Config{
		Namespace:          "runmesh-tasks",
		TaskServiceAccount: "runmesh-task",
		TTLAfterFinished:   5 * time.Minute,
		DeadlineGrace:      time.Minute,
	}
	c.setDefaults()
	return c
}

// TestBuildJobPinsTheDivisionOfLabour. Three fields carry ADR 0003, and all
// three are the kind of thing that gets "tidied" by somebody who does not know
// why they are there — which is exactly why they are asserted rather than
// merely commented.
func TestBuildJobPinsTheDivisionOfLabour(t *testing.T) {
	t.Parallel()

	job, err := BuildJob(testConfig(), testInput(), "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0. RunMesh owns retries; if Kubernetes "+
			"retried too there would be two budgets and two answers to "+
			"'which attempt was this'", job.Spec.BackoffLimit)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never. backoffLimit only bounds "+
			"Job-level retries; a restarting container re-runs the tool inside "+
			"one attempt", got)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 300. This is the cleanup "+
			"that survives RunMesh dying", job.Spec.TTLSecondsAfterFinished)
	}
}

// TestBuildJobDeadlineIsLooserThanRunMeshs: the two deadlines must not race.
// RunMesh's own timeout fires first and produces a classified TIMED_OUT with a
// timeline entry; Kubernetes winning that race would replace it with an opaque
// DeadlineExceeded.
func TestBuildJobDeadlineIsLooserThanRunMeshs(t *testing.T) {
	t.Parallel()

	in := testInput()
	job, err := BuildJob(testConfig(), in, "owner")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("no activeDeadlineSeconds: a workload RunMesh stops watching would run for ever")
	}
	stepTimeout := int64(in.Limits.Timeout / time.Second)
	if *job.Spec.ActiveDeadlineSeconds <= stepTimeout {
		t.Fatalf("activeDeadlineSeconds = %d, must exceed the step timeout of %ds",
			*job.Spec.ActiveDeadlineSeconds, stepTimeout)
	}
}

// TestDeadlineForNeverRoundsToZero: activeDeadlineSeconds is an integer, and a
// sub-second step must not become a zero-second deadline that kills the Job
// before its container starts.
func TestDeadlineForNeverRoundsToZero(t *testing.T) {
	t.Parallel()

	for _, timeout := range []time.Duration{0, time.Nanosecond, time.Millisecond, 500 * time.Millisecond} {
		if got := deadlineFor(timeout, 0); got < 1 {
			t.Errorf("deadlineFor(%v) = %d, want at least 1", timeout, got)
		}
	}
}

func TestBuildJobCarriesRunMeshIdentity(t *testing.T) {
	t.Parallel()

	in := testInput()
	job, err := BuildJob(testConfig(), in, "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}

	// Labels are what an operator greps and what a future reaper selects on.
	if got := job.Labels[LabelJobID]; got != in.JobID {
		t.Errorf("%s = %q, want the job id %q intact", LabelJobID, got, in.JobID)
	}
	if got := job.Labels[LabelStepID]; got != in.StepID {
		t.Errorf("%s = %q, want %q", LabelStepID, got, in.StepID)
	}
	if got := job.Labels[LabelAttempt]; got != "2" {
		t.Errorf("%s = %q, want \"2\"", LabelAttempt, got)
	}
	if got := job.Labels[LabelManagedBy]; got != ManagedByValue {
		t.Errorf("%s = %q, want %q", LabelManagedBy, got, ManagedByValue)
	}
	// The name is lossy, so the annotation carries the identity in full.
	if got := job.Annotations[AnnAttemptID]; got != in.AttemptID {
		t.Errorf("%s = %q, want the untruncated attempt id %q", AnnAttemptID, got, in.AttemptID)
	}
	// The pod template must carry them too, or `kubectl logs -l` finds nothing.
	if job.Spec.Template.Labels[LabelJobID] != in.JobID {
		t.Error("the pod template does not carry the RunMesh labels")
	}
}

func TestBuildJobPassesTheContainerContract(t *testing.T) {
	t.Parallel()

	in := testInput()
	job, err := BuildJob(testConfig(), in, "owner")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		EnvTool:           in.Tool,
		EnvJobID:          in.JobID,
		EnvStepID:         in.StepID,
		EnvAttempt:        "2",
		EnvAttemptID:      in.AttemptID,
		EnvIdempotencyKey: in.IdempotencyKey,
		EnvParams:         `{"file":"sales.csv"}`,
		// Handed to the image rather than hard-coded there, so the producer
		// and the parser cannot drift apart across versions.
		EnvResultMarker: ResultMarker,
	} {
		if env[name] != want {
			t.Errorf("env %s = %q, want %q", name, env[name], want)
		}
	}
	if env[EnvDeps] == "" {
		t.Error("upstream results were not passed; a step cannot consume its dependencies")
	}
	if c.Image != in.Limits.Image {
		t.Errorf("image = %q, want the tool's own %q", c.Image, in.Limits.Image)
	}
}

// TestBuildJobDeniesTheAPIServerToTasks: the default is to mount a
// ServiceAccount token, and a compromised tool with a token is a compromised
// tool with a Kubernetes client. This costs nothing and closes a real path.
func TestBuildJobDeniesTheAPIServerToTasks(t *testing.T) {
	t.Parallel()

	job, err := BuildJob(testConfig(), testInput(), "owner")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	spec := job.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("a task pod is being given a ServiceAccount token it has no use for")
	}
}

// TestBuildJobSetsRequestsEqualToLimits: bursting makes a step that takes
// 200ms idle and 3s under load, which produces a waterfall nobody can read and
// a retry budget that depends on what else the cluster is doing.
func TestBuildJobSetsRequestsEqualToLimits(t *testing.T) {
	t.Parallel()

	job, err := BuildJob(testConfig(), testInput(), "owner")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	res := job.Spec.Template.Spec.Containers[0].Resources
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		limit, hasLimit := res.Limits[name]
		request, hasRequest := res.Requests[name]
		if !hasLimit || !hasRequest {
			t.Fatalf("%s: limit set=%v request set=%v, want both", name, hasLimit, hasRequest)
		}
		if limit.Cmp(request) != 0 {
			t.Errorf("%s: request %s != limit %s", name, request.String(), limit.String())
		}
	}
}

func TestBuildJobRejectsUnusableSpecs(t *testing.T) {
	t.Parallel()

	t.Run("no image anywhere", func(t *testing.T) {
		t.Parallel()
		in := testInput()
		in.Limits.Image = ""
		cfg := testConfig()
		cfg.Image = ""
		if _, err := BuildJob(cfg, in, "owner"); err == nil {
			t.Fatal("BuildJob accepted a container tool with no image")
		}
	})

	t.Run("the configured default image is used", func(t *testing.T) {
		t.Parallel()
		in := testInput()
		in.Limits.Image = ""
		cfg := testConfig()
		cfg.Image = "runmesh/task:fallback"
		job, err := BuildJob(cfg, in, "owner")
		if err != nil {
			t.Fatalf("BuildJob: %v", err)
		}
		if got := job.Spec.Template.Spec.Containers[0].Image; got != "runmesh/task:fallback" {
			t.Fatalf("image = %q, want the configured default", got)
		}
	})

	t.Run("a quantity Kubernetes would reject", func(t *testing.T) {
		t.Parallel()
		in := testInput()
		in.Limits.Memory = "256 gigabytes"
		if _, err := BuildJob(testConfig(), in, "owner"); err == nil {
			t.Fatal("BuildJob accepted a memory limit the API server would reject")
		}
	})
}
