package k8s

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

func epochForTest() time.Time {
	return time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
}

// Reading a result out of stdout is the weakest link in Week 3, so it gets the
// most tests. Every case here is a way a real container misbehaves.
func TestParseResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		logs    string
		want    string
		wantErr string // runmesh error code, "" for none
	}{
		{
			name: "a clean result after ordinary logging",
			logs: "starting\nreading sales.csv\n" + ResultMarker + ` {"rows":128}` + "\ndone\n",
			want: `{"rows":128}`,
		},
		{
			name: "no marker at all is not an error",
			logs: "just some logs\nand more\n",
			want: "",
		},
		{
			name: "empty logs",
			logs: "",
			want: "",
		},
		{
			// A step may legitimately do work and produce nothing. Output.Result
			// is documented as optional, and inventing a failure here would fail
			// every side-effecting tool.
			name: "a marker with nothing after it",
			logs: ResultMarker + "\n",
			want: "",
		},
		{
			// The rule is one sentence — last wins — so a tool that happens to
			// log the marker before emitting its real result is still correct.
			name: "the last marker wins",
			logs: ResultMarker + ` {"n":1}` + "\nmore work\n" + ResultMarker + ` {"n":2}` + "\n",
			want: `{"n":2}`,
		},
		{
			name: "a marker part-way along a line",
			logs: "2026-09-07 INFO " + ResultMarker + ` {"ok":true}` + "\n",
			want: `{"ok":true}`,
		},
		{
			name: "a JSON array is a valid result",
			logs: ResultMarker + ` [1,2,3]` + "\n",
			want: `[1,2,3]`,
		},
		{
			name: "no trailing newline",
			logs: ResultMarker + ` {"ok":true}`,
			want: `{"ok":true}`,
		},
		{
			// A contract violation, not an empty result: the difference tells
			// the tool author they have a bug rather than leaving them to
			// wonder where their data went.
			name:    "a marker with invalid JSON",
			logs:    ResultMarker + " not json at all\n",
			wantErr: runmesh.CodeContractBroken,
		},
		{
			name:    "windows line endings",
			logs:    "log\r\n" + ResultMarker + ` {"ok":true}` + "\r\n",
			want:    `{"ok":true}`,
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseResult(strings.NewReader(tc.logs), 1<<20)

			if tc.wantErr != "" {
				var te *runmesh.ToolError
				if !errors.As(err, &te) {
					t.Fatalf("error = %v, want a classified *runmesh.ToolError", err)
				}
				if te.Code != tc.wantErr {
					t.Fatalf("error code = %q, want %q", te.Code, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseResult: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseResultDistinguishesTruncationFromAbsence is the case worth being
// careful about. A chatty container that buried its marker past the cap must
// NOT look identical to one that never emitted a result: the first is a
// misconfigured limit and the second is a tool that produced nothing, and only
// one of those is the tool author's problem.
func TestParseResultDistinguishesTruncationFromAbsence(t *testing.T) {
	t.Parallel()

	noisy := strings.Repeat("a chatty container line\n", 2000)
	logs := noisy + ResultMarker + ` {"ok":true}` + "\n"

	_, err := ParseResult(strings.NewReader(logs), 256)
	var te *runmesh.ToolError
	if !errors.As(err, &te) || te.Code != runmesh.CodeOutputTooLarge {
		t.Fatalf("error = %v, want %s: exceeding the cap must be reported as "+
			"itself, not as a missing result", err, runmesh.CodeOutputTooLarge)
	}

	// The same logs under a cap that fits parse cleanly.
	got, err := ParseResult(strings.NewReader(logs), 1<<20)
	if err != nil {
		t.Fatalf("ParseResult with a sufficient cap: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("result = %q", got)
	}
}

// TestParseResultRejectsAnOversizedLine: bufio's default 64 KiB token limit
// would otherwise turn a large-but-valid result into a truncated-line error a
// long way from its cause, so the scanner's buffer is raised to the cap and
// the cap itself is what reports.
func TestParseResultRejectsAnOversizedLine(t *testing.T) {
	t.Parallel()

	huge := ResultMarker + ` {"blob":"` + strings.Repeat("x", 200_000) + `"}`
	_, err := ParseResult(strings.NewReader(huge), 1024)

	var te *runmesh.ToolError
	if !errors.As(err, &te) || te.Code != runmesh.CodeOutputTooLarge {
		t.Fatalf("error = %v, want %s", err, runmesh.CodeOutputTooLarge)
	}
}

// TestParseResultAcceptsALargeResultUnderTheCap pins the other half: raising
// the scanner buffer to the cap is what makes a legitimately large result work
// rather than tripping bufio's 64 KiB default.
func TestParseResultAcceptsALargeResultUnderTheCap(t *testing.T) {
	t.Parallel()

	blob := strings.Repeat("x", 100_000)
	line := ResultMarker + ` {"blob":"` + blob + `"}`

	got, err := ParseResult(strings.NewReader(line), 1<<20)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if len(got) < 100_000 {
		t.Fatalf("result is %d bytes, want the whole %d-byte payload", len(got), len(blob))
	}
}

// TestFormatResultRoundTrips: the images in this repository produce their
// result lines with FormatResult, so the producer and the parser cannot
// disagree about the format.
func TestFormatResultRoundTrips(t *testing.T) {
	t.Parallel()

	line, err := FormatResult(map[string]any{"rows": 128, "ok": true})
	if err != nil {
		t.Fatalf("FormatResult: %v", err)
	}
	got, err := ParseResult(strings.NewReader("noise\n"+line+"\n"), 1<<20)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if !strings.Contains(string(got), `"rows":128`) {
		t.Fatalf("round trip lost the payload: %q", got)
	}
}
