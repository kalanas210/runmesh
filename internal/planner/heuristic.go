package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Heuristic is the planner that runs without an API key.
//
// It is not a planner and this file will not pretend otherwise: it is a small
// set of rules over the words in the goal, and it will produce a sensible plan
// for the shapes it recognises and a shrug for everything else. It exists for a
// reason the project plan states directly — "keep the architecture usable
// without Gemini so development and testing do not depend on API spend" — and
// it earns its place three ways:
//
//   - the whole path from goal to executed DAG works on a laptop with no key,
//     no billing account and no network;
//   - it is deterministic, so the end-to-end tests that exercise that path are
//     assertions rather than samples of a model's mood;
//   - it produces a REAL dependency graph — fan-out fetches, a join, a report —
//     so the waterfall in Week 6 has something with a shape to draw.
//
// The output goes through exactly the same validation as a generated plan. That
// is the point of the pipeline being separate from the model: swapping the
// intelligence out does not change what is trusted.
type Heuristic struct {
	catalogue Catalogue
	cfg       Config
}

var _ Planner = (*Heuristic)(nil)

// NewHeuristic builds the keyless planner.
func NewHeuristic(catalogue Catalogue, cfg Config) *Heuristic {
	cfg.setDefaults()
	return &Heuristic{catalogue: catalogue, cfg: cfg}
}

// urlRe finds http(s) URLs in the goal. Deliberately conservative: it stops at
// whitespace and at trailing punctuation, because a URL at the end of an
// English sentence usually has a full stop attached to it.
var urlRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)

// Plan builds a DAG from what it can recognise in the goal.
//
//	every URL in the goal      → one http_request step, all running concurrently
//	python_execute available   → one analysis step joining those fetches
//	report_generate available  → one report step joining everything
//	nothing recognised         → one echo step, so the path still runs end to end
func (h *Heuristic) Plan(ctx context.Context, goal Goal) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(goal.Text) == "" {
		return Result{}, &runmesh.ValidationError{Details: []runmesh.Detail{
			{Field: "goal", Issue: "required"},
		}}
	}

	has := map[string]bool{}
	var offered []string
	for _, d := range h.catalogue.Descriptors() {
		if d.Denied {
			continue
		}
		has[d.Name] = true
		offered = append(offered, d.Name)
	}
	if len(has) == 0 {
		return Result{}, ErrNoTools
	}

	started := h.cfg.Clock.Now()
	plan := &runmesh.Plan{Name: planName(goal.Text)}
	var fetched []string
	var notes []string

	// Fan-out: the URLs, concurrently. Capped well below the step limit so a
	// goal that happens to contain a wall of links does not become a crawler.
	urls := urlRe.FindAllString(goal.Text, -1)
	if len(urls) > maxHeuristicFetches {
		urls = urls[:maxHeuristicFetches]
		notes = append(notes, fmt.Sprintf("only the first %d URLs were used", maxHeuristicFetches))
	}
	switch {
	case len(urls) == 0:
	case !has["http_request"]:
		notes = append(notes, "the goal names URLs but http_request is not available "+
			"in this deployment, so nothing is fetched")
	default:
		for i, u := range urls {
			id := fmt.Sprintf("fetch_%d", i+1)
			plan.Steps = append(plan.Steps, runmesh.PlanStep{
				ID:         id,
				Tool:       "http_request",
				Params:     mustJSON(map[string]any{"url": u}),
				TimeoutSec: int(h.cfg.DefaultTimeout / time.Second),
			})
			fetched = append(fetched, id)
		}
	}

	// The join: one analysis over whatever was fetched.
	analysis := ""
	if has["python_execute"] {
		analysis = "analyze"
		plan.Steps = append(plan.Steps, runmesh.PlanStep{
			ID:         analysis,
			Tool:       "python_execute",
			DependsOn:  fetched,
			Params:     mustJSON(map[string]any{"code": summariseCode, "params": map[string]any{"goal": goal.Text}}),
			TimeoutSec: int(h.cfg.DefaultTimeout / time.Second),
		})
	}

	// The report: one document over everything that came before.
	if has["report_generate"] {
		deps := fetched
		if analysis != "" {
			// Depending on the analysis alone would be wrong: the fetches'
			// results reach the report only through steps that depend on them.
			deps = append(append([]string{}, fetched...), analysis)
		}
		plan.Steps = append(plan.Steps, runmesh.PlanStep{
			ID:        "report",
			Tool:      "report_generate",
			DependsOn: deps,
			Params: mustJSON(map[string]any{
				"title":   planName(goal.Text),
				"summary": strings.TrimSpace(goal.Text),
			}),
			TimeoutSec: int(h.cfg.DefaultTimeout / time.Second),
		})
	}

	if len(plan.Steps) == 0 {
		// Nothing was recognised. An echo step is not useful work, and saying
		// so in the reasoning is the honest answer — but the alternative, an
		// error, would break the one thing this planner exists to guarantee:
		// that goal → plan → execution works with no API key at all.
		if !has["echo"] {
			return Result{}, ErrNoTools
		}
		plan.Steps = append(plan.Steps, runmesh.PlanStep{
			ID:         "restate",
			Tool:       "echo",
			Params:     mustJSON(map[string]any{"goal": strings.TrimSpace(goal.Text)}),
			TimeoutSec: int(h.cfg.DefaultTimeout / time.Second),
		})
		notes = append(notes, "no URLs and no analysable structure were recognised in the goal")
	}

	reasoning := "Built without a language model by internal/planner.Heuristic: " +
		"URLs in the goal became concurrent fetches, joined by an analysis and a report. " +
		"Set RUNMESH_GEMINI_API_KEY for an actual planner."
	if len(notes) > 0 {
		reasoning += " Notes: " + strings.Join(notes, "; ") + "."
	}

	return Result{
		Plan: plan,
		Trace: Trace{
			Model:      "heuristic",
			Reasoning:  reasoning,
			Tools:      offered,
			DurationMS: h.cfg.Clock.Since(started).Milliseconds(),
			Attempts:   []Attempt{{N: 1, Accepted: true}},
		},
	}, nil
}

const maxHeuristicFetches = 3

// summariseCode is what the heuristic's analysis step runs. It is deliberately
// dull — count and describe what arrived — because its job is to prove that
// dependency results actually reach a step, not to be clever.
const summariseCode = `
summary = {}
for step_id, value in deps.items():
    if isinstance(value, dict):
        body = value.get("body") or ""
        summary[step_id] = {
            "status": value.get("status"),
            "bytes": value.get("body_bytes", len(body)),
            "content_type": value.get("content_type"),
        }
    else:
        summary[step_id] = {"type": type(value).__name__}
result = {"goal": params.get("goal"), "sources": len(deps), "summary": summary}
`

// planName derives a short, stable name from the goal.
func planName(goal string) string {
	name := strings.Join(strings.Fields(goal), " ")
	if len(name) > 60 {
		// Cut at a word boundary where there is one nearby, so the name reads
		// as a truncated phrase rather than a truncated word.
		cut := strings.LastIndex(name[:60], " ")
		if cut < 30 {
			cut = 60
		}
		name = name[:cut] + "..."
	}
	if name == "" {
		name = "generated plan"
	}
	return name
}

// mustJSON marshals a map built entirely from strings, numbers and maps. It
// cannot fail for those inputs, and a nil result would be caught by plan
// validation immediately if it somehow did.
func mustJSON(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
