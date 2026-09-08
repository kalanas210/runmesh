package policy_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kalanas210/runmesh/internal/policy"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// A permissive baseline, so each test changes exactly the one thing it is
// about. Anything a test does not mention is deliberately generous, which means
// a refusal in a test body can only have come from the rule under test.
func baseConfig() policy.Config {
	return policy.Config{
		Mode:                    tools.ModeContainer,
		DefaultCPU:              "100m",
		DefaultMemory:           "64Mi",
		DefaultEphemeralStorage: "32Mi",
		MaxCPU:                  "1",
		MaxMemory:               "512Mi",
		MaxEphemeralStorage:     "512Mi",
		MaxTimeout:              15 * time.Minute,
		MaxAttempts:             10,
		DefaultImage:            "runmesh/task:dev",
	}
}

func descriptor(name string, l tools.Limits, mode tools.ExecutionMode) tools.Descriptor {
	return tools.Descriptor{
		Name:        name,
		Version:     "1.0",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Limits:      l,
		Execution:   mode,
	}
}

func newEngine(t *testing.T, cfg policy.Config) *policy.Engine {
	t.Helper()
	e, err := policy.New(cfg)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return e
}

func refusal(t *testing.T, err error) *runmesh.ToolError {
	t.Helper()
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want *runmesh.ToolError so the engine can "+
			"classify it without guessing", err, err)
	}
	if te.Retryable {
		t.Errorf("%s is retryable; a policy refusal will give the same answer "+
			"every time, and retrying it burns the step's whole budget on a "+
			"misconfiguration", te.Code)
	}
	return te
}

// ------------------------------------------------------------------ clamping

// TestResolveClampsRatherThanRefuses. A tool asking for more CPU than the
// cluster allows is not an attack — it is a portable descriptor meeting a
// smaller cluster — so it runs smaller instead of failing.
func TestResolveClampsRatherThanRefuses(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	d := descriptor("greedy", tools.Limits{
		CPU: "8", Memory: "16Gi", EphemeralStorage: "10Gi", Image: "runmesh/task:dev",
	}, tools.ModeContainer)

	got, err := e.Resolve(d, policy.Request{})
	if err != nil {
		t.Fatalf("Resolve refused a merely-greedy tool: %v", err)
	}
	for _, tc := range []struct{ what, got, want string }{
		{"cpu", got.CPU, "1"},
		{"memory", got.Memory, "512Mi"},
		{"ephemeral storage", got.EphemeralStorage, "512Mi"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the ceiling %q", tc.what, tc.got, tc.want)
		}
	}
}

// TestResolveNeverWidens is the property the whole package exists for: the
// resolved envelope is never larger than what the operator configured, whatever
// the descriptor says. From Week 5 the descriptor's step was chosen by a
// language model.
func TestResolveNeverWidens(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	for _, want := range []tools.Limits{
		{CPU: "50m", Memory: "16Mi", EphemeralStorage: "1Mi"},
		{CPU: "1", Memory: "512Mi", EphemeralStorage: "512Mi"},
		{CPU: "64", Memory: "1Ti", EphemeralStorage: "1Ti"},
		{}, // declares nothing at all
	} {
		got, err := e.Resolve(descriptor("t", want, tools.ModeContainer), policy.Request{})
		if err != nil {
			t.Fatalf("Resolve(%+v): %v", want, err)
		}
		if cmpQuantity(t, got.CPU, "1") > 0 ||
			cmpQuantity(t, got.Memory, "512Mi") > 0 ||
			cmpQuantity(t, got.EphemeralStorage, "512Mi") > 0 {
			t.Errorf("asked for %+v and got %+v, which exceeds the ceiling", want, got)
		}
	}
}

