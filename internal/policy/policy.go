// Package policy is the execution policy engine: the one place that decides
// what sandbox a step actually gets.
//
// The rule it exists to enforce is a single sentence from the project plan —
// "the orchestrator should never allow a task to override security policy
// arbitrarily" — and it matters more here than in an ordinary runtime, because
// from Week 5 the thing proposing the step is a language model. A descriptor's
// limits are a REQUEST. The operator's configuration is the GRANT. Resolve is
// where the two meet, and it always resolves towards the operator:
//
//	effective = min(tool asks for, operator ceiling), defaulted, never widened
//
// It is a pure function of its inputs. No clock, no client, no environment, no
// IO — so every rule below is a table test rather than something you learn from
// a cluster at three in the morning.
//
// The same Engine is consulted in three places, which is deliberate:
//
//   - at SUBMIT, so a plan naming a denied tool is a 400 with a reason;
//   - at DISPATCH, so a policy that changed after submission still binds the
//     attempt that is about to run;
//   - at GET /api/v1/tools, so the catalogue — and the Week-5 planner reading
//     it — is told the envelope that will actually apply, not the one the tool
//     wished for.
package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Config is the operator's half of the contract. Every field is loaded from
// the environment by internal/config; nothing here reads one itself.
type Config struct {
	// Mode is where this deployment runs tools. A tool whose descriptor says
	// container is refused outright when this is in_process — see Resolve.
	Mode tools.ExecutionMode

	// Defaults apply when a tool declares nothing. A tool that names no CPU
	// must not get an unbounded one: an unset limit in Kubernetes is not "a
	// small share", it is "as much as the node has".
	DefaultCPU              string
	DefaultMemory           string
	DefaultEphemeralStorage string

	// Ceilings clamp what a tool may ask for. Empty means no ceiling, which is
	// a deliberate, documented choice for a single-tenant development cluster
	// and a bad one anywhere else; config.Validate warns rather than forbids.
	MaxCPU              string
	MaxMemory           string
	MaxEphemeralStorage string

	MaxTimeout  time.Duration
	MaxAttempts int

	// AllowNetwork is the master switch for egress. False means no tool gets
	// the network, whatever its descriptor says — so turning the feature off is
	// one variable rather than an audit of every descriptor.
	AllowNetwork bool

	// AllowTools, if non-empty, is an exact allowlist: only these names may
	// run, whatever the registry contains. DenyTools always wins over it.
	//
	// Both exist because they answer different questions. An allowlist is the
	// posture ("this deployment runs exactly these three tools"); a denylist is
	// the incident response ("turn that one off, now, without redeploying a
	// registry").
	AllowTools []string
	DenyTools  []string

	// AllowImagePrefixes, if non-empty, constrains which images a tool may
	// name. Descriptors are compiled in today, so this guards a smaller hole
	// than it looks — but the descriptor list is exactly the kind of thing that
	// grows a configuration file, and the check costs one string compare.
	AllowImagePrefixes []string

	// DefaultImage backs a container tool that names none.
	DefaultImage string
}

// Engine answers questions about the policy. It is immutable after New and
// therefore safe to share across every goroutine in the process without a lock.
type Engine struct {
	cfg Config

	// Parsed once at boot: a quantity that will be compared on every dispatch
	// should not be re-parsed on every dispatch, and a ceiling that does not
	// parse should fail the boot rather than every step.
	maxCPU       *resource.Quantity
	maxMemory    *resource.Quantity
	maxEphemeral *resource.Quantity

	allow map[string]bool
	deny  map[string]bool
}

