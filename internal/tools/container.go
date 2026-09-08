package tools

import (
	"context"
	"encoding/json"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Container is a tool that exists in the registry but has no in-process
// implementation: its behaviour lives in an image, behind the contract in
// cmd/task/main.go.
//
// It is registered anyway, and that is the point. The registry is the
// allowlist, the source of the descriptors GET /api/v1/tools serves, and the
// place submit-time parameter validation hangs. A container tool that were
// merely "known to the Kubernetes executor" would be absent from all three:
// plans naming it would 400 with unknown_tool, its parameters would be
// unchecked until a pod had already been scheduled, and the Week-5 planner
// would never be told it exists.
//
// Run is the backstop, not the gate. internal/policy refuses a container tool
// at SUBMIT time when this deployment executes in-process, so this error should
// be unreachable — but "should be unreachable" is not the same as "cannot
// happen", and the failure mode it guards is arbitrary code running inside the
// API process. Two independent mechanisms, for the same reason the task pod
// gets both an unbound ServiceAccount and automountServiceAccountToken: false.
type Container struct {
	Descriptor Descriptor
	// Check validates params at submit time. Optional; nil means the tool
	// accepts anything the plan's own limits allow.
	Check func(params json.RawMessage) error
}

var (
	_ Tool      = Container{}
	_ Describer = Container{}
	_ Validator = Container{}
)

func (c Container) Describe() Descriptor {
	d := c.Descriptor
	d.Execution = ModeContainer
	return d
}

func (c Container) Validate(params json.RawMessage) error {
	if c.Check == nil {
		return nil
	}
	return c.Check(params)
}

func (c Container) Run(context.Context, Input) (Output, error) {
	return Output{}, runmesh.Fatal(runmesh.CodeToolNotSandboxed,
		"tool %q runs only in a container and this process executes tools in-process; "+
			"set RUNMESH_EXECUTOR=kubernetes", c.Descriptor.Name)
}
