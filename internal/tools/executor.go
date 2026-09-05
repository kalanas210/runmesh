package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Executor is THE Kubernetes seam.
//
// The engine depends on this interface and never on Tool, so Week 3 registers
// a k8sjob.Executor whose Execute creates a Kubernetes Job, watches it and
// collects its result — and the dispatcher, worker pool, lease handling,
// classification and state machine do not change at all.
type Executor interface {
	Execute(ctx context.Context, in Input) (Output, error)
}

// Local runs a tool in this process. It owns the three things that must happen
// around every tool invocation regardless of what the tool does:
//
//   - registry lookup, so an unregistered tool fails as tool_unknown rather
//     than as a nil-pointer panic;
//   - the panic barrier, on the tool's own goroutine, so a panicking tool can
//     neither reach the worker's stack nor skip the outcome write;
//   - the output cap and JSON check, so one runaway tool cannot put a
//     megabyte of unparseable bytes into the job record.
type Local struct {
	Registry       Registry
	MaxOutputBytes int
	Log            *slog.Logger
}

var _ Executor = Local{}

// Execute looks the tool up, runs it behind a panic barrier, and validates
// what comes back. Every failure it produces is a classified *runmesh.ToolError
// so the engine's classifier never has to guess.
func (l Local) Execute(ctx context.Context, in Input) (out Output, err error) {
	tool, ok := l.Registry.Lookup(in.Tool)
	if !ok {
		return Output{}, runmesh.Fatal(runmesh.CodeToolUnknown, "tool %q is not registered", in.Tool)
	}

	// Recovering here rather than in the worker keeps the panic on the
	// goroutine that caused it, and turns it into an ordinary terminal outcome
	// that still gets written to the store.
	defer func() {
		if r := recover(); r != nil {
			log := l.Log
			if log == nil {
				log = slog.Default()
			}
			log.Error("tool panicked",
				"tool", in.Tool, "attempt_id", in.AttemptID,
				"panic", r, "stack", string(debug.Stack()))
			out = Output{}
			err = runmesh.Fatal(runmesh.CodePanic, "tool panicked: %v", r)
		}
	}()

	out, err = tool.Run(ctx, in)
	if err != nil {
		return Output{}, err
	}

	limit := in.Limits.MaxOutputBytes
	if limit <= 0 {
		limit = l.MaxOutputBytes
	}
	if limit > 0 && len(out.Result) > limit {
		return Output{}, runmesh.Fatal(runmesh.CodeOutputTooLarge,
			"tool returned %d bytes, limit is %d", len(out.Result), limit)
	}
	if len(out.Result) > 0 && !json.Valid(out.Result) {
		return Output{}, runmesh.Fatal(runmesh.CodeContractBroken,
			"tool returned a result that is not valid JSON")
	}
	return out, nil
}
