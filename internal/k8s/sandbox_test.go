package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kalanas210/runmesh/internal/tools"
)

// TestBuildJobHardensThePod asserts the Week-4 security context field by field.
//
// Every one of these is a line somebody can delete without breaking a single
// other test, and the effect of deleting it is invisible until it matters. That
// is the definition of something worth pinning.
func TestBuildJobHardensThePod(t *testing.T) {
	t.Parallel()

	job, err := BuildJob(testConfig(), testInput(), "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]

	if pod.SecurityContext == nil {
		t.Fatal("the pod has no securityContext at all")
	}
	if c.SecurityContext == nil {
		t.Fatal("the container has no securityContext at all")
	}

	// Checked at BOTH levels on purpose: a container security context replaces
	// the pod's rather than merging into it, so a container that inherits by
	// omission is one added field away from inheriting nothing.
	for _, tc := range []struct {
		what string
		got  *bool
		want bool
	}{
		{"pod runAsNonRoot", pod.SecurityContext.RunAsNonRoot, true},
		{"container runAsNonRoot", c.SecurityContext.RunAsNonRoot, true},
		{"container allowPrivilegeEscalation", c.SecurityContext.AllowPrivilegeEscalation, false},
		{"container readOnlyRootFilesystem", c.SecurityContext.ReadOnlyRootFilesystem, true},
		{"container privileged", c.SecurityContext.Privileged, false},
		{"automountServiceAccountToken", pod.AutomountServiceAccountToken, false},
		{"enableServiceLinks", pod.EnableServiceLinks, false},
	} {
		switch {
		case tc.got == nil:
			t.Errorf("%s is unset; unset is not the same as %v and Kubernetes "+
				"defaults it the other way", tc.what, tc.want)
		case *tc.got != tc.want:
			t.Errorf("%s = %v, want %v", tc.what, *tc.got, tc.want)
		}
	}

	if c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]. Enumerating the dangerous "+
			"capabilities produces a list that ages; dropping everything does not",
			c.SecurityContext.Capabilities)
	}

	for _, sc := range []*corev1.SeccompProfile{
		pod.SecurityContext.SeccompProfile, c.SecurityContext.SeccompProfile,
	} {
		if sc == nil || sc.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("seccompProfile = %v, want RuntimeDefault. Omitting it means "+
				"Unconfined, which is the default worth opting out of", sc)
		}
	}

	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Errorf("runAsUser = %v, want a non-zero uid: runAsNonRoot with uid 0 "+
			"is refused at admission", pod.SecurityContext.RunAsUser)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("imagePullPolicy = %q, want IfNotPresent so a kind-loaded image "+
			"is not re-pulled from a registry that does not have it", c.ImagePullPolicy)
	}
}

// TestBuildJobGivesExactlyOneWritablePath. A read-only root filesystem with no
// scratch is a Python that cannot import a module that touches a temporary
// file, and the error it produces reads like a bug in the tool.
func TestBuildJobGivesExactlyOneWritablePath(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Limits.EphemeralStorage = "32Mi"
	job, err := BuildJob(testConfig(), in, "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	pod := job.Spec.Template.Spec

	if len(pod.Volumes) != 1 {
		t.Fatalf("volumes = %d, want exactly 1 (the scratch emptyDir)", len(pod.Volumes))
	}
	vol := pod.Volumes[0]
	if vol.EmptyDir == nil {
		t.Fatalf("the scratch volume is %+v, want an emptyDir that dies with the pod", vol.VolumeSource)
	}
	want := resource.MustParse("32Mi")
	if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.Cmp(want) != 0 {
		t.Errorf("emptyDir sizeLimit = %v, want 32Mi. An unbounded emptyDir is the "+
			"one door a task has onto the node's disk", vol.EmptyDir.SizeLimit)
	}

	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != scratchPath || mounts[0].Name != vol.Name {
		t.Fatalf("volumeMounts = %+v, want exactly %s from %q", mounts, scratchPath, vol.Name)
	}

	// The environment has to agree with the mount, or a runtime writes to a
	// path that is not there.
	env := map[string]string{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for _, key := range []string{"TMPDIR", "HOME"} {
		if env[key] != scratchPath {
			t.Errorf("%s = %q, want %q: the root filesystem is read-only and this "+
				"is the only writable mount", key, env[key], scratchPath)
		}
	}
}

// TestBuildJobLabelsNetworkAccessFromTheRESOLVEDLimits. The label is what the
// NetworkPolicy selects on, so it must follow the limits the policy engine
// produced — never a descriptor's request, which policy may have refused.
func TestBuildJobLabelsNetworkAccessFromTheRESOLVEDLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		network bool
		want    string
	}{
		{"denied by default", false, NetworkDeny},
		{"granted by policy", true, NetworkAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := testInput()
			in.Limits.Network = tc.network
			job, err := BuildJob(testConfig(), in, "runmesh-abc-1")
			if err != nil {
				t.Fatalf("BuildJob: %v", err)
			}
			for where, labels := range map[string]map[string]string{
				"job":          job.Labels,
				"pod template": job.Spec.Template.Labels,
			} {
				if got := labels[LabelNetwork]; got != tc.want {
					t.Errorf("%s label %s = %q, want %q", where, LabelNetwork, got, tc.want)
				}
			}
		})
	}
}

