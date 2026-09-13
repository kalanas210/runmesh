import { describe, expect, it } from "vitest";
import { viewResult } from "./result";

describe("viewResult", () => {
  it("detects a report by its markdown member, not by the tool's name", () => {
    // Tool names are configuration — cmd/task implements the same renderer for
    // the container path — so the shape of the data is the more reliable key.
    const view = viewResult({ markdown: "# Report\n", bytes: 9, sections: 1 });
    expect(view.kind).toBe("markdown");
    if (view.kind === "markdown") {
      expect(view.text).toBe("# Report\n");
      expect(view.bytes).toBe(9);
    }
  });

  it("returns the markdown as TEXT, never as anything a renderer could interpret", () => {
    // report.go states that its escaping is formatting hygiene and not a
    // security control, and the content may have originated from a fetched web
    // page. This assertion exists so that a later change to render it as HTML
    // has to delete a test that says why not.
    const hostile = '<img src=x onerror="fetch(`/api/rm/jobs`)">';
    const view = viewResult({ markdown: hostile });
    expect(view.kind).toBe("markdown");
    if (view.kind === "markdown") expect(view.text).toBe(hostile);
  });

  it("treats a missing result as empty rather than as the string null", () => {
    expect(viewResult(null).kind).toBe("empty");
    expect(viewResult(undefined).kind).toBe("empty");
  });

  it("pretty-prints anything else", () => {
    const view = viewResult({ echo: { message: "hi" }, attempt: 1 });
    expect(view.kind).toBe("json");
    if (view.kind === "json") expect(view.text).toContain('"message": "hi"');
  });

  it("does not mistake an array whose first member has a markdown key", () => {
    const view = viewResult([{ markdown: "x" }]);
    expect(view.kind).toBe("json");
  });

  it("does not mistake a non-string markdown member for a report", () => {
    const view = viewResult({ markdown: { nested: true } });
    expect(view.kind).toBe("json");
  });

  it("survives a circular structure instead of taking the screen down with it", () => {
    // `result` is typed unknown precisely because nothing validates it, and an
    // inspector panel that throws takes the whole job screen with it.
    const circular: Record<string, unknown> = { name: "loop" };
    circular.self = circular;
    expect(() => viewResult(circular)).not.toThrow();
    expect(viewResult(circular).kind).toBe("json");
  });
});
