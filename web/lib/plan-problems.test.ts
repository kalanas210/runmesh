import { describe, expect, it } from "vitest";
import { anchorDetail, groupByStep, lineOfAnchor, stepLines } from "./plan-problems";

describe("anchorDetail", () => {
  it("reads the index and the member out of the Go field path", () => {
    // Built on the Go side with fmt.Sprintf("steps[%d].%s", i, field), so this
    // is the only shape that needs to parse.
    expect(anchorDetail({ field: "steps[2].depends_on[0]", issue: "unknown step" })).toEqual({
      field: "steps[2].depends_on[0]",
      issue: "unknown step",
      stepIndex: 2,
      member: "depends_on",
    });
  });

  it("anchors a bare step path with no member", () => {
    const anchor = anchorDetail({ field: "steps[0]", issue: "duplicate id" });
    expect(anchor.stepIndex).toBe(0);
    expect(anchor.member).toBeUndefined();
  });

  it("leaves a plan-level path unanchored to any step", () => {
    expect(anchorDetail({ field: "name", issue: "must not be empty" }).stepIndex).toBeUndefined();
  });

  it("does not invent an anchor for a shape it does not understand", () => {
    // A confidently wrong highlight is worse than none: the reader fixes the
    // line the UI pointed at and the API rejects the plan again.
    expect(anchorDetail({ field: "graph.cycle", issue: "cycle" }).stepIndex).toBeUndefined();
  });
});

describe("stepLines", () => {
  it("finds the line each step opens on", () => {
    const source = [
      "{",
      '  "name": "demo",',
      '  "steps": [',
      '    { "id": "a", "tool": "echo" },',
      '    {',
      '      "id": "b",',
      '      "tool": "report_generate"',
      "    }",
      "  ]",
      "}",
    ].join("\n");
    expect(stepLines(source)).toEqual([4, 5]);
  });

  it("is not fooled by a brace inside a string", () => {
    // The entire reason this is a character scanner rather than a regex.
    const source = ['{"steps":[', '{"id":"a","params":{"note":"} ] {"}},', '{"id":"b"}', "]}"].join(
      "\n",
    );
    expect(stepLines(source)).toEqual([2, 3]);
  });

  it("is not fooled by an escaped quote inside a string", () => {
    const source = ['{"steps":[', '{"id":"a","params":{"note":"say \\" then }"}},', '{"id":"b"}', "]}"].join(
      "\n",
    );
    expect(stepLines(source)).toEqual([2, 3]);
  });

  it("ignores a nested steps key that is not the plan's", () => {
    const source = ['{"steps":[', '{"id":"a","params":{"steps":[{"x":1}]}}', "]}"].join("\n");
    expect(stepLines(source)).toEqual([2]);
  });

  it("does not treat depends_on elements as steps", () => {
    const source = ['{"steps":[', '{"id":"b","depends_on":["a"]}', "]}"].join("\n");
    expect(stepLines(source)).toEqual([2]);
  });

  it("returns what it found for a half-typed document rather than nothing", () => {
    // Mid-edit is exactly when the reader most wants the problem list to point
    // somewhere, so an unbalanced document must still yield anchors.
    const source = ['{"steps":[', '{"id":"a"},', '{"id":"b"'].join("\n");
    expect(stepLines(source)).toEqual([2, 3]);
  });

  it("finds nothing in a document with no steps array", () => {
    expect(stepLines('{"name":"demo"}')).toEqual([]);
    expect(stepLines("")).toEqual([]);
  });

  it("does not arm on a steps key whose value is not an array", () => {
    const source = ['{"steps":"none","other":[', "{}", "]}"].join("\n");
    expect(stepLines(source)).toEqual([]);
  });
});

describe("lineOfAnchor", () => {
  const source = ['{"steps":[', '{"id":"a"},', '{"id":"b"}', "]}"].join("\n");

  it("locates a step", () => {
    expect(lineOfAnchor({ field: "steps[1].tool", issue: "x", stepIndex: 1 }, source)).toBe(3);
  });

  it("sends a plan-level problem to the top of the document", () => {
    expect(lineOfAnchor({ field: "name", issue: "x" }, source)).toBe(1);
  });

  it("returns null for a step index the text does not contain", () => {
    expect(lineOfAnchor({ field: "steps[9]", issue: "x", stepIndex: 9 }, source)).toBeNull();
  });
});

describe("groupByStep", () => {
  it("puts plan-level problems first, then steps in order", () => {
    const grouped = groupByStep([
      { field: "steps[2].tool", issue: "b" },
      { field: "steps[2].tool", issue: "b", stepIndex: 2 },
      { field: "steps[0].id", issue: "a", stepIndex: 0 },
      { field: "name", issue: "empty" },
    ]);
    expect(grouped.map((g) => g.stepIndex)).toEqual([null, 0, 2]);
    expect(grouped[0].problems).toHaveLength(2);
  });
});