// TestBuildJobAlwaysLabelsNetworkAccess. A pod carrying no runmesh.io/network
// label matches neither policy, and in Kubernetes "matched by no policy" means
// unrestricted — so an unlabelled pod is the failure this guards.
func TestBuildJobAlwaysLabelsNetworkAccess(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Limits = tools.Limits{Image: "runmesh/task:dev"} // nothing declared at all
	job, err := BuildJob(testConfig(), in, "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if got, ok := job.Spec.Template.Labels[LabelNetwork]; !ok || got != NetworkDeny {
		t.Fatalf("network label = %q (present=%v), want %q. A pod with no label "+
			"matches no policy, and no policy means unrestricted",
			got, ok, NetworkDeny)
	}
}

// TestBuildJobLimitsEphemeralStorage. The dimension that takes a node down
// rather than a pod: container logs count towards it, so a task looping on
// print() fills the node's disk unless this is set.
func TestBuildJobLimitsEphemeralStorage(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Limits.EphemeralStorage = "64Mi"
	job, err := BuildJob(testConfig(), in, "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	res := job.Spec.Template.Spec.Containers[0].Resources
	for _, list := range []struct {
		what string
		l    corev1.ResourceList
	}{{"limits", res.Limits}, {"requests", res.Requests}} {
		q, ok := list.l[corev1.ResourceEphemeralStorage]
		if !ok {
			t.Errorf("%s has no ephemeral-storage entry", list.what)
			continue
		}
		if q.String() != "64Mi" {
			t.Errorf("%s ephemeral-storage = %s, want 64Mi", list.what, q.String())
		}
	}
}

// TestTerminationGraceIsShortAndNeverNegative.
func TestTerminationGraceIsShortAndNeverNegative(t *testing.T) {
	t.Parallel()

	job, err := BuildJob(testConfig(), testInput(), "runmesh-abc-1")
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	g := job.Spec.Template.Spec.TerminationGracePeriodSeconds
	if g == nil || *g < 0 {
		t.Fatalf("terminationGracePeriodSeconds = %v, want a non-negative value", g)
	}
	if *g > 30 {
		t.Errorf("terminationGracePeriodSeconds = %d: a task pod is deleted exactly "+
			"when RunMesh has decided the step should stop, so a long grace is a "+
			"node held by work nobody wants the answer to", *g)
	}
}

// ------------------------------------------------------- the manifests agree

// TestNetworkPolicySelectorsMatchTheJobLabels reads the shipped manifest and
// checks it against the constants this package labels pods with.
//
// This is the test that earns its place. The Go code and the YAML are edited by
// different people at different times with no compiler between them, and a
// selector that no longer matches produces a task pod that matches NO policy —
// which Kubernetes treats as unrestricted. The symptom is a sandbox that
// silently stops being one, with every manifest still listed and every pod
// still running.
func TestNetworkPolicySelectorsMatchTheJobLabels(t *testing.T) {
	t.Parallel()

	policies := loadNetworkPolicies(t)
	if len(policies) == 0 {
		t.Fatal("no NetworkPolicy objects found in deploy/kubernetes")
	}

	var defaultDeny *networkingv1.NetworkPolicy
	allowSelected := 0

	for i := range policies {
		p := &policies[i]
		if p.Namespace != "runmesh-tasks" {
			t.Errorf("%s is in namespace %q; the sandbox policies belong in "+
				"runmesh-tasks, and a policy in the wrong namespace applies to nothing",
				p.Name, p.Namespace)
		}
		switch {
		case len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.PodSelector.MatchExpressions) == 0:
			defaultDeny = p
		case p.Spec.PodSelector.MatchLabels[LabelNetwork] == NetworkAllow:
			allowSelected++
		default:
			t.Errorf("%s selects %v, which matches no label BuildJob sets. "+
				"The sandbox label is %s=%s|%s", p.Name,
				p.Spec.PodSelector.MatchLabels, LabelNetwork, NetworkDeny, NetworkAllow)
		}
	}

	if defaultDeny == nil {
		t.Fatal("no default-deny policy (podSelector: {}). Without one, a pod " +
			"matched by no other policy is unrestricted")
	}
	if len(defaultDeny.Spec.Ingress) != 0 || len(defaultDeny.Spec.Egress) != 0 {
		t.Errorf("%s has rules; a default-deny policy is the ABSENCE of rules, "+
			"not an empty allowance", defaultDeny.Name)
	}
	if !hasPolicyType(defaultDeny, networkingv1.PolicyTypeIngress) ||
		!hasPolicyType(defaultDeny, networkingv1.PolicyTypeEgress) {
		t.Errorf("%s must list both Ingress and Egress in policyTypes; a type "+
			"that is not listed is not denied", defaultDeny.Name)
	}
	if allowSelected == 0 {
		t.Errorf("nothing selects %s=%s, so a tool granted the network by policy "+
			"would still be denied by the CNI", LabelNetwork, NetworkAllow)
	}
}

