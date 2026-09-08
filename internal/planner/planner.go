// Package planner turns a goal in English into a validated RunMesh plan.
//
// The pipeline is the whole design, and it is the project plan's own, in order:
//
//	goal ──▶ model ──▶ structured output ──▶ schema validation
//	                                              │
//	                        plan validation ◀─────┘
//	                                │
//	                        policy validation
//	                                │
//	                             scheduler
//
// Every arrow after the first treats what came before as hostile. That is not
// a posture about language models specifically; it is the same posture the API
// takes towards an HTTP client, and the point of designing the submission
// contract this way in Week 1 is that it costs nothing now: a generated plan is
// a runmesh.Plan, so it goes through the identical Validate that a curl request
// does, and then through the identical execution policy.
//
// What the model is trusted with is CHOICE — which tools, in what order, with
// what arguments. What it is not trusted with is anything else: it cannot name
// a tool that is not registered (the response schema's enum makes that
// undecodable and validation rejects it anyway), it cannot raise a limit, it
// cannot reach the network unless an operator switched that on, and it cannot
// widen its own sandbox, because the sandbox is resolved from the operator's
// policy at dispatch and never from the plan. See ADR 0011.
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Model is the generative half, declared here rather than imported so this
// package does not depend on Gemini specifically — and so the pipeline's tests
// can drive it with a model that returns a fixed string.
type Model interface {
	// Generate returns text conforming to req.Schema, or a classified error.
	Generate(ctx context.Context, req Request) (Response, error)
	// Name identifies the model in the trace and in logs.
	Name() string
}

// Request is one generation.
type Request struct {
	System string
	User   string
	Schema json.RawMessage
}

// Response is what came back.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Goal is what a caller asks for.
type Goal struct {
	// Text is the objective, in English. UNTRUSTED: it can come from anywhere
	// and it will contain whatever it contains, including instructions aimed
	// at the model. See prompt.go for what is done about that and — more
	// honestly — what is not.
	Text string
	// Context is optional structured data the plan may refer to: a list of
	// URLs, a table of numbers, whatever the caller already has. It is passed
	// through verbatim and never interpreted here.
	Context json.RawMessage
	// MaxSteps bounds the plan. Zero means the planner's configured default.
	MaxSteps int
	// Name overrides the name the planner chose. Applied by the caller after
	// planning rather than fed into the prompt: it is presentation, and
	// spending prompt tokens on it would be spending them on nothing.
	Name string
}

// Result is a plan and the story of how it was arrived at.
type Result struct {
	Plan  *runmesh.Plan `json:"plan"`
	Trace Trace         `json:"trace"`
}

// Trace is what makes a generated plan reviewable.
//
// It exists because "the model produced this" is not an acceptable answer when
// a plan does something surprising. Every attempt is recorded, with what was
// wrong with it, so a rejected plan is a diagnosable event rather than a 400
// with no history.
type Trace struct {
	Model     string    `json:"model"`
	Attempts  []Attempt `json:"attempts"`
	Reasoning string    `json:"reasoning,omitempty"`
	// Tools is the catalogue the model was actually given — after the
	// execution policy removed what this deployment refuses. A plan is only
	// explicable next to the choices that were available when it was made.
	Tools       []string `json:"tools_offered"`
	DurationMS  int64    `json:"duration_ms"`
	TotalTokens int      `json:"total_tokens"`
}

// Attempt is one round trip, and its verdict.
type Attempt struct {
	N            int              `json:"n"`
	InputTokens  int              `json:"input_tokens"`
	OutputTokens int              `json:"output_tokens"`
	Accepted     bool             `json:"accepted"`
	Problems     []runmesh.Detail `json:"problems,omitempty"`
	// Error is a failure that was not the plan's content: the API refused, the
	// response was truncated, the JSON did not parse.
	Error string `json:"error,omitempty"`
}

// Planner produces plans.
type Planner interface {
	Plan(ctx context.Context, goal Goal) (Result, error)
}

// Catalogue is what the planner is allowed to know about tools. It is
// satisfied by policy.Sandbox, and declared as an interface here so this
// package does not import the policy engine — the same trick httpapi uses.
type Catalogue interface {
	// Descriptors is the EFFECTIVE catalogue: descriptors as this deployment
	// will actually honour them, with the refused ones marked Denied.
	Descriptors() []tools.Descriptor
	// Allows reports whether a tool may run here.
	Allows(tool string) error
}

// Config tunes the pipeline.
type Config struct {
	// MaxSteps caps a generated plan. Separate from the plan-wide limit, and
	// smaller: a model asked for "a plan" with a ceiling of 100 will
	// occasionally produce 40 steps of busywork, and the cost of that is not
	// the validation, it is the execution.
	MaxSteps int

	// MaxRepairs is how many times a rejected plan is handed back with its
	// errors. Bounded, and bounded low: a model that cannot produce a valid
	// plan given the schema, the catalogue and an explicit list of what was
	// wrong is not going to produce one on the fifth try, and each round is a
	// full round trip of latency and tokens.
	MaxRepairs int

	// DefaultTimeout is the per-step timeout applied when a plan does not name
	// one. It is the planner's own default, not the runtime's, because a model
	// omitting a timeout is a different situation from a human doing it.
	DefaultTimeout time.Duration

	// Clock is the only source of time here, as everywhere else in the
	// codebase — the trace's durations included. internal/clock's purity test
	// walks every file under internal/ and fails on a direct time.Now, which
	// is what stops "it is only for a log line" becoming an untestable
	// timeout three packages later.
	Clock clock.Clock

	Log *slog.Logger
}

