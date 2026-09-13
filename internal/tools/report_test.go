package tools_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/tools"
)

func runReport(t *testing.T, params string, deps map[string]json.RawMessage) map[string]any {
	t.Helper()
	in := input("report_generate", params)
	in.Deps = deps

	out, err := tools.Report{}.Run(t.Context(), in)
	if err != nil {
		t.Fatalf("report_generate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Result, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	return got
}

// TestReportRendersItsDependencies.
//
// The tool takes no data of its own: what it renders is the results of the
// steps it depends on, handed to it by the runtime. A report tool that fetched
// its own inputs would need a store handle, and tools do not get one.
func TestReportRendersItsDependencies(t *testing.T) {
	t.Parallel()

	got := runReport(t, `{"title":"Quarterly","summary":"Two sources."}`,
		map[string]json.RawMessage{
			"fetch_1": json.RawMessage(`{"status":200,"body_bytes":1024}`),
			"analyze": json.RawMessage(`{"rows":42}`),
		})

	md, _ := got["markdown"].(string)
	for _, want := range []string{"# Quarterly", "Two sources.", "## analyze", "## fetch_1", "42"} {
		if !strings.Contains(md, want) {
			t.Errorf("the report does not contain %q:\n%s", want, md)
		}
	}
	if got["sections"].(float64) != 2 {
		t.Errorf("sections = %v, want 2", got["sections"])
	}
}

// TestReportSectionsAreOrdered. A report whose sections shuffle between runs is
// not a report — and map iteration in Go is deliberately randomised, so this is
// the kind of thing that passes locally and fails in CI once a week.
func TestReportSectionsAreOrdered(t *testing.T) {
	t.Parallel()

	deps := map[string]json.RawMessage{
		"c": json.RawMessage(`{"n":3}`),
		"a": json.RawMessage(`{"n":1}`),
		"b": json.RawMessage(`{"n":2}`),
	}
	first := runReport(t, `{}`, deps)["markdown"].(string)
	for range 10 {
		if got := runReport(t, `{}`, deps)["markdown"].(string); got != first {
			t.Fatalf("two renderings of the same input differ:\n%s\n---\n%s", first, got)
		}
	}
	// Default order is by step id.
	if strings.Index(first, "## a") > strings.Index(first, "## b") {
		t.Errorf("sections are not in id order:\n%s", first)
	}

	// An explicit order is honoured exactly.
	got := runReport(t, `{"include":["c","a"]}`, deps)["markdown"].(string)
	if strings.Index(got, "## c") > strings.Index(got, "## a") {
		t.Errorf("include order was not honoured:\n%s", got)
	}
	if strings.Contains(got, "## b") {
		t.Errorf("include did not narrow the report:\n%s", got)
	}
}

// TestReportNamesAMissingDependencyRatherThanFailing. Four sections of five,
// with the fifth named, is more use than no report — and the plan bug (a step
// in `include` that is not in `depends_on`) is still plainly visible.
func TestReportNamesAMissingDependencyRatherThanFailing(t *testing.T) {
	t.Parallel()

	got := runReport(t, `{"include":["present","absent"]}`, map[string]json.RawMessage{
		"present": json.RawMessage(`{"ok":true}`),
	})

	md := got["markdown"].(string)
	if !strings.Contains(md, "## present") {
		t.Error("the available section was not rendered")
	}
	if !strings.Contains(md, "absent") || !strings.Contains(md, "depends_on") {
		t.Errorf("the report does not say what was missing or why:\n%s", md)
	}
	missing, _ := got["missing"].([]any)
	if len(missing) != 1 || missing[0] != "absent" {
		t.Errorf("missing = %v, want [absent]", missing)
	}
}

// TestReportWritesProse. The console shows a report as plain text, so the
// document has to read as one: an overview in sentences, values as a list, and
// passages as quotes rather than table cells cut off at a column width.
func TestReportWritesProse(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("Passengers rose every summer. ", 8)
	got := runReport(t, `{"title":"Airline passengers"}`, map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"months":144,"trend":"increasing","notes":"` + long + `"}`),
		"fetch":   json.RawMessage(`["1949-01","1949-02"]`),
	})
	md := got["markdown"].(string)

	for _, want := range []string{
		"## Overview",
		"This report covers 2 steps: `analyze` and `fetch`.",
		"`analyze` returned 3 fields.",
		"`fetch` returned a list of 2 items.",
		"- `months`: 144",
		"- `trend`: increasing",
		"### `notes`",
		"> Passengers rose every summer.",
		"- 1949-01",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the report does not contain %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "```") {
		t.Errorf("flat data was rendered as JSON with raw off:\n%s", md)
	}
}

// TestReportAppendsTheFullResultOnRequest. The data a debugger needs is one
// flag away, and off by default because the sections are written for a reader.
func TestReportAppendsTheFullResultOnRequest(t *testing.T) {
	t.Parallel()

	deps := map[string]json.RawMessage{"analyze": json.RawMessage(`{"rows":42}`)}
	if md := runReport(t, `{}`, deps)["markdown"].(string); strings.Contains(md, "```json") {
		t.Errorf("raw defaults to off, but the JSON was appended:\n%s", md)
	}
	md := runReport(t, `{"raw":true}`, deps)["markdown"].(string)
	if !strings.Contains(md, "The full result:") || !strings.Contains(md, `"rows": 42`) {
		t.Errorf("raw:true did not append the full result:\n%s", md)
	}
}

// TestReportKeepsDataInsideItsBlocks. A result is data from somewhere else — in
// the general case a fetched web page, via a step a language model chose to
// add — and a line of it that reads like Markdown must not become the report's
// own structure: no heading of its own, and no fence it can close early.
func TestReportKeepsDataInsideItsBlocks(t *testing.T) {
	t.Parallel()

	got := runReport(t, `{}`, map[string]json.RawMessage{
		"page":   json.RawMessage(`"intro\n## Injected heading\n` + "```" + `\nafter"`),
		"nested": json.RawMessage(`{"rows":[{"note":"` + "```" + `"}]}`),
	})
	md := got["markdown"].(string)

	for _, line := range strings.Split(md, "\n") {
		if line == "## Injected heading" {
			t.Fatalf("a line of data became a heading of the report:\n%s", md)
		}
	}
	if !strings.Contains(md, "> ## Injected heading") {
		t.Errorf("the text was not quoted:\n%s", md)
	}
	if !strings.Contains(md, "````json") {
		t.Errorf("data containing ``` was fenced with a fence it can close:\n%s", md)
	}
}

// TestReportHandlesAResultThatIsNotAnObject. The shape of a tool's result
// belongs to that tool; guessing harder would produce a document that lies
// about nested structure.
func TestReportHandlesAResultThatIsNotAnObject(t *testing.T) {
	t.Parallel()

	got := runReport(t, `{}`, map[string]json.RawMessage{
		"list":   json.RawMessage(`[1,2,3]`),
		"scalar": json.RawMessage(`"just a string"`),
		"nested": json.RawMessage(`{"a":{"b":[1,2]}}`),
	})
	md := got["markdown"].(string)
	for _, want := range []string{"## list", "## scalar", "## nested", "```json"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q:\n%s", want, md)
		}
	}
}

