package runmesh

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Plan is the SUBMISSION contract. POST /api/v1/jobs decodes into it today,
// and in Week 5 the Gemini planner produces this exact struct — so validation
// and persistence are unchanged, and "never trust raw model output" is
// satisfied by code that already exists and is already tested.
type Plan struct {
	Name          string        `json:"name"`
	Priority      int           `json:"priority,omitempty"`
	OnStepFailure FailurePolicy `json:"on_step_failure,omitempty"`
	Steps         []PlanStep    `json:"steps"`
}

// PlanStep is one node of the submitted DAG.
type PlanStep struct {
	ID          string          `json:"id"`
	Tool        string          `json:"tool"`
	Params      json.RawMessage `json:"params,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	TimeoutSec  int             `json:"timeout_seconds,omitempty"`
	MaxAttempts int             `json:"max_attempts,omitempty"`
}

// Limits bound a plan. They come from config, never from a magic number, so
// the ceiling a caller hits is the ceiling an operator configured.
type Limits struct {
	MaxSteps       int
	MaxDependsOn   int
	MaxParamsBytes int
	MaxStepTimeout time.Duration
	MaxAttempts    int
	MaxResultBytes int
}

// Defaults fill in what a plan step leaves unset.
type Defaults struct {
	StepTimeout time.Duration
	MaxAttempts int
}

var stepIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// maxBuildTimeout is the absolute ceiling Build honours, independent of
// configuration. It exists only to make an unvalidated plan safe; the real,
// operator-configured limit is Limits.MaxStepTimeout, checked in Validate.
const maxBuildTimeout = 24 * time.Hour

// timeoutInRange bounds the raw integer BEFORE it is converted to a Duration.
//
// time.Duration(n) * time.Second overflows int64 for n above roughly 9.2e9 and
// wraps to a NEGATIVE value, which then compares as comfortably under any
// ceiling. A step built from it would get a deadline already in the past, its
// context would be expired the instant it was created, and every attempt would
// time out immediately until the retry budget was gone — all from a plan that
// passed validation.
func timeoutInRange(sec int, max time.Duration) bool {
	if sec < 0 {
		return false
	}
	return int64(sec) <= int64(max/time.Second)
}

// Validate is the ONE gate between an untrusted plan and the runtime. It
// reports every problem it finds rather than stopping at the first, so a 400
// body tells the caller everything at once — which matters far more when the
// author is an LLM retrying in a loop than when it is a human.
//
// known reports whether a tool name is registered; passing the registry's
// lookup here is what makes "allowlisted tools" a property of submission
// rather than of execution.
func (p *Plan) Validate(known func(tool string) bool, lim Limits) error {
	var d []Detail
	add := func(field, issue string) { d = append(d, Detail{Field: field, Issue: issue}) }

	if strings.TrimSpace(p.Name) == "" {
		add("name", "required")
	}
	if len(p.Steps) == 0 {
		add("steps", "required")
	}
	if lim.MaxSteps > 0 && len(p.Steps) > lim.MaxSteps {
		add("steps", "too_many")
	}

	// seen maps step id -> 1-based declaration index, so zero means "not
	// declared" and the dependency pass below can test membership without a
	// second map.
	seen := make(map[string]int, len(p.Steps))
	for i, s := range p.Steps {
		f := "steps[" + strconv.Itoa(i) + "]"
		switch {
		case !stepIDRe.MatchString(s.ID):
			add(f+".id", "malformed")
		case seen[s.ID] > 0:
			add(f+".id", "duplicate")
		}
		if seen[s.ID] == 0 {
			seen[s.ID] = i + 1
		}
		if !known(s.Tool) {
			add(f+".tool", "unknown_tool")
		}
		if lim.MaxParamsBytes > 0 && len(s.Params) > lim.MaxParamsBytes {
			add(f+".params", "too_large")
		}
		if len(s.Params) > 0 && !json.Valid(s.Params) {
			add(f+".params", "invalid_json")
		}
		if lim.MaxDependsOn > 0 && len(s.DependsOn) > lim.MaxDependsOn {
			add(f+".depends_on", "too_many")
		}
		if !timeoutInRange(s.TimeoutSec, lim.MaxStepTimeout) {
			add(f+".timeout_seconds", "out_of_range")
		}
		if s.MaxAttempts < 0 || s.MaxAttempts > lim.MaxAttempts {
			add(f+".max_attempts", "out_of_range")
		}
	}

	for i, s := range p.Steps {
		f := "steps[" + strconv.Itoa(i) + "].depends_on"
		dup := make(map[string]bool, len(s.DependsOn))
		for k, dep := range s.DependsOn {
			fk := f + "[" + strconv.Itoa(k) + "]"
			switch {
			case dep == s.ID:
				add(fk, "self_dependency")
			case seen[dep] == 0:
				add(fk, "unknown_step")
			case dup[dep]:
				add(fk, "duplicate")
			}
			dup[dep] = true
		}
	}

	// Cycle detection runs only once ids and references are sound, so its
	// message can never be confused by a duplicate or dangling id.
	if len(d) == 0 {
		if cyc := findCycle(p.Steps); len(cyc) > 0 {
			add("steps", "cycle:"+strings.Join(cyc, ","))
		}
	}
	if len(d) > 0 {
		return &ValidationError{Details: d}
	}
	return nil
}

// findCycle returns the step ids that never settle, in declaration order.
// Kahn's algorithm: O(V+E), and deterministic, which is what makes the 400
// body stable enough to pin with a golden test.
func findCycle(steps []PlanStep) []string {
	indeg := make(map[string]int, len(steps))
	out := make(map[string][]string, len(steps))
	for _, s := range steps {
		indeg[s.ID] += 0
	}
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if _, ok := indeg[dep]; !ok {
				continue // dangling; already reported by Validate
			}
			indeg[s.ID]++
			out[dep] = append(out[dep], s.ID)
		}
	}
	queue := make([]string, 0, len(steps))
	for _, s := range steps {
		if indeg[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}
	settled := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		settled++
		for _, m := range out[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if settled == len(indeg) {
		return nil
	}
	var stuck []string
	for _, s := range steps {
		if indeg[s.ID] > 0 {
			stuck = append(stuck, s.ID)
		}
	}
	return stuck
}

// Build materialises a validated Plan into a Job. It is pure: id and now are
// supplied by the caller, so the whole function is deterministic under test
// and this package needs no clock and no randomness.
func (p *Plan) Build(id string, now time.Time, def Defaults) *Job {
	j := &Job{
		ID:            id,
		Name:          p.Name,
		State:         Queued,
		Priority:      p.Priority,
		OnStepFailure: p.OnStepFailure,
		CreatedAt:     now,
		UpdatedAt:     now,
		Version:       1,
		Steps:         make([]*Step, len(p.Steps)),
	}
	for i, ps := range p.Steps {
		// Guarded again here, not only in Validate: Build is exported, and a
		// caller that skipped validation must still not be able to install a
		// negative or wrapped deadline.
		timeout := def.StepTimeout
		if ps.TimeoutSec > 0 && timeoutInRange(ps.TimeoutSec, maxBuildTimeout) {
			timeout = time.Duration(ps.TimeoutSec) * time.Second
		}
		attempts := def.MaxAttempts
		if ps.MaxAttempts > 0 {
			attempts = ps.MaxAttempts
		}
		j.Steps[i] = &Step{
			ID:            ps.ID,
			Tool:          ps.Tool,
			Params:        cloneRaw(ps.Params),
			DependsOn:     cloneStrings(ps.DependsOn),
			State:         Queued,
			Timeout:       timeout,
			MaxAttempts:   attempts,
			NextAttemptAt: now,
			Version:       1,
		}
	}
	return j
}