func (c *Config) setDefaults() {
	if c.MaxSteps < 1 {
		c.MaxSteps = 12
	}
	if c.MaxRepairs < 0 {
		c.MaxRepairs = 0
	}
	if c.MaxRepairs == 0 {
		c.MaxRepairs = 2
	}
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = 60 * time.Second
	}
	if c.Clock == nil {
		c.Clock = clock.System()
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Pipeline is the model-backed planner.
type Pipeline struct {
	model     Model
	catalogue Catalogue
	limits    runmesh.Limits
	cfg       Config
	log       *slog.Logger
}

var _ Planner = (*Pipeline)(nil)

// New builds a pipeline. limits are the runtime's own plan limits, so the
// planner validates against exactly what the API will validate against — a
// planner that accepts what the API rejects is a machine for producing 400s.
func New(model Model, catalogue Catalogue, limits runmesh.Limits, cfg Config) (*Pipeline, error) {
	if model == nil {
		return nil, errors.New("planner: a Model is required")
	}
	if catalogue == nil {
		return nil, errors.New("planner: a Catalogue is required")
	}
	cfg.setDefaults()
	if limits.MaxSteps > 0 && cfg.MaxSteps > limits.MaxSteps {
		cfg.MaxSteps = limits.MaxSteps
	}
	return &Pipeline{
		model:     model,
		catalogue: catalogue,
		limits:    limits,
		cfg:       cfg,
		log:       cfg.Log.With("component", "planner"),
	}, nil
}

// ErrNoTools means the execution policy left nothing for a plan to be built
// from. It is a distinct error because the fix is configuration, not a better
// goal, and asking a model to plan with an empty catalogue produces a
// confidently wrong answer rather than an error.
var ErrNoTools = errors.New("planner: no tools are available to plan with")

// Plan generates, validates, and — if necessary — asks for a repair.
func (p *Pipeline) Plan(ctx context.Context, goal Goal) (Result, error) {
	started := p.cfg.Clock.Now()

	if strings.TrimSpace(goal.Text) == "" {
		return Result{}, &runmesh.ValidationError{Details: []runmesh.Detail{
			{Field: "goal", Issue: "required"},
		}}
	}

	available := p.available()
	if len(available) == 0 {
		return Result{}, ErrNoTools
	}

	maxSteps := goal.MaxSteps
	if maxSteps < 1 || maxSteps > p.cfg.MaxSteps {
		maxSteps = p.cfg.MaxSteps
	}

	schema := ResponseSchema(names(available), maxSteps)
	system := SystemPrompt(available, maxSteps, p.limits)
	user := UserPrompt(goal)

	trace := Trace{Model: p.model.Name(), Tools: names(available)}

	// One attempt, plus MaxRepairs more. Each repair carries the previous
	// candidate and every problem with it, because one problem per round trip
	// costs a round trip per problem.
	for n := 1; n <= p.cfg.MaxRepairs+1; n++ {
		att := Attempt{N: n}

		resp, err := p.model.Generate(ctx, Request{System: system, User: user, Schema: schema})
		att.InputTokens, att.OutputTokens = resp.InputTokens, resp.OutputTokens
		trace.TotalTokens += resp.InputTokens + resp.OutputTokens

		if err != nil {
			att.Error = err.Error()
			trace.Attempts = append(trace.Attempts, att)
			trace.DurationMS = p.cfg.Clock.Since(started).Milliseconds()
			// A generation failure is not a plan problem, so it is returned as
			// itself — already classified retryable or terminal by the model —
			// rather than flattened into a validation error.
			return Result{Trace: trace}, err
		}

		candidate, problems := p.decode(resp.Text)
		if len(problems) == 0 {
			problems = p.validate(candidate.plan)
		}
		att.Problems = problems

		if len(problems) == 0 {
			att.Accepted = true
			trace.Attempts = append(trace.Attempts, att)
			trace.Reasoning = candidate.reasoning
			trace.DurationMS = p.cfg.Clock.Since(started).Milliseconds()
			p.log.Info("plan accepted",
				"attempt", n, "steps", len(candidate.plan.Steps),
				"tokens", trace.TotalTokens)
			return Result{Plan: candidate.plan, Trace: trace}, nil
		}

		trace.Attempts = append(trace.Attempts, att)
		p.log.Info("plan rejected", "attempt", n, "problems", len(problems))
		user = RepairPrompt(goal, resp.Text, problems)
	}

	trace.DurationMS = p.cfg.Clock.Since(started).Milliseconds()
	// The last attempt's problems ARE the answer here: the caller gets the
	// same per-field envelope a hand-written plan would have produced, so a
	// human debugging a refused goal reads the same 400 shape they already
	// know.
	last := trace.Attempts[len(trace.Attempts)-1]
	return Result{Trace: trace}, &runmesh.ValidationError{Details: last.Problems}
}

// available is the catalogue minus what policy refuses.
//
// Denied tools are removed rather than shown-and-marked, which is the opposite
// of what GET /api/v1/tools does, and deliberately so. A human reading the
// catalogue benefits from knowing a tool exists but is switched off. A model
// given the same information will use it: a tool in the prompt is a tool that
// appears in plans, however firmly the surrounding text says not to.
func (p *Pipeline) available() []tools.Descriptor {
	all := p.catalogue.Descriptors()
	out := make([]tools.Descriptor, 0, len(all))
	for _, d := range all {
		if d.Denied {
			continue
		}
		out = append(out, d)
	}
	return out
}

// candidate is one decoded response.
type candidate struct {
	plan      *runmesh.Plan
	reasoning string
}

// decode parses the model's JSON into a runmesh.Plan.
func (p *Pipeline) decode(text string) (candidate, []runmesh.Detail) {
	var raw generatedPlan
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		// Constrained decoding should make this impossible; it is handled
		// anyway, because "the API guarantees it" is a claim about somebody
		// else's system.
		return candidate{}, []runmesh.Detail{{
			Field: "response",
			Issue: "the response is not valid JSON: " + err.Error(),
		}}
	}

	var details []runmesh.Detail
	plan := &runmesh.Plan{
		Name:  strings.TrimSpace(raw.Name),
		Steps: make([]runmesh.PlanStep, 0, len(raw.Steps)),
	}
	if plan.Name == "" {
		plan.Name = "generated plan"
	}

	for i, s := range raw.Steps {
		step := runmesh.PlanStep{
			ID:         strings.TrimSpace(s.ID),
			Tool:       strings.TrimSpace(s.Tool),
			DependsOn:  s.DependsOn,
			TimeoutSec: s.TimeoutSeconds,
		}
		// params travels as a STRING carrying JSON, and that is a workaround
		// for a real constraint rather than a preference: Gemini's response
		// schema is an OpenAPI subset with no free-form object type, so a
		// per-tool params object cannot be expressed at all. Unpacking it here
		// means the malformed case is one clear message about one step instead
		// of a decode failure over the whole document.
		if strings.TrimSpace(s.ParamsJSON) != "" {
			var probe json.RawMessage
			if err := json.Unmarshal([]byte(s.ParamsJSON), &probe); err != nil {
				details = append(details, runmesh.Detail{
					Field: fmt.Sprintf("steps[%d].params_json", i),
					Issue: "not valid JSON: " + err.Error(),
				})
				continue
			}
			step.Params = probe
		}
		if step.TimeoutSec == 0 {
			step.TimeoutSec = int(p.cfg.DefaultTimeout / time.Second)
		}
		plan.Steps = append(plan.Steps, step)
	}
	return candidate{plan: plan, reasoning: strings.TrimSpace(raw.Reasoning)}, details
}

