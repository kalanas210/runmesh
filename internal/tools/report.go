package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kalanas210/runmesh/internal/report"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Report renders a Markdown document from its dependencies' results.
//
// It is the join at the bottom of the plan's Definition of Done — "analyse
// these files and generate a report" — and it is deliberately the least clever
// tool in the registry. Everything upstream of it may be arbitrary code in a
// pod; this is pure formatting over data that has already been produced, with
// no execution, no network and no filesystem. That is why it is safe in-process
// and why its output is completely determined by its inputs.
//
// The interesting design point is that it takes no data of its own. What it
// renders is `deps` — the results of the steps it depends on, handed to it by
// the runtime, keyed by step id. A report tool that fetched its own inputs
// would need a store handle, and tools do not get one.
//
// The rendering itself lives in internal/report because cmd/task implements the
// same tool for the container path, and one renderer written twice is one
// renderer that drifts.
type Report struct{}

var (
	_ Tool      = Report{}
	_ Describer = Report{}
	_ Validator = Report{}
)

func (Report) Describe() Descriptor {
	return Descriptor{
		Name:    "report_generate",
		Version: "1.0",
		Description: "Renders a Markdown report from the results of the steps it depends on. " +
			"Takes no input data of its own: put it at the end of a plan and list the " +
			"steps whose output it should present in depends_on.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":   {"type": "string", "description": "Heading for the report."},
    "summary": {"type": "string", "description": "A paragraph placed under the heading."},
    "include": {"type": "array", "items": {"type": "string"}, "description": "Step ids to present, in this order. Defaults to every dependency, in id order."},
    "raw":     {"type": "boolean", "description": "Append each step's full result as JSON. Defaults to true."}
  },
  "additionalProperties": false
}`),
		Limits: Limits{
			MaxAttempts:      3,
			MaxOutputBytes:   256 << 10,
			CPU:              "100m",
			Memory:           "64Mi",
			EphemeralStorage: "16Mi",
		},
		Execution: ModeInProcess,
	}
}

func (Report) Validate(params json.RawMessage) error {
	if len(params) == 0 {
		return nil
	}
	var o report.Options
	if err := strictUnmarshal(params, &o); err != nil {
		return fmt.Errorf("report_generate params: %w", err)
	}
	if err := o.Validate(); err != nil {
		return fmt.Errorf("report_generate params: %w", err)
	}
	return nil
}

func (Report) Run(ctx context.Context, in Input) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	var o report.Options
	if len(in.Params) > 0 {
		if err := strictUnmarshal(in.Params, &o); err != nil {
			return Output{}, runmesh.Fatal(runmesh.CodeContractBroken,
				"report_generate params: %v", err)
		}
	}

	out, err := json.Marshal(report.Render(o, in.Deps, in.JobID, in.StepID))
	if err != nil {
		return Output{}, runmesh.Fatal(runmesh.CodeContractBroken,
			"report_generate: encoding the result: %v", err)
	}
	return Output{Result: out, Meta: map[string]string{"format": "markdown"}}, nil
}
