package bench_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// benchLimits are the shipped admission limits, taken from internal/config's
// defaults rather than invented here. Benchmarking Validate against a
// hand-picked ceiling would measure a configuration nobody runs.
var benchLimits = runmesh.Limits{
	MaxSteps:       100,
	MaxDependsOn:   32,
	MaxParamsBytes: 64 << 10,
	MaxStepTimeout: 15 * time.Minute,
	MaxAttempts:    10,
	MaxResultBytes: 256 << 10,
}

func knownTool(string) bool { return true }

// BenchmarkPlanValidate measures the one gate between an untrusted plan and the
// runtime.
//
// It is on the submission path, once per POST /api/v1/jobs, so it is not hot in
// the way Claim is. It is benchmarked anyway for two reasons. The first is that
// it is the only place in the request path that does real work proportional to
// the input — a regexp per step, a map build, and a cycle search over the whole
// graph — so it is where a plan-shaped denial of service would land if one
// existed. The second is that Week 5 made a language model the author: the
// planner's repair loop calls Validate once per rejected candidate, up to
// RUNMESH_PLANNER_MAX_REPAIRS + 1 times for a single goal, and that loop is
// latency a user waits on.
//
// The two shapes are the two ends of what MaxDependsOn permits. A chain is the
// cheapest graph with the deepest cycle search; a fan-in is the most edges a
// hundred-step plan can carry, and it is where the dependency pass actually
// costs something.
func BenchmarkPlanValidate(b *testing.B) {
	for _, tc := range []struct {
		name string
		plan *runmesh.Plan
	}{
		{"shape=chain/steps=10", chainPlan(10)},
		{"shape=chain/steps=100", chainPlan(100)},
		{"shape=fanin/steps=100", faninPlan(100)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := tc.plan.Validate(knownTool, benchLimits); err != nil {
					b.Fatalf("the fixture plan does not validate: %v", err)
				}
			}
			b.StopTimer()
			reportRate(b, b.N)
		})
	}
}

// BenchmarkReadiness measures the DAG readiness computation: jobstate.Claimable
// over every step of a job.
//
// This is the hottest pure function in the system and it is hot in a way that
// is easy to miss. Readiness is a PREDICATE, never a stored state — there is no
// BLOCKED state and no materialised pending-dependency counter, which is
// precisely why there is no "who unblocks this step" question to get wrong. The
// price of that correctness property is that memstore evaluates it for every
// step of every job on every Claim and on every QueueDepth, and pgstore
// evaluates the same predicate as two correlated NOT EXISTS subqueries on every
// claim and on every readiness probe. So the cost of the predicate multiplies
// by the queue, and this benchmark is the multiplier.
//
// One iteration is one whole job swept, not one step, because that is the unit
// the callers actually do.
func BenchmarkReadiness(b *testing.B) {
	for _, tc := range []struct {
		name string
		job  *runmesh.Job
	}{
		{"shape=chain/steps=100", buildJob(chainPlan(100))},
		{"shape=fanin/steps=100", buildJob(faninPlan(100))},
		{"shape=fanin/steps=100/deps=met", succeeded(buildJob(faninPlan(100)))},
		{"shape=flat/steps=100", buildJob(flatPlan(100))},
	} {
		b.Run(tc.name, func(b *testing.B) {
			j := tc.job
			b.ReportAllocs()
			b.ResetTimer()
			var claimable int
			for range b.N {
				for _, s := range j.Steps {
					if jobstate.Claimable(j, s, epoch, nil) {
						claimable++
					}
				}
			}
			b.StopTimer()
			// One sweep is one operation, and the per-step rate is what a
			// reader multiplies by their queue depth.
			reportRate(b, b.N)
			b.ReportMetric(float64(b.N*len(j.Steps))/b.Elapsed().Seconds(), "steps/sec")
			// Guard against the compiler deciding the loop has no effect, and
			// against a fixture that is accidentally all-blocked and therefore
			// exits Claimable on its first branch every time.
			if claimable == 0 {
				b.Fatal("no step was ever claimable: the fixture makes this benchmark measure an early return")
			}
		})
	}
}