// validate runs the three gates, in the order the project plan specifies, and
// collects every problem rather than stopping at the first.
//
// The order matters. Plan validation first, because it is what makes the rest
// meaningful — a step referring to a step that does not exist has no
// parameters worth checking. Then per-tool parameters. Then policy, which is
// about the deployment rather than the plan.
func (p *Pipeline) validate(plan *runmesh.Plan) []runmesh.Detail {
	known := func(tool string) bool {
		for _, d := range p.available() {
			if d.Name == tool {
				return true
			}
		}
		return false
	}

	var details []runmesh.Detail
	if err := plan.Validate(known, p.limits); err != nil {
		var ve *runmesh.ValidationError
		if errors.As(err, &ve) {
			details = append(details, ve.Details...)
		} else {
			details = append(details, runmesh.Detail{Field: "steps", Issue: err.Error()})
		}
	}
	if len(plan.Steps) > p.cfg.MaxSteps {
		details = append(details, runmesh.Detail{
			Field: "steps",
			Issue: fmt.Sprintf("too_many: %d steps, the planner's limit is %d",
				len(plan.Steps), p.cfg.MaxSteps),
		})
	}

	for i, s := range plan.Steps {
		if err := p.catalogue.Allows(s.Tool); err != nil {
			details = append(details, runmesh.Detail{
				Field: fmt.Sprintf("steps[%d].tool", i),
				Issue: policyIssue(err),
			})
		}
	}
	return details
}

func policyIssue(err error) string {
	var te *runmesh.ToolError
	if errors.As(err, &te) {
		return te.Code + ": " + te.Message
	}
	return err.Error()
}

func names(ds []tools.Descriptor) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

// generatedPlan is the model's output shape. It is NOT runmesh.Plan, and the
// difference is params_json — see decode.
type generatedPlan struct {
	Name      string          `json:"name"`
	Reasoning string          `json:"reasoning"`
	Steps     []generatedStep `json:"steps"`
}

type generatedStep struct {
	ID             string   `json:"id"`
	Tool           string   `json:"tool"`
	ParamsJSON     string   `json:"params_json"`
	DependsOn      []string `json:"depends_on"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}
