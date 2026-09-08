package k8s

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// dns1123Label is the alphabet Kubernetes actually enforces for a name that
// has to become part of a pod's name. Asserting against the real pattern
// rather than "looks fine" is the point: every failure this catches is one the
// API server would otherwise catch at 3am in someone else's namespace.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestJobNameIsAlwaysAValidLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		attempt string
	}{
		{"a real attempt id", "job_06g71af4wx3vdak8enst952h34.fetch.1"},
		{"underscores and dots", "job_a.b_c.d.99"},
		{"upper case", "JOB_ABC.STEP.1"},
		{"leading and trailing junk", "___...abc...___"},
		{"unicode", "job_café.étape.1"},
		{"a very long id", "job_" + strings.Repeat("x", 300) + ".step.1"},
		{"nothing legal at all", "___...___"},
		{"empty", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := JobName(tc.attempt)

			if !dns1123Label.MatchString(got) {
				t.Fatalf("JobName(%q) = %q, which is not a DNS-1123 label", tc.attempt, got)
			}
			// 63 is the pod-name ceiling; Kubernetes appends "-xxxxx" to name
			// the Job's pod, and a Job whose pod cannot be named never runs.
			if len(got) > maxNameLen {
				t.Fatalf("JobName(%q) is %d chars, over the %d budget that leaves "+
					"room for the pod suffix", tc.attempt, len(got), maxNameLen)
			}
		})
	}
}

// TestJobNameIsDeterministic is the property the adopt-on-AlreadyExists path
// depends on. Creating a Job is not idempotent by itself: if the API server
// accepts the create and the response is lost, the retry has to recognise its
// own work rather than start a second workload for one attempt.
func TestJobNameIsDeterministic(t *testing.T) {
	t.Parallel()

	const id = "job_06g71af4wx3vdak8enst952h34.analyze.2"
	first := JobName(id)
	for range 100 {
		if got := JobName(id); got != first {
			t.Fatalf("JobName is not deterministic: %q then %q", first, got)
		}
	}
}

// TestJobNameSeparatesAttempts: a retry is a NEW execution and must get its own
// workload. Safety ACROSS attempts is IdempotencyKey's job, not the name's.
func TestJobNameSeparatesAttempts(t *testing.T) {
	t.Parallel()

	base := "job_06g71af4wx3vdak8enst952h34.fetch."
	seen := map[string]int{}
	for attempt := 1; attempt <= 10; attempt++ {
		seen[JobName(base+itoa(attempt))]++
	}
	if len(seen) != 10 {
		t.Fatalf("10 attempts produced %d distinct names", len(seen))
	}
}

// TestJobNameSurvivesTruncation is why there is a hash suffix at all. Two long
// attempt ids that share a prefix truncate to the same readable body; without
// the hash of the UNtruncated value they would collide, and two unrelated
// steps would fight over one Kubernetes Job.
func TestJobNameSurvivesTruncation(t *testing.T) {
	t.Parallel()

	prefix := "job_" + strings.Repeat("a", 80)
	first := JobName(prefix + ".step-one.1")
	second := JobName(prefix + ".step-two.1")

	if first == second {
		t.Fatalf("two attempts sharing a long prefix collided on %q", first)
	}
	if !strings.HasPrefix(first, namePrefix) {
		t.Errorf("name %q lost its %q prefix", first, namePrefix)
	}
}

// TestLabelValueKeepsRunMeshIdentityQueryable: the object NAME is lossy, so the
// labels are what an operator greps. A job id must survive them intact, which
// is why label values use the laxer alphabet rather than the name's.
func TestLabelValueKeepsRunMeshIdentityQueryable(t *testing.T) {
	t.Parallel()

	id := runmesh.NewID("job_", epochForTest())
	if got := labelValue(id); got != id {
		t.Fatalf("labelValue(%q) = %q; a job id must pass through unchanged so "+
			"`kubectl get jobs -l runmesh.io/job-id=%s` works", id, got, id)
	}

	// And it still has to be a legal label value in the ugly cases.
	for _, in := range []string{"a/b", "café", strings.Repeat("z", 100), "--x--"} {
		got := labelValue(in)
		if len(got) > 63 {
			t.Errorf("labelValue(%q) is %d chars, over the 63 limit", in, len(got))
		}
		if got != "" && !regexp.MustCompile(`^[a-zA-Z0-9]([-a-zA-Z0-9_.]*[a-zA-Z0-9])?$`).MatchString(got) {
			t.Errorf("labelValue(%q) = %q, which Kubernetes would reject", in, got)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
