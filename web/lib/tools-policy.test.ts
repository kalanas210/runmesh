import { describe, expect, it } from "vitest";
import {
  declaredContract,
  formatBytes,
  grantedPolicy,
  orderTools,
  sandboxCaveat,
  schemaFields,
} from "./tools-policy";
import type { ToolDescriptor } from "./types";

function tool(overrides: Partial<ToolDescriptor> = {}): ToolDescriptor {
  return {
    name: "echo",
    version: "1.0.0",
    description: "Return the input verbatim.",
    input_schema: {},
    limits: { max_attempts: 3, max_output_bytes: 1 << 20, network: false },
    execution: "in_process",
    ...overrides,
  };
}

describe("the asks-versus-grants split", () => {
  it("puts every sandbox dimension on the policy side", () => {
    // Effective overwrites CPU, Memory, EphemeralStorage, Network and Image
    // unconditionally, so none of them is ever the tool's own statement.
    const labels = grantedPolicy(tool()).map((row) => row.label);
    expect(labels).toEqual(
      expect.arrayContaining(["cpu", "memory", "ephemeral storage", "image", "network", "execution"]),
    );
    for (const row of grantedPolicy(tool())) expect(row.origin).toBe("policy");
  });

  it("marks max attempts as clamped rather than as either side's alone", () => {
    // The number is the tool author's idempotency claim; policy only lowers it.
    const row = declaredContract(tool()).find((r) => r.label === "max attempts");
    expect(row?.origin).toBe("clamped");
  });

  it("renders network as a word, not a boolean", () => {
    const denied = grantedPolicy(tool()).find((r) => r.label === "network");
    expect(denied?.value).toBe("denied");
    const allowed = grantedPolicy(tool({ limits: { ...tool().limits, network: true } })).find(
      (r) => r.label === "network",
    );
    expect(allowed?.value).toBe("allowed");
  });
});

describe("sandboxCaveat", () => {
  it("warns that in-process limits are not enforced by anything", () => {
    // The one security-shaped claim this screen could get wrong.
    expect(sandboxCaveat(tool({ execution: "in_process" }))).toContain("in process");
  });

  it("is silent when the executor really is a sandbox", () => {
    expect(sandboxCaveat(tool({ execution: "container" }))).toBeNull();
  });
});

describe("formatBytes", () => {
  it("uses binary units, because the configured values are powers of two", () => {
    expect(formatBytes(1 << 20)).toBe("1 MiB");
    expect(formatBytes(64 * 1024)).toBe("64 KiB");
    expect(formatBytes(512)).toBe("512 B");
  });

  it("renders an absent or nonsensical limit as the house placeholder", () => {
    expect(formatBytes(0)).toBe("—");
    expect(formatBytes(Number.NaN)).toBe("—");
  });
});

describe("schemaFields", () => {
  it("reads properties, types, requiredness, descriptions and defaults", () => {
    const fields = schemaFields({
      type: "object",
      required: ["text"],
      properties: {
        text: { type: "string", description: "What to echo." },
        times: { type: "integer", default: 1 },
      },
    });
    expect(fields).toEqual([
      { name: "text", type: "string", required: true, description: "What to echo.", defaultValue: undefined },
      { name: "times", type: "integer", required: false, description: undefined, defaultValue: "1" },
    ]);
  });

  it("joins a union type", () => {
    const fields = schemaFields({ properties: { v: { type: ["string", "null"] } } });
    expect(fields[0].type).toBe("string | null");
  });

  it("falls back to any rather than dropping an untyped property", () => {
    expect(schemaFields({ properties: { v: {} } })[0].type).toBe("any");
  });

  it("returns nothing rather than throwing on a schema it cannot read", () => {
    // input_schema is json.RawMessage passed through untouched. A card that
    // threw on a $ref would hide the tool, which is the same failure as
    // omitting a denied one.
    expect(schemaFields(undefined)).toEqual([]);
    expect(schemaFields(null)).toEqual([]);
    expect(schemaFields("a string")).toEqual([]);
    expect(schemaFields({ $ref: "#/defs/x" })).toEqual([]);
    expect(schemaFields({ properties: "not an object" })).toEqual([]);
  });
});

describe("orderTools", () => {
  it("lists denied tools last but never drops them", () => {
    const ordered = orderTools([
      tool({ name: "zebra" }),
      tool({ name: "banned", denied: true, denied_reason: "denied by policy" }),
      tool({ name: "alpha" }),
    ]);
    expect(ordered.map((t) => t.name)).toEqual(["alpha", "zebra", "banned"]);
  });
});
