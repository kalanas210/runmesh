package tools_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// TestContainerToolsRefuseToRunInProcess.
//
// internal/policy rejects these at submit time, so this path should be
// unreachable. It is asserted anyway, because "unreachable" and "cannot happen"
// are different claims and the thing on the other side of this one is arbitrary
// code executing inside the API process.
func TestContainerToolsRefuseToRunInProcess(t *testing.T) {
	t.Parallel()

	reg := tools.Builtins(tools.Options{
		TaskImage:   "runmesh/task:dev",
		PythonImage: "runmesh/python:dev",
	})
	exec := localExecutor(reg, 1<<20)

	for _, name := range []string{"python_execute", "http_request"} {
		in := input(name, `{"code":"result = 1","url":"https://example.com"}`)
		_, err := exec.Execute(t.Context(), in)
		if err == nil {
			t.Fatalf("%s ran in this process", name)
		}
		te := toolError(t, err)
		if te.Code != runmesh.CodeToolNotSandboxed {
			t.Errorf("%s: code = %q, want %q", name, te.Code, runmesh.CodeToolNotSandboxed)
		}
		if te.Retryable {
			t.Errorf("%s: the refusal is retryable; no number of attempts adds a "+
				"pod to a process that has none", name)
		}
	}
}

// TestContainerToolsDescribeThemselvesAsContainers. The descriptor's Execution
// field is what the policy engine gates on, so a container tool that forgot to
// say so would be permitted in-process.
func TestContainerToolsDescribeThemselvesAsContainers(t *testing.T) {
	t.Parallel()

	for _, tool := range []tools.Container{
		tools.PythonExecute("runmesh/python:dev"),
		tools.HTTPRequest("runmesh/task:dev"),
	} {
		d := tool.Describe()
		if d.Execution != tools.ModeContainer {
			t.Errorf("%s execution = %q, want %q", d.Name, d.Execution, tools.ModeContainer)
		}
		if d.Limits.Image == "" {
			t.Errorf("%s names no image", d.Name)
		}
		if !json.Valid(d.InputSchema) {
			t.Errorf("%s has an input schema that is not valid JSON; it is served "+
				"verbatim to callers and handed to the planner as a function "+
				"declaration", d.Name)
		}
	}
}

// TestSandboxedToolsAreOnlyRegisteredWithAnImage. A registry entry with no
// image passes plan validation and then fails every attempt at build time;
// absent is a better answer, because the 400 says unknown_tool and lists what
// IS available.
func TestSandboxedToolsAreOnlyRegisteredWithAnImage(t *testing.T) {
	t.Parallel()

	bare := tools.Builtins(tools.Options{})
	for _, name := range []string{"python_execute", "http_request"} {
		if bare.Has(name) {
			t.Errorf("%s is registered with no image configured for it", name)
		}
	}

	full := tools.Builtins(tools.Options{TaskImage: "t", PythonImage: "p"})
	for _, name := range []string{"python_execute", "http_request", "echo", "sleep"} {
		if !full.Has(name) {
			t.Errorf("%s is missing from a fully configured registry", name)
		}
	}
}

// TestOnlyHTTPRequestAsksForTheNetwork. Every other tool must resolve to a pod
// labelled runmesh.io/network=deny, which is what the default-deny
// NetworkPolicy selects.
func TestOnlyHTTPRequestAsksForTheNetwork(t *testing.T) {
	t.Parallel()

	reg := tools.Builtins(tools.Options{
		EnableTestTools: true, TaskImage: "t", PythonImage: "p",
	})
	for _, d := range reg.Descriptors() {
		if d.Limits.Network && d.Name != "http_request" {
			t.Errorf("%s asks for network egress; only http_request should", d.Name)
		}
	}
}

// ------------------------------------------------------- submit-time checks

func TestPythonExecuteValidation(t *testing.T) {
	t.Parallel()

	tool := tools.PythonExecute("runmesh/python:dev")
	for name, tc := range map[string]struct {
		params string
		ok     bool
	}{
		"a program":            {`{"code":"result = 2 + 2"}`, true},
		"with params":          {`{"code":"result = params['x']","params":{"x":1}}`, true},
		"no code":              {`{}`, false},
		"empty code":           {`{"code":"   "}`, false},
		"nothing at all":       {``, false},
		"code is not a string": {`{"code":42}`, false},
		"unknown field":        {`{"code":"x","cpu":"64"}`, false},
	} {
		err := tool.Validate(json.RawMessage(tc.params))
		if tc.ok && err != nil {
			t.Errorf("%s: rejected a valid step: %v", name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted %s", name, tc.params)
		}
	}
}

// TestPythonExecuteDoesNotPretendToVetTheSource. A check that rejects
// os.system while __import__("os").system sails past is worse than no check:
// it manufactures the belief that the source was reviewed. The sandbox is the
// pod, and this test pins that decision so nobody "improves" it into a filter.
func TestPythonExecuteDoesNotPretendToVetTheSource(t *testing.T) {
	t.Parallel()

	tool := tools.PythonExecute("runmesh/python:dev")
	hostile := `{"code":"import os, socket, subprocess\nresult = os.listdir('/')"}`
	if err := tool.Validate(json.RawMessage(hostile)); err != nil {
		t.Fatalf("submit-time validation rejected code on its content: %v.\n"+
			"That is a filter, and a bypassable one. If this behaviour is wanted, "+
			"it belongs in a documented policy with its bypasses stated, not in a "+
			"parameter check that reads like a security boundary", err)
	}
}

func TestHTTPRequestValidation(t *testing.T) {
	t.Parallel()

	tool := tools.HTTPRequest("runmesh/task:dev")
	for name, tc := range map[string]struct {
		params string
		ok     bool
	}{
		"a fetch":            {`{"url":"https://example.com/data.csv"}`, true},
		"with a method":      {`{"url":"https://example.com","method":"POST","body":"{}"}`, true},
		"no url":             {`{}`, false},
		"relative url":       {`{"url":"/data.csv"}`, false},
		"file scheme":        {`{"url":"file:///etc/passwd"}`, false},
		"gopher scheme":      {`{"url":"gopher://example.com"}`, false},
		"credentials in url": {`{"url":"https://user:pass@example.com"}`, false},
		"host header":        {`{"url":"https://example.com","headers":{"Host":"internal"}}`, false},
		"unknown method":     {`{"url":"https://example.com","method":"TRACE"}`, false},
		"negative max_bytes": {`{"url":"https://example.com","max_bytes":-1}`, false},
	} {
		err := tool.Validate(json.RawMessage(tc.params))
		if tc.ok && err != nil {
			t.Errorf("%s: rejected a valid step: %v", name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted %s", name, tc.params)
		}
	}
}

// TestHTTPRequestRefusesCredentialsInTheURL, specifically, because the failure
// is not "the request goes to the wrong place" but "the password is now in the
// timeline, the logs and the pod's environment".
func TestHTTPRequestRefusesCredentialsInTheURL(t *testing.T) {
	t.Parallel()

	err := tools.HTTPRequest("t").Validate(json.RawMessage(`{"url":"https://admin:hunter2@example.com/"}`))
	if err == nil {
		t.Fatal("a URL carrying credentials was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection message contains the password it rejected: %q", err)
	}
}