// TestResolveDefaultsWhatATooDeclaresNothingFor. An unset CPU limit in
// Kubernetes is not a small share; it is as much as the node has.
func TestResolveDefaultsWhatATooDeclaresNothingFor(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	got, err := e.Resolve(descriptor("bare", tools.Limits{}, tools.ModeContainer), policy.Request{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.CPU != "100m" || got.Memory != "64Mi" || got.EphemeralStorage != "32Mi" {
		t.Errorf("got %+v, want the configured defaults; an unset limit means "+
			"unlimited, which is the opposite of a sandbox", got)
	}
}

// ---------------------------------------------------------------- the gates

// TestContainerToolsAreRefusedInProcess. "Never execute arbitrary code in the
// API process" is security principle 1, and this is the line that enforces it.
func TestContainerToolsAreRefusedInProcess(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Mode = tools.ModeInProcess
	e := newEngine(t, cfg)

	_, err := e.Resolve(descriptor("python_execute", tools.Limits{}, tools.ModeContainer), policy.Request{})
	if err == nil {
		t.Fatal("a container-only tool was permitted in a process that has no pod " +
			"to put it in")
	}
	if code := refusal(t, err).Code; code != runmesh.CodeToolNotSandboxed {
		t.Errorf("code = %q, want %q", code, runmesh.CodeToolNotSandboxed)
	}

	// The same tool is fine where there IS a sandbox.
	cfg.Mode = tools.ModeContainer
	if _, err := newEngine(t, cfg).Resolve(
		descriptor("python_execute", tools.Limits{}, tools.ModeContainer), policy.Request{}); err != nil {
		t.Fatalf("refused a container tool in a container deployment: %v", err)
	}
}

// TestNetworkIsDeniedUnlessSwitchedOn, and refused rather than downgraded.
// Silently running a network tool without the network produces a connection
// timeout that reads like the remote host's fault.
func TestNetworkIsDeniedUnlessSwitchedOn(t *testing.T) {
	t.Parallel()

	d := descriptor("http_request", tools.Limits{Network: true}, tools.ModeContainer)

	cfg := baseConfig()
	_, err := newEngine(t, cfg).Resolve(d, policy.Request{})
	if err == nil {
		t.Fatal("egress was granted with RUNMESH_POLICY_ALLOW_NETWORK unset")
	}
	if code := refusal(t, err).Code; code != runmesh.CodeNetworkDenied {
		t.Errorf("code = %q, want %q", code, runmesh.CodeNetworkDenied)
	}

	cfg.AllowNetwork = true
	got, err := newEngine(t, cfg).Resolve(d, policy.Request{})
	if err != nil {
		t.Fatalf("Resolve with network allowed: %v", err)
	}
	if !got.Network {
		t.Error("network was switched on and the resolved limits still deny it; " +
			"this value becomes the pod label the NetworkPolicy selects on")
	}
}

// TestNetworkIsNotGrantedToToolsThatDidNotAskForIt. Switching the feature on
// must not put every task pod on the allow side of the policy.
func TestNetworkIsNotGrantedToToolsThatDidNotAskForIt(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.AllowNetwork = true
	got, err := newEngine(t, cfg).Resolve(
		descriptor("python_execute", tools.Limits{}, tools.ModeContainer), policy.Request{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Network {
		t.Error("a tool that never asked for egress was given it because the " +
			"deployment permits egress to tools that do")
	}
}

// TestDenylistBeatsAllowlist. Deny is the incident-response lever; it has to
// win, and it has to win without an edit to the allowlist.
func TestDenylistBeatsAllowlist(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.AllowTools = []string{"echo", "python_execute"}
	cfg.DenyTools = []string{"python_execute"}
	e := newEngine(t, cfg)

	if _, err := e.Resolve(descriptor("echo", tools.Limits{}, tools.ModeInProcess),
		policy.Request{}); err != nil {
		t.Fatalf("an allowlisted tool was refused: %v", err)
	}

	_, err := e.Resolve(descriptor("python_execute", tools.Limits{}, tools.ModeContainer),
		policy.Request{})
	if code := refusal(t, err).Code; code != runmesh.CodeToolDenied {
		t.Errorf("code = %q, want %q", code, runmesh.CodeToolDenied)
	}

	// Not on the allowlist at all is the same answer, with a message that names
	// what IS permitted.
	_, err = e.Resolve(descriptor("sleep", tools.Limits{}, tools.ModeInProcess), policy.Request{})
	te := refusal(t, err)
	if te.Code != runmesh.CodeToolDenied {
		t.Errorf("code = %q, want %q", te.Code, runmesh.CodeToolDenied)
	}
	if !strings.Contains(te.Message, "echo") {
		t.Errorf("the refusal does not name the allowlist, so nobody reading it "+
			"learns what they could have asked for: %q", te.Message)
	}
}

// TestAnEmptyAllowlistMeansEverything. The alternative — empty meaning nothing
// — would make an unset variable stop the fleet.
func TestAnEmptyAllowlistMeansEverything(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	for _, name := range []string{"echo", "sleep", "anything_at_all"} {
		if _, err := e.Resolve(descriptor(name, tools.Limits{}, tools.ModeInProcess),
			policy.Request{}); err != nil {
			t.Errorf("%s was refused with no allowlist configured: %v", name, err)
		}
	}
}

// TestImagePrefixesAreEnforced.
func TestImagePrefixesAreEnforced(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.AllowImagePrefixes = []string{"runmesh/", "ghcr.io/kalanas210/"}
	e := newEngine(t, cfg)

	if _, err := e.Resolve(descriptor("ok",
		tools.Limits{Image: "runmesh/python:dev"}, tools.ModeContainer), policy.Request{}); err != nil {
		t.Fatalf("an allowed image was refused: %v", err)
	}

	_, err := e.Resolve(descriptor("bad",
		tools.Limits{Image: "docker.io/attacker/miner:latest"}, tools.ModeContainer), policy.Request{})
	if code := refusal(t, err).Code; code != runmesh.CodePolicyViolation {
		t.Errorf("code = %q, want %q", code, runmesh.CodePolicyViolation)
	}
}

// TestAContainerToolWithNoImageAnywhereIsRefused. Reaching Kubernetes with an
// empty image field produces an admission error per attempt; refusing here
// produces one message that says what is missing.
func TestAContainerToolWithNoImageAnywhereIsRefused(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.DefaultImage = ""
	_, err := newEngine(t, cfg).Resolve(
		descriptor("nameless", tools.Limits{}, tools.ModeContainer), policy.Request{})
	if code := refusal(t, err).Code; code != runmesh.CodePolicyViolation {
		t.Errorf("code = %q, want %q", code, runmesh.CodePolicyViolation)
	}
}

// TestUnparseableQuantitiesAreFatalNotClamped. Nobody can say what sandbox was
// intended by "256 MB", so guessing one is worse than refusing.
func TestUnparseableQuantitiesAreFatalNotClamped(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	_, err := e.Resolve(descriptor("typo",
		tools.Limits{Memory: "256 MB"}, tools.ModeContainer), policy.Request{})
	if code := refusal(t, err).Code; code != runmesh.CodePolicyViolation {
		t.Errorf("code = %q, want %q", code, runmesh.CodePolicyViolation)
	}
}

// --------------------------------------------------------- the per-step half

// TestResolveClampsTheRequestToo. The plan's own limits are validated against
// separate configuration, and only one of the two is a security control.
func TestResolveClampsTheRequestToo(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.MaxTimeout = time.Minute
	cfg.MaxAttempts = 3
	e := newEngine(t, cfg)

	got, err := e.Resolve(descriptor("t", tools.Limits{}, tools.ModeContainer), policy.Request{
		Timeout:        time.Hour,
		MaxAttempts:    99,
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Timeout != time.Minute {
		t.Errorf("timeout = %s, want the ceiling 1m", got.Timeout)
	}
	if got.MaxAttempts != 3 {
		t.Errorf("max attempts = %d, want the ceiling 3", got.MaxAttempts)
	}
}

// TestTheToolsOwnOutputCapWins when it is smaller than the runtime's. A tool
// that says it returns at most a kilobyte should not be allowed a megabyte.
func TestTheToolsOwnOutputCapWins(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	got, err := e.Resolve(
		descriptor("small", tools.Limits{MaxOutputBytes: 1 << 10}, tools.ModeContainer),
		policy.Request{MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.MaxOutputBytes != 1<<10 {
		t.Errorf("max output = %d, want the smaller of the two (1024)", got.MaxOutputBytes)
	}
}

// ------------------------------------------------------------- construction

// TestNewRefusesAContradictoryPolicy. A default above its own ceiling silently
// clamps every tool that declares nothing, which is a fact you would otherwise
// discover from a resource graph.
func TestNewRefusesAContradictoryPolicy(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*policy.Config){
		"default above ceiling": func(c *policy.Config) { c.DefaultMemory = "1Gi"; c.MaxMemory = "512Mi" },
		"unparseable ceiling":   func(c *policy.Config) { c.MaxCPU = "half a core" },
		"unparseable default":   func(c *policy.Config) { c.DefaultCPU = "lots" },
		"negative attempts":     func(c *policy.Config) { c.MaxAttempts = -1 },
	} {
		cfg := baseConfig()
		mutate(&cfg)
		if _, err := policy.New(cfg); err == nil {
			t.Errorf("%s: policy.New accepted it; a policy nobody can reason "+
				"about should fail the boot, not every step", name)
		}
	}
}

// ---------------------------------------------------------------- catalogue

// TestEffectivePublishesTheGrantNotTheRequest. Publishing what a tool asked for
// tells a planner it has 8 CPUs on a cluster that will give it 250m, and every
// plan built on that number is wrong the same way.
func TestEffectivePublishesTheGrantNotTheRequest(t *testing.T) {
	t.Parallel()

	e := newEngine(t, baseConfig())
	got := e.Effective(descriptor("greedy",
		tools.Limits{CPU: "8", Memory: "16Gi", Image: "runmesh/task:dev"}, tools.ModeContainer))

	if got.Limits.CPU != "1" || got.Limits.Memory != "512Mi" {
		t.Errorf("published limits %+v, want the clamped ones", got.Limits)
	}
	if got.Denied {
		t.Error("a tool that merely asked for too much was marked denied")
	}
	if got.Execution != tools.ModeContainer {
		t.Errorf("execution = %q, want the deployment's mode", got.Execution)
	}
}

// TestEffectiveListsDeniedToolsWithAReason. Omitting them would make
// "unknown_tool" the only evidence that a tool exists but is switched off.
func TestEffectiveListsDeniedToolsWithAReason(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.DenyTools = []string{"python_execute"}
	got := newEngine(t, cfg).Effective(
		descriptor("python_execute", tools.Limits{}, tools.ModeContainer))

	if !got.Denied {
		t.Fatal("a denied tool is published as available")
	}
	if got.DeniedReason == "" {
		t.Error("no reason given, so a caller learns nothing it can act on")
	}
	if got.Name != "python_execute" {
		t.Errorf("name = %q, want the tool to still be identifiable", got.Name)
	}
}

// ------------------------------------------------------------------ sandbox

// TestSandboxSeparatesUnknownFromDenied. "I never installed that tool" and "I
// switched that tool off" are different problems with different fixes.
func TestSandboxSeparatesUnknownFromDenied(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Mode = tools.ModeInProcess
	cfg.DenyTools = []string{"sleep"}
	box := policy.NewSandbox(
		tools.Registry{"echo": tools.Echo{}, "sleep": tools.Sleep{}},
		newEngine(t, cfg))

	if err := box.Allows("echo"); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if code := refusal(t, box.Allows("sleep")).Code; code != runmesh.CodeToolDenied {
		t.Errorf("a denied tool reports %q, want %q", code, runmesh.CodeToolDenied)
	}
	if code := refusal(t, box.Allows("nonexistent")).Code; code != runmesh.CodeToolUnknown {
		t.Errorf("an unregistered tool reports %q, want %q", code, runmesh.CodeToolUnknown)
	}
	if _, err := box.Limits("nonexistent", policy.Request{}); err == nil {
		t.Error("Limits resolved an envelope for a tool that does not exist")
	}
}

// TestSandboxDescriptorsCoverTheWholeRegistry, denied ones included.
func TestSandboxDescriptorsCoverTheWholeRegistry(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Mode = tools.ModeInProcess
	cfg.DenyTools = []string{"sleep"}
	box := policy.NewSandbox(
		tools.Registry{"echo": tools.Echo{}, "sleep": tools.Sleep{}},
		newEngine(t, cfg))

	got := box.Descriptors()
	if len(got) != 2 {
		t.Fatalf("descriptors = %d, want 2 (a denied tool is still listed)", len(got))
	}
	for _, d := range got {
		if d.Name == "sleep" && !d.Denied {
			t.Error("the denied tool is listed as available")
		}
		if d.Name == "echo" && d.Denied {
			t.Error("a permitted tool is listed as denied")
		}
	}
}

// TestPassthroughGrantsExactlyWhatWasAsked. It is what a unit test gets when it
// builds an engine with no policy, and it must not quietly invent limits.
func TestPassthroughGrantsExactlyWhatWasAsked(t *testing.T) {
	t.Parallel()

	got, err := policy.Passthrough{}.Limits("anything", policy.Request{
		Timeout: time.Second, MaxAttempts: 2, MaxOutputBytes: 99,
	})
	if err != nil {
		t.Fatalf("Passthrough: %v", err)
	}
	if got.Timeout != time.Second || got.MaxAttempts != 2 || got.MaxOutputBytes != 99 {
		t.Errorf("got %+v, want the request unchanged", got)
	}
	if got.CPU != "" || got.Memory != "" || got.Image != "" || got.Network {
		t.Errorf("got %+v, want no sandbox at all: inventing one here would make "+
			"a test pass against limits production never applies", got)
	}
}

// cmpQuantity compares two Kubernetes quantity strings, via the resolved
// values' own ordering rather than string equality — "1" and "1000m" are the
// same CPU limit.
func cmpQuantity(t *testing.T, got, want string) int {
	t.Helper()
	if got == "" {
		return -1
	}
	g := mustParse(t, got)
	w := mustParse(t, want)
	return g.Cmp(w)
}

func mustParse(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("ParseQuantity(%q): %v", s, err)
	}
	return q
}
