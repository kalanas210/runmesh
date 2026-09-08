package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// strictUnmarshal rejects a field the schema does not declare, matching the
// "additionalProperties": false in every schema in this file.
//
// Silently ignoring an unrecognised parameter is the wrong failure for a
// caller that is a language model: it will write {"cpu": "8"} because that
// looks like it should work, the field will be dropped without comment, and the
// step will run with a quarter of the resources the plan believed it asked for.
// A 400 naming the field is the only feedback that changes the next plan.
func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// The tools that only ever run inside a pod.
//
// Both are declared here, as data, rather than in the image that implements
// them, because the registry is what the allowlist, the submit-time validator
// and the Week-5 planner all read. The image supplies the behaviour; this file
// supplies the contract.

// PythonExecute runs a snippet of Python in the sandbox image.
//
// This is the tool the whole of Week 4 exists for. Everything about it is
// hostile by default: it never runs in the API process, its pod has no network
// unless an operator has switched network tools on, its root filesystem is
// read-only, it runs as uid 65532 with every capability dropped, and the only
// writable path is a size-capped /tmp.
//
// Note what is NOT claimed: the Python interpreter is not the sandbox. Removing
// builtins, auditing imports and rewriting AST nodes are all bypassable and
// have been repeatedly; treating them as a security boundary is how "we
// restrict `eval`" becomes a shell. The boundary is the pod — the kernel, the
// cgroup and the CNI — and the interpreter inside it is assumed hostile.
func PythonExecute(image string) Container {
	return Container{
		Descriptor: Descriptor{
			Name:    "python_execute",
			Version: "1.0",
			Description: "Runs a short Python 3 program in an isolated, non-root, " +
				"read-only, network-denied pod and returns the value it binds to `result`.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["code"],
  "properties": {
    "code":   {"type": "string", "description": "Python 3 source. Bind the JSON-serialisable answer to a variable named result. The dicts params and deps (upstream step results, keyed by step id) are in scope. Anything printed is kept as logs, not as the result."},
    "params": {"type": "object", "description": "Arbitrary JSON handed to the program as params."}
  },
  "additionalProperties": false
}`),
			Limits: Limits{
				MaxAttempts:      3,
				MaxOutputBytes:   256 << 10,
				CPU:              "500m",
				Memory:           "256Mi",
				EphemeralStorage: "64Mi",
				Image:            image,
				Network:          false,
			},
		},
		Check: validatePython,
	}
}

type pythonParams struct {
	Code   string          `json:"code"`
	Params json.RawMessage `json:"params,omitempty"`
}

// validatePython is a submit-time check, and deliberately a shallow one. It
// rejects an obviously malformed step — no code at all, or a program larger
// than the environment can carry — and nothing more. It does not scan the
// source for dangerous constructs, because a check that rejects `os.system`
// while `__import__("os").system` sails past is worse than no check: it
// produces the belief that the source was vetted.
func validatePython(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("python_execute params: code is required")
	}
	var p pythonParams
	if err := strictUnmarshal(raw, &p); err != nil {
		return fmt.Errorf("python_execute params: %w", err)
	}
	if strings.TrimSpace(p.Code) == "" {
		return fmt.Errorf("python_execute params: code is required")
	}
	if len(p.Code) > maxPythonCodeBytes {
		return fmt.Errorf("python_execute params: code is %d bytes, limit is %d",
			len(p.Code), maxPythonCodeBytes)
	}
	return nil
}

// maxPythonCodeBytes is well under the plan-wide RUNMESH_MAX_PARAMS_BYTES, and
// under it for a different reason: params travel to the pod as an environment
// variable, and the kernel's limit on the whole environment block is the real
// ceiling. Failing here, at submit time, beats failing at exec time with a
// message about argument lists being too long.
const maxPythonCodeBytes = 32 << 10

// HTTPRequest performs one outbound HTTP request from the sandbox.
//
// It is the tool that makes the NetworkPolicy observable. Every other tool runs
// with egress denied, so nothing proves the deny-all policy is doing anything;
// this one asks for egress, gets the runmesh.io/network=allow label, and is
// therefore the pod that fails to reach the internet the moment the policy is
// wrong in either direction.
func HTTPRequest(image string) Container {
	return Container{
		Descriptor: Descriptor{
			Name:    "http_request",
			Version: "1.0",
			Description: "Fetches one HTTP(S) URL from the sandbox and returns its status, " +
				"headers and body. Only public addresses are reachable.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["url"],
  "properties": {
    "url":     {"type": "string", "description": "Absolute http:// or https:// URL. Private, loopback and link-local addresses are refused."},
    "method":  {"type": "string", "enum": ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"]},
    "headers": {"type": "object", "additionalProperties": {"type": "string"}},
    "body":    {"type": "string"},
    "max_bytes": {"type": "integer", "minimum": 1, "description": "Cap on the response body kept. Defaults to the tool's output limit."}
  },
  "additionalProperties": false
}`),
			Limits: Limits{
				MaxAttempts:      3,
				MaxOutputBytes:   256 << 10,
				CPU:              "200m",
				Memory:           "128Mi",
				EphemeralStorage: "16Mi",
				Image:            image,
				Network:          true,
			},
		},
		Check: validateHTTPRequest,
	}
}

type httpParams struct {
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     string            `json:"body,omitempty"`
	MaxBytes int               `json:"max_bytes,omitempty"`
}

// validateHTTPRequest rejects at submit time what the tool would refuse at
// runtime anyway. The scheme and hostname checks are duplicated on purpose:
// here so a bad plan is a 400 rather than a scheduled pod, and again inside the
// tool because the tool is what actually opens the socket — and a check that
// only exists on the far side of a queue is a check an attacker skips by
// finding another way onto the queue.
func validateHTTPRequest(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("http_request params: url is required")
	}
	var p httpParams
	if err := strictUnmarshal(raw, &p); err != nil {
		return fmt.Errorf("http_request params: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil {
		return fmt.Errorf("http_request params: url: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("http_request params: url must be http or https, got %q", u.Scheme)
	case u.Host == "":
		return fmt.Errorf("http_request params: url must be absolute")
	case u.User != nil:
		// Credentials in a URL end up in logs, in the timeline and in the
		// Kubernetes object's environment. There is no way to accept this and
		// still be able to say the timeline is safe to show somebody.
		return fmt.Errorf("http_request params: url must not contain credentials")
	}
	switch strings.ToUpper(p.Method) {
	case "", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
	default:
		return fmt.Errorf("http_request params: unsupported method %q", p.Method)
	}
	if p.MaxBytes < 0 {
		return fmt.Errorf("http_request params: max_bytes must not be negative")
	}
	for k := range p.Headers {
		if strings.EqualFold(k, "host") {
			// Overriding Host is how a request to an allowed address is
			// pointed at a different virtual host, which is the shape of half
			// the SSRF bypasses that survive an IP allowlist.
			return fmt.Errorf("http_request params: the Host header may not be set")
		}
	}
	return nil
}