// New validates the configuration and precomputes what Resolve needs.
//
// Every error here is a boot failure. A policy that cannot be parsed is a
// policy nobody can reason about, and starting anyway means running with an
// envelope that is neither what was configured nor what was intended.
func New(cfg Config) (*Engine, error) {
	if cfg.Mode == "" {
		cfg.Mode = tools.ModeInProcess
	}
	e := &Engine{cfg: cfg, allow: set(cfg.AllowTools), deny: set(cfg.DenyTools)}

	var errs []string
	parse := func(name, raw string) *resource.Quantity {
		if raw == "" {
			return nil
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %q is not a Kubernetes quantity (e.g. 500m, 256Mi)", name, raw))
			return nil
		}
		return &q
	}
	e.maxCPU = parse("max cpu", cfg.MaxCPU)
	e.maxMemory = parse("max memory", cfg.MaxMemory)
	e.maxEphemeral = parse("max ephemeral storage", cfg.MaxEphemeralStorage)

	// The defaults are parsed and then discarded: they are only ever used as
	// strings, but a default that does not parse is a boot failure for exactly
	// the same reason a ceiling is — every step would fail at build time with
	// an error naming a value the operator has to go and find.
	for name, raw := range map[string]string{
		"default cpu":               cfg.DefaultCPU,
		"default memory":            cfg.DefaultMemory,
		"default ephemeral storage": cfg.DefaultEphemeralStorage,
	} {
		parse(name, raw)
	}

	// A default above its own ceiling means every tool that declares nothing is
	// clamped down silently. That is a configuration mistake worth refusing,
	// not a fact to discover from a resource graph.
	for _, c := range []struct {
		name    string
		def     string
		ceiling *resource.Quantity
	}{
		{"cpu", cfg.DefaultCPU, e.maxCPU},
		{"memory", cfg.DefaultMemory, e.maxMemory},
		{"ephemeral storage", cfg.DefaultEphemeralStorage, e.maxEphemeral},
	} {
		if c.def == "" || c.ceiling == nil {
			continue
		}
		if q, err := resource.ParseQuantity(c.def); err == nil && q.Cmp(*c.ceiling) > 0 {
			errs = append(errs, fmt.Sprintf("default %s (%s) exceeds the ceiling (%s)",
				c.name, c.def, c.ceiling.String()))
		}
	}

	if cfg.MaxTimeout < 0 {
		errs = append(errs, "max timeout must not be negative")
	}
	if cfg.MaxAttempts < 0 {
		errs = append(errs, "max attempts must not be negative")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("policy: %s", strings.Join(errs, "; "))
	}
	return e, nil
}

// Request is the per-attempt part: the values the plan asked for, already
// clamped by plan validation. Resolve clamps them again, because plan limits
// and policy limits are configured separately and only one of them is a
// security control.
type Request struct {
	Timeout        time.Duration
	MaxAttempts    int
	MaxOutputBytes int
}

// Resolve produces the sandbox one attempt actually gets, or a classified,
// TERMINAL error explaining the refusal.
//
// Terminal is the only defensible classification for every branch here. A tool
// that is denied stays denied; a container tool in an in-process deployment
// stays wrong; a quantity that does not parse will not parse next time. Marking
// any of them retryable would burn a step's whole budget on a misconfiguration
// and bury the one message that says what to fix.
func (e *Engine) Resolve(d tools.Descriptor, req Request) (tools.Limits, error) {
	name := d.Name

	if e.deny[name] {
		return tools.Limits{}, runmesh.Fatal(runmesh.CodeToolDenied,
			"tool %q is denied by execution policy", name)
	}
	if len(e.allow) > 0 && !e.allow[name] {
		return tools.Limits{}, runmesh.Fatal(runmesh.CodeToolDenied,
			"tool %q is not on the execution policy's allowlist (%s)",
			name, strings.Join(sorted(e.allow), ", "))
	}

	// The line that keeps arbitrary code out of the API process. python_execute
	// declares ModeContainer; a deployment with RUNMESH_EXECUTOR=local has no
	// pod to put it in, and the honest answer is to refuse rather than to run
	// it somewhere weaker than advertised.
	if d.Execution == tools.ModeContainer && e.cfg.Mode != tools.ModeContainer {
		return tools.Limits{}, runmesh.Fatal(runmesh.CodeToolNotSandboxed,
			"tool %q requires container execution; this deployment runs tools in-process "+
				"(set RUNMESH_EXECUTOR=kubernetes)", name)
	}

	want := d.Limits
	out := tools.Limits{
		Timeout:        req.Timeout,
		MaxAttempts:    req.MaxAttempts,
		MaxOutputBytes: req.MaxOutputBytes,
		Network:        want.Network,
	}

	if want.Network && !e.cfg.AllowNetwork {
		// Downgrading silently to "no network" was the alternative, and it is
		// worse: the tool would run, fail to connect, and report a timeout that
		// looks like the remote host's fault. A refusal names the switch.
		return tools.Limits{}, runmesh.Fatal(runmesh.CodeNetworkDenied,
			"tool %q requires network egress and this deployment denies it to every tool "+
				"(set RUNMESH_POLICY_ALLOW_NETWORK=true)", name)
	}

	if want.MaxOutputBytes > 0 && (out.MaxOutputBytes <= 0 || want.MaxOutputBytes < out.MaxOutputBytes) {
		out.MaxOutputBytes = want.MaxOutputBytes
	}
	if e.cfg.MaxTimeout > 0 && (out.Timeout <= 0 || out.Timeout > e.cfg.MaxTimeout) {
		out.Timeout = e.cfg.MaxTimeout
	}
	if e.cfg.MaxAttempts > 0 && (out.MaxAttempts <= 0 || out.MaxAttempts > e.cfg.MaxAttempts) {
		out.MaxAttempts = e.cfg.MaxAttempts
	}

	var err error
	if out.CPU, err = clamp("cpu", want.CPU, e.cfg.DefaultCPU, e.maxCPU); err != nil {
		return tools.Limits{}, err
	}
	if out.Memory, err = clamp("memory", want.Memory, e.cfg.DefaultMemory, e.maxMemory); err != nil {
		return tools.Limits{}, err
	}
	if out.EphemeralStorage, err = clamp("ephemeral-storage", want.EphemeralStorage,
		e.cfg.DefaultEphemeralStorage, e.maxEphemeral); err != nil {
		return tools.Limits{}, err
	}

	if d.Execution == tools.ModeContainer {
		if out.Image, err = e.resolveImage(name, want.Image); err != nil {
			return tools.Limits{}, err
		}
	}
	return out, nil
}