// TestNetworkPolicyExcludesTheMetadataService is the one assertion in this file
// about a specific address.
//
// 169.254.169.254 is the cloud instance metadata service. On EC2, GCE and Azure
// it hands the node's IAM credentials to anything that asks, over plain HTTP,
// with no authentication. An egress rule of 0.0.0.0/0 with no exceptions passes
// every other check here and is a direct path from "a language model chose to
// fetch a URL" to the cluster's cloud credentials.
func TestNetworkPolicyExcludesTheMetadataService(t *testing.T) {
	t.Parallel()

	const (
		metadata = "169.254."
		rfc1918a = "10.0.0.0/8"
	)
	found := false

	for _, p := range loadNetworkPolicies(t) {
		for _, rule := range p.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock == nil || peer.IPBlock.CIDR != "0.0.0.0/0" {
					continue
				}
				found = true
				excepted := strings.Join(peer.IPBlock.Except, " ")
				if !strings.Contains(excepted, metadata) {
					t.Errorf("%s permits egress to 0.0.0.0/0 without excluding the "+
						"link-local range: the instance metadata service at "+
						"169.254.169.254 hands out the node's cloud credentials to "+
						"anything that asks. except = %v", p.Name, peer.IPBlock.Except)
				}
				if !strings.Contains(excepted, rfc1918a) {
					t.Errorf("%s permits egress to 0.0.0.0/0 without excluding %s: "+
						"the cluster's own pods, services and nodes live there, and so "+
						"does everything else on its network. except = %v",
						p.Name, rfc1918a, peer.IPBlock.Except)
				}
			}
		}
	}
	if !found {
		t.Skip("no policy grants egress to 0.0.0.0/0; nothing to check")
	}
}

// TestTaskPodsAreNeverReachable. A task is a batch workload that dials out and
// exits; nothing in the cluster should be able to reach into one, and a policy
// that permits ingress to a sandbox has no reason to exist.
func TestTaskPodsAreNeverReachable(t *testing.T) {
	t.Parallel()

	for _, p := range loadNetworkPolicies(t) {
		if len(p.Spec.Ingress) > 0 {
			t.Errorf("%s permits ingress to task pods. Nothing needs to reach a "+
				"task: results leave through the container's stdout, read by "+
				"RunMesh over the Kubernetes API", p.Name)
		}
	}
}

// loadNetworkPolicies decodes every NetworkPolicy in deploy/kubernetes.
//
// It reads the SHIPPED manifests rather than a fixture, which is the entire
// point: a copy of the policy inside the test package would agree with the code
// for ever while the file that actually gets applied drifted away.
func loadNetworkPolicies(t *testing.T) []networkingv1.NetworkPolicy {
	t.Helper()

	dir := filepath.Join("..", "..", "deploy", "kubernetes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	var out []networkingv1.NetworkPolicy
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("opening %s: %v", e.Name(), err)
		}
		// A multi-document stream, decoded one document at a time. The
		// decoder converts YAML to JSON and then uses encoding/json, so the
		// struct tags on the k8s.io/api types are what parse it — no new
		// dependency, and no hand-rolled YAML.
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for {
			var doc struct {
				Kind string `json:"kind"`
			}
			var raw networkingv1.NetworkPolicy
			if err := dec.Decode(&raw); err != nil {
				break
			}
			doc.Kind = raw.Kind
			if doc.Kind == "NetworkPolicy" {
				out = append(out, raw)
			}
		}
		_ = f.Close()
	}
	return out
}

func hasPolicyType(p *networkingv1.NetworkPolicy, want networkingv1.PolicyType) bool {
	for _, t := range p.Spec.PolicyTypes {
		if t == want {
			return true
		}
	}
	return false
}