// TestReportValidatesAtSubmitTime.
func TestReportValidatesAtSubmitTime(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		params string
		ok     bool
	}{
		"nothing":       {``, true},
		"a title":       {`{"title":"x"}`, true},
		"an include":    {`{"include":["a","b"]}`, true},
		"raw off":       {`{"raw":false}`, true},
		"duplicate ids": {`{"include":["a","a"]}`, false},
		"an empty id":   {`{"include":[""]}`, false},
		"unknown field": {`{"heading":"x"}`, false},
		"wrong type":    {`{"title":42}`, false},
	} {
		err := tools.Report{}.Validate(json.RawMessage(tc.params))
		if tc.ok && err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted %s", name, tc.params)
		}
	}
}

// TestReportIsRegisteredEverywhere. It needs no image, no network and no
// execution, so there is no deployment in which it should be absent — and a
// planner with no way to present an answer produces plans that end in a fetch.
func TestReportIsRegisteredEverywhere(t *testing.T) {
	t.Parallel()

	for _, opt := range []tools.Options{
		{},
		{EnableTestTools: true},
		{TaskImage: "t", PythonImage: "p"},
	} {
		if !tools.Builtins(opt).Has("report_generate") {
			t.Errorf("report_generate is missing from a registry built with %+v", opt)
		}
	}
}
