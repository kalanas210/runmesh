// Command task is the reference implementation of RunMesh's container tool
// contract, and the entrypoint of the runmesh/task image.
//
// It exists so Week 3 has something real to run in a pod: the same echo, sleep
// and fail behaviours the in-process tools provide, so the demo plans in the
// README execute identically whether the executor is Local or Kubernetes.
//
// Week 4 adds http_request, the one tool that asks for the network — and the
// only way to observe whether the sandbox NetworkPolicy is enforced, since
// every other task pod runs with egress denied and would not notice either
// way. The Python sandbox is a SEPARATE image against this same contract
// (deploy/docker/python/runner.py), which is what proves the contract is
// "environment in, marker line out" rather than "whatever this binary does".
//
// # The contract
//
// Everything arrives in the environment:
//
//	RUNMESH_TOOL             which behaviour to run
//	RUNMESH_PARAMS           the step's params, as JSON
//	RUNMESH_DEPS             upstream results keyed by step id, as JSON
//	RUNMESH_JOB_ID           identity, for logging
//	RUNMESH_STEP_ID
//	RUNMESH_ATTEMPT
//	RUNMESH_ATTEMPT_ID
//	RUNMESH_IDEMPOTENCY_KEY  stable across attempts; what makes a retry safe
//	RUNMESH_RESULT_MARKER    the sentinel to prefix the result line with
//
// The result is one line on stdout:
//
//	##RUNMESH-RESULT## {"rows":128}
//
// Exit 0 means success. Any other exit code is a terminal failure, which is
// the same default the in-process executor applies to an unclassified error:
// under an at-least-once contract, silently retrying what nobody classified is
// how a side-effecting tool runs three times.
//
// It imports nothing from the rest of the repository except internal/report,
// which is stdlib-only and is a Markdown renderer rather than any part of the
// contract. The marker still comes from the environment rather than from a
// shared constant, so this binary stays a few hundred kilobytes instead of
// linking client-go — and so an image written in any other language is a
// first-class citizen of the same contract.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/kalanas210/runmesh/internal/report"
)

const defaultMarker = "##RUNMESH-RESULT##"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(_ []string, getenv func(string) string, stdout, stderr *os.File) int {
	tool := getenv("RUNMESH_TOOL")
	if tool == "" {
		fmt.Fprintln(stderr, "task: RUNMESH_TOOL is not set")
		return 2
	}

	marker := getenv("RUNMESH_RESULT_MARKER")
	if marker == "" {
		marker = defaultMarker
	}

	var params map[string]any
	if raw := getenv("RUNMESH_PARAMS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &params); err != nil {
			fmt.Fprintf(stderr, "task: RUNMESH_PARAMS is not valid JSON: %v\n", err)
			return 2
		}
	}

	fmt.Fprintf(stdout, "task: tool=%s job=%s step=%s attempt=%s\n",
		tool, getenv("RUNMESH_JOB_ID"), getenv("RUNMESH_STEP_ID"), getenv("RUNMESH_ATTEMPT"))

	result, code := dispatch(tool, params, getenv, stdout, stderr)
	if code != 0 {
		return code
	}

	line, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(stderr, "task: encoding the result: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "%s %s\n", marker, line)
	return 0
}

func dispatch(tool string, params map[string]any, getenv func(string) string,
	stdout, stderr *os.File) (any, int) {

	switch tool {
	case "echo":
		// Mirrors the in-process echo: the params come back, with the identity
		// that produced them, so a plan can prove which attempt ran.
		return map[string]any{
			"echo":    params,
			"job_id":  getenv("RUNMESH_JOB_ID"),
			"step_id": getenv("RUNMESH_STEP_ID"),
			"attempt": atoi(getenv("RUNMESH_ATTEMPT")),
			"deps":    rawDeps(getenv("RUNMESH_DEPS")),
		}, 0

	case "sleep":
		d, err := sleepDuration(params)
		if err != nil {
			fmt.Fprintf(stderr, "task: %v\n", err)
			return nil, 2
		}
		// A real sleep, and the only one in the repository outside the clock
		// package: this process is a container with no injected clock and
		// nothing to synchronise on. RunMesh's own deadline and the Job's
		// activeDeadlineSeconds both bound it from outside.
		time.Sleep(d)
		return map[string]any{"slept_ms": d.Milliseconds()}, 0

	case "report_generate":
		// The one tool whose renderer is shared with the in-process registry,
		// through internal/report. See that package for why this single import
		// does not weaken the "cmd/task imports nothing" argument: the CONTRACT
		// is still environment in, marker line out, and the package is
		// stdlib-only. What it buys is one Markdown renderer instead of two
		// that drift until a plan produces a different report depending on
		// where it ran.
		var opts report.Options
		if raw := getenv("RUNMESH_PARAMS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &opts); err != nil {
				fmt.Fprintf(stderr, "task: report_generate params: %v\n", err)
				return nil, 2
			}
		}
		if err := opts.Validate(); err != nil {
			fmt.Fprintf(stderr, "task: report_generate params: %v\n", err)
			return nil, 2
		}
		return report.Render(opts, rawDeps(getenv("RUNMESH_DEPS")),
			getenv("RUNMESH_JOB_ID"), getenv("RUNMESH_STEP_ID")), 0

	case "http_request":
		// The only tool that touches the network, and the reason the sandbox
		// NetworkPolicy has an allow path at all. See http.go: it refuses to
		// connect to anything that is not a public address, independently of
		// whether the CNI is enforcing anything.
		return httpRequest(params, stderr)

	case "fail":
		// Exists to exercise the failure paths over a real cluster: a step that
		// fails on its first N attempts and then succeeds. RUNMESH_ATTEMPT is
		// what makes that decidable without any state of its own.
		failTimes := 1
		if v, ok := params["fail_times"]; ok {
			failTimes = toInt(v)
		}
		if attempt := atoi(getenv("RUNMESH_ATTEMPT")); attempt <= failTimes {
			fmt.Fprintf(stderr, "task: failing attempt %d of %d on purpose\n", attempt, failTimes)
			return nil, 1
		}
		return map[string]any{"ok": true, "attempt": atoi(getenv("RUNMESH_ATTEMPT"))}, 0

	default:
		fmt.Fprintf(stderr, "task: unknown tool %q\n", tool)
		return nil, 2
	}
}

// sleepDuration accepts either {"duration":"120ms"} or {"seconds":0.12}, the
// same two spellings the in-process sleep tool takes.
func sleepDuration(params map[string]any) (time.Duration, error) {
	if v, ok := params["duration"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("duration %q is not a duration: %w", v, err)
		}
		return d, nil
	}
	if v, ok := params["seconds"].(float64); ok {
		return time.Duration(v * float64(time.Second)), nil
	}
	return 0, nil
}

// rawDeps passes upstream results straight through without interpreting them:
// their shape belongs to whichever tool produced them.
func rawDeps(raw string) map[string]json.RawMessage {
	if raw == "" {
		return nil
	}
	var deps map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil
	}
	return deps
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		return atoi(n)
	default:
		return 0
	}
}
