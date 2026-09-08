package k8s

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// ResultMarker is the sentinel a containerised tool prefixes to the single
// line carrying its result.
//
//	some ordinary log output
//	##RUNMESH-RESULT## {"rows":128,"ok":true}
//
// Reading a result out of stdout is the weakest part of Week 3 and it is
// chosen with that understood. The plan's own table says stdout is not a data
// channel, and it is right. The two alternatives cost more than they are worth
// today: a callback to the RunMesh API needs egress from a sandbox whose whole
// point is not having any, plus a per-attempt credential to authenticate it;
// object storage needs object storage.
//
// So: a sentinel, a size cap, and an explicit contract — and the mitigations
// are real rather than hopeful. The marker is unmistakable, the parse failure
// is a classified contract violation rather than a silent empty result, and
// exceeding the cap is reported as itself rather than as "no result found".
// The callback lands when the tool contract stops moving, which is Week 4.
const ResultMarker = "##RUNMESH-RESULT##"

// ParseResult extracts the result from a container's logs.
//
// It returns the LAST marker line, not the first. A tool that logs a line
// happening to contain the marker before emitting its real result is then
// still correct, and the rule is one sentence rather than a special case. The
// cost is that a tool cannot emit two results and expect the first to win,
// which is not a thing any tool should want to do.
//
// A missing marker is NOT an error. A container that exits 0 without producing
// a result is a legitimate side-effecting step, and Output.Result is
// documented as optional.
//
// maxBytes caps the whole stream. Hitting that cap is reported as itself: a
// chatty container that buried its marker past the limit must not look
// identical to a container that never emitted one, because the fixes are
// different and only one of them is the tool author's.
func ParseResult(r io.Reader, maxBytes int) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	// One byte past the cap, so hitting it is distinguishable from a stream
	// that happened to end exactly on it.
	limited := &countingReader{r: io.LimitReader(r, int64(maxBytes)+1)}
	sc := bufio.NewScanner(limited)
	// A result line may legitimately be the whole budget. The default 64 KiB
	// token limit would otherwise turn a large-but-valid result into a
	// truncated-line error a long way from its cause.
	sc.Buffer(make([]byte, 0, 64<<10), maxBytes+1)

	var last string
	var found bool
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, ResultMarker); i >= 0 {
			last = strings.TrimSpace(line[i+len(ResultMarker):])
			found = true
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, runmesh.Fatal(runmesh.CodeOutputTooLarge,
				"a single log line exceeded the %d byte limit", maxBytes)
		}
		return nil, runmesh.Retry(runmesh.CodeUnclassified, "reading container logs: %v", err)
	}
	if limited.n > int64(maxBytes) {
		return nil, runmesh.Fatal(runmesh.CodeOutputTooLarge,
			"container logs exceeded %d bytes before a %s line was found",
			maxBytes, ResultMarker)
	}

	if !found || last == "" {
		return nil, nil
	}
	if !json.Valid([]byte(last)) {
		// Deliberately does not echo the payload. It is unvalidated container
		// output on its way into a log line and a JSON error envelope.
		return nil, runmesh.Fatal(runmesh.CodeContractBroken,
			"the %s line is not valid JSON (%d bytes)", ResultMarker, len(last))
	}
	return json.RawMessage(last), nil
}

// countingReader records how much was actually consumed, so the caller can
// tell "the stream ended" from "the limit was reached".
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// FormatResult renders a result line, and exists so the tool images shipped in
// this repository cannot disagree with the parser about the format.
func FormatResult(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("k8s: encoding a result line: %w", err)
	}
	return ResultMarker + " " + string(b), nil
}