// Allows is the submit-time gate, and the predicate handed to plan validation
// alongside the registry's own. It answers the same question Resolve does but
// without a per-step request, so a plan can be refused before anything is
// persisted.
func (e *Engine) Allows(d tools.Descriptor) error {
	_, err := e.Resolve(d, Request{})
	return err
}

// Effective is what GET /api/v1/tools publishes: the descriptor as it will
// actually behave here.
//
// A tool the policy refuses is still listed, with Denied set and the reason
// attached. Hiding it would make a 400 saying "unknown_tool" the only signal
// that python_execute exists but is switched off — which is the kind of answer
// that costs an afternoon, and which the Week-5 planner cannot act on at all.
func (e *Engine) Effective(d tools.Descriptor) tools.Descriptor {
	out := d
	out.Execution = e.cfg.Mode
	limits, err := e.Resolve(d, Request{})
	if err != nil {
		out.Denied = true
		out.DeniedReason = message(err)
		return out
	}
	// Timeout and attempts are per-step and not part of a descriptor, so only
	// the sandbox dimensions are overwritten here.
	out.Limits.CPU = limits.CPU
	out.Limits.Memory = limits.Memory
	out.Limits.EphemeralStorage = limits.EphemeralStorage
	out.Limits.Network = limits.Network
	out.Limits.Image = limits.Image
	if limits.MaxOutputBytes > 0 {
		out.Limits.MaxOutputBytes = limits.MaxOutputBytes
	}
	if e.cfg.MaxAttempts > 0 && (out.Limits.MaxAttempts <= 0 || out.Limits.MaxAttempts > e.cfg.MaxAttempts) {
		out.Limits.MaxAttempts = e.cfg.MaxAttempts
	}
	return out
}

// Mode reports where this deployment runs tools.
func (e *Engine) Mode() tools.ExecutionMode { return e.cfg.Mode }

// resolveImage picks the image and checks it against the allowed prefixes.
func (e *Engine) resolveImage(tool, want string) (string, error) {
	image := want
	if image == "" {
		image = e.cfg.DefaultImage
	}
	if image == "" {
		return "", runmesh.Fatal(runmesh.CodePolicyViolation,
			"tool %q runs in a container but names no image, and no default image is configured", tool)
	}
	if len(e.cfg.AllowImagePrefixes) == 0 {
		return image, nil
	}
	for _, p := range e.cfg.AllowImagePrefixes {
		if strings.HasPrefix(image, p) {
			return image, nil
		}
	}
	return "", runmesh.Fatal(runmesh.CodePolicyViolation,
		"tool %q names image %q, which matches none of the allowed prefixes (%s)",
		tool, image, strings.Join(e.cfg.AllowImagePrefixes, ", "))
}

// clamp resolves one resource dimension: the tool's request, defaulted if
// absent, then bounded by the ceiling.
//
// The clamp is SILENT by design, unlike the refusals above. A tool asking for
// more CPU than it may have is not an attack and not a misconfiguration — it is
// a portable descriptor meeting a smaller cluster — and the right answer is to
// run it smaller, not to refuse the plan. A quantity that does not parse is a
// different thing entirely and is fatal, because nobody can say what sandbox
// was intended.
func clamp(dimension, want, def string, ceiling *resource.Quantity) (string, error) {
	raw := want
	if raw == "" {
		raw = def
	}
	if raw == "" {
		return "", nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return "", runmesh.Fatal(runmesh.CodePolicyViolation,
			"%s=%q is not a Kubernetes quantity: %v", dimension, raw, err)
	}
	if ceiling != nil && q.Cmp(*ceiling) > 0 {
		return ceiling.String(), nil
	}
	return q.String(), nil
}

// message renders a classified error's text without its code prefix, for the
// human-facing reason on a descriptor.
func message(err error) string {
	var te *runmesh.ToolError
	if errors.As(err, &te) {
		return te.Message
	}
	return err.Error()
}

func set(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