// BenchmarkBlockedBy measures the derived field the dashboard reads.
//
// runmesh.Job.BlockedBy is computed per REQUEST, on every step of every job in
// a GET /api/v1/jobs page — it is not stored, deliberately, because a stored
// copy of a derived truth is a stored copy that goes stale. That decision is
// paid for here, once per step per response, and a list endpoint returning a
// page of hundred-step jobs pays it a few thousand times.
func BenchmarkBlockedBy(b *testing.B) {
	j := buildJob(faninPlan(100))
	last := j.Steps[len(j.Steps)-1]

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := j.BlockedBy(last); len(got) == 0 {
			b.Fatal("the terminal step of a fan-in reports nothing blocking it")
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

// ─── plan shapes ─────────────────────────────────────────────────────────────

// chainPlan is s0 -> s1 -> ... -> sN-1: the deepest graph the step limit
// allows, and the worst case for the cycle search.
func chainPlan(n int) *runmesh.Plan {
	p := &runmesh.Plan{Name: "chain", Steps: make([]runmesh.PlanStep, 0, n)}
	for i := range n {
		s := runmesh.PlanStep{ID: "s" + strconv.Itoa(i), Tool: "echo", Params: nullResult}
		if i > 0 {
			s.DependsOn = []string{"s" + strconv.Itoa(i-1)}
		}
		p.Steps = append(p.Steps, s)
	}
	return p
}

// faninPlan is the widest graph MaxDependsOn permits: a flat rank of leaves and
// a terminal step depending on the last 32 of them, which is the shipped
// RUNMESH_MAX_DEPENDS_ON ceiling.
func faninPlan(n int) *runmesh.Plan {
	p := &runmesh.Plan{Name: "fanin", Steps: make([]runmesh.PlanStep, 0, n)}
	for i := range n - 1 {
		p.Steps = append(p.Steps, runmesh.PlanStep{
			ID: "s" + strconv.Itoa(i), Tool: "echo", Params: nullResult,
		})
	}
	deps := make([]string, 0, benchLimits.MaxDependsOn)
	for i := max(0, n-1-benchLimits.MaxDependsOn); i < n-1; i++ {
		deps = append(deps, "s"+strconv.Itoa(i))
	}
	p.Steps = append(p.Steps, runmesh.PlanStep{
		ID: "report", Tool: "echo", Params: nullResult, DependsOn: deps,
	})
	return p
}

// flatPlan is n independent steps: no edges at all, which is the shape the
// throughput benchmarks queue and therefore the readiness cost those numbers
// already include.
func flatPlan(n int) *runmesh.Plan {
	p := &runmesh.Plan{Name: "flat", Steps: make([]runmesh.PlanStep, 0, n)}
	for i := range n {
		p.Steps = append(p.Steps, runmesh.PlanStep{
			ID: "s" + strconv.Itoa(i), Tool: "echo", Params: nullResult,
		})
	}
	return p
}

// succeeded marks every step but the last SUCCEEDED, which is the only state in
// which the dependency walk runs to completion.
//
// Without it the fan-in fixture measures an early return and nothing else:
// Claimable exits on the FIRST unsatisfied dependency, so a terminal step whose
// thirty-two upstreams are all still QUEUED costs exactly one Job.Step lookup.
// The moment worth measuring is the opposite one — the instant a rank finishes
// and the next Claim has to walk all thirty-two edges before it can say yes —
// because that is the evaluation the dispatcher does at every dependency
// boundary in every job, and it is the one that is quadratic in the job size
// through Job.Step's linear lookup.
func succeeded(j *runmesh.Job) *runmesh.Job {
	for _, s := range j.Steps[:len(j.Steps)-1] {
		s.State = runmesh.Succeeded
	}
	return j
}

func buildJob(p *runmesh.Plan) *runmesh.Job {
	return p.Build("job_bench", epoch, runmesh.Defaults{
		StepTimeout: 30 * time.Second,
		MaxAttempts: 3,
	})
}
