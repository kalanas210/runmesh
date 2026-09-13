import type { ToolDescriptor } from "./types";

/**
 * Splitting a tool descriptor into the two halves an operator actually asks
 * about: what the tool CLAIMS about itself, and what this deployment will
 * actually let it do.
 *
 * ---------------------------------------------------------------------------
 * THE FACT THIS MODULE EXISTS TO MAKE VISIBLE.
 *
 * `GET /api/v1/tools` returns `policy.Engine.Effective(d)` — the GRANT, not the
 * request. Effective overwrites `Execution` with the deployment's mode and
 * overwrites CPU, Memory, EphemeralStorage, Network and Image with the resolved
 * values (policy.go:283-296), caps MaxOutputBytes when the policy sets one, and
 * lowers MaxAttempts to the policy ceiling when the tool asked for more. The
 * clamp is SILENT by design: a tool asking for more CPU than it may have is a
 * portable descriptor meeting a smaller cluster, and the right answer is to run
 * it smaller rather than to refuse the plan — TestEffectivePublishesTheGrantNot
 * TheRequest asserts precisely that.
 *
 * The consequence for this screen is uncomfortable and has to be stated rather
 * than designed around: THE ASK IS NOT ON THE WIRE. There is no field carrying
 * what the tool requested, so a catalogue cannot render "asked 8 CPU, got 1".
 * The honest UI therefore does the next best thing — it separates the fields
 * that are the tool's own from the fields policy resolved, labels the second
 * group as resolved, and says plainly that the request itself is not
 * observable. The dishonest alternative, and the obvious one, is to print the
 * limits as a single flat table titled "limits", which reads as the tool's
 * declaration and is the exact misunderstanding this endpoint invites.
 * ---------------------------------------------------------------------------
 *
 * No React and no JSX here: these are the rows, and the card decides how a row
 * looks. That keeps the "which half does this field belong to" decision in one
 * place with a test on it, instead of in the markup where it would be re-made
 * by hand for every field added later.
 */

/** Who decided this value. */
export type Origin =
  | "tool" /** The tool declared it and policy does not touch it. */
  | "policy" /** Policy resolved or overwrote it, always. */
  | "clamped"; /** The tool declares it and policy lowers it to a ceiling. */

export interface ContractRow {
  label: string;
  value: string;
  origin: Origin;
  /** One sentence, shown on the row. Absent when the label is self-evident. */
  note?: string;
}

/** The house placeholder for a value the API did not send. */
const ABSENT = "—";

/**
 * Bytes as a round quantity: 1 MiB, 64 KiB.
 *
 * Binary units, not decimal, because the Go side's limit is a byte count
 * compared with `len(b)` and the numbers configured for it are powers of two.
 * Printing 1.05 MB for 1 MiB would make a configured value look like an odd
 * one.
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return ABSENT;
  const units = ["B", "KiB", "MiB", "GiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const rounded = value >= 100 || Number.isInteger(value) ? Math.round(value) : Number(value.toFixed(1));
  return `${rounded} ${units[unit]}`;
}

/**
 * The half of the descriptor that belongs to the tool: its identity and the
 * shape of its input.
 *
 * `max_attempts` lives here rather than in the grant half even though policy
 * can lower it, because the number is the tool author's statement about its own
 * idempotency — "I am safe to run four times" — and policy only ever caps it.
 * Marking it `clamped` says both things at once.
 */
export function declaredContract(descriptor: ToolDescriptor): ContractRow[] {
  return [
    { label: "version", value: descriptor.version || ABSENT, origin: "tool" },
    {
      label: "max attempts",
      value: descriptor.limits.max_attempts > 0 ? String(descriptor.limits.max_attempts) : ABSENT,
      origin: "clamped",
      note: "the tool's own retry ceiling, lowered if the deployment sets a smaller one",
    },
  ];
}

/**
 * The half this deployment decided. Every row here is a policy answer, and the
 * card labels them as such.
 *
 * Network is rendered as two words rather than a boolean, and it is the row
 * that matters most: `network: false` is the difference between a tool that can
 * reach the internet from inside the sandbox and one that cannot, and a reader
 * scanning for "which of these can phone out" should not have to decode `false`.
 */
export function grantedPolicy(descriptor: ToolDescriptor): ContractRow[] {
  const limits = descriptor.limits;
  const rows: ContractRow[] = [
    {
      label: "execution",
      value: descriptor.execution === "container" ? "container" : "in process",
      origin: "policy",
      note:
        descriptor.execution === "container"
          ? "runs in its own sandbox, under the limits below"
          : "runs inside the server process, so the resource limits below are not enforced by a sandbox",
    },
    {
      label: "network",
      value: limits.network ? "allowed" : "denied",
      origin: "policy",
    },
    { label: "cpu", value: limits.cpu || ABSENT, origin: "policy" },
    { label: "memory", value: limits.memory || ABSENT, origin: "policy" },
    {
      label: "ephemeral storage",
      value: limits.ephemeral_storage || ABSENT,
      origin: "policy",
    },
    { label: "image", value: limits.image || ABSENT, origin: "policy" },
    {
      label: "max output",
      value: formatBytes(limits.max_output_bytes),
      origin: "policy",
      note: "output past this is not truncated — the step fails with output_too_large",
    },
  ];

  return rows;
}

/**
 * The in-process caveat, as a sentence, or null when it does not apply.
 *
 * An in-process tool's descriptor still carries cpu, memory and image, because
 * `Effective` fills them in from the policy regardless of the mode. Printing
 * them without this note would tell a reader their sandbox is enforcing limits
 * that nothing is enforcing — the single most misleading thing this screen
 * could do, because it is a security-shaped claim.
 */
export function sandboxCaveat(descriptor: ToolDescriptor): string | null {
  if (descriptor.execution === "container") return null;
  return (
    "This deployment runs tools in process, so the resource limits above are the " +
    "policy's resolved values rather than anything a sandbox is enforcing. They " +
    "become real when the executor is switched to containers."
  );
}

/* -------------------------------------------------------------------------- */
/* The input schema.                                                          */
/* -------------------------------------------------------------------------- */

export interface SchemaField {
  name: string;
  /** The JSON Schema `type`, or "any" when the schema does not say. */
  type: string;
  required: boolean;
  description?: string;
  /** `default`, rendered. Absent when the schema declares none. */
  defaultValue?: string;
}

/**
 * Read the top-level properties out of a JSON Schema, defensively.
 *
 * `input_schema` is `json.RawMessage` passed through untouched, so it is
 * genuinely unknown: a tool may ship a schema this reader has never seen, or a
 * `$ref`, or nothing at all. Every access below is guarded and the function
 * returns an empty array rather than throwing, because the catalogue's job is
 * to list the tool even when its schema is unreadable — a card that failed to
 * render because of a `$ref` would hide the tool entirely, which is the same
 * failure as omitting a denied tool.
 *
 * Nested object properties are deliberately NOT flattened. A card is a summary;
 * the full schema is one click away in the raw JSON, and a recursive flattener
 * would turn a deeply nested tool into thirty rows of dotted paths that nobody
 * reads.
 */
export function schemaFields(schema: unknown): SchemaField[] {
  if (!schema || typeof schema !== "object") return [];
  const root = schema as Record<string, unknown>;

  const properties = root.properties;
  if (!properties || typeof properties !== "object") return [];

  const required = new Set(
    Array.isArray(root.required) ? root.required.filter((v): v is string => typeof v === "string") : [],
  );

  return Object.entries(properties as Record<string, unknown>).map(([name, raw]) => {
    const property = raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
    const type = property.type;

    return {
      name,
      type:
        typeof type === "string"
          ? type
          : Array.isArray(type)
            ? type.filter((v): v is string => typeof v === "string").join(" | ") || "any"
            : "any",
      required: required.has(name),
      description: typeof property.description === "string" ? property.description : undefined,
      defaultValue:
        property.default === undefined ? undefined : JSON.stringify(property.default),
    };
  });
}

/**
 * Sort the catalogue: usable tools first, denied ones last, alphabetically
 * within each group.
 *
 * Denied tools are LISTED, never hidden — omitting them makes `unknown_tool` at
 * submission time the only evidence that a tool exists but is switched off, and
 * TestEffectiveListsDeniedToolsWithAReason exists on the Go side for exactly
 * this reason. Sorting them to the bottom is the compromise: present, findable,
 * and not in the way of the tools a plan can actually call.
 */
export function orderTools(tools: readonly ToolDescriptor[]): ToolDescriptor[] {
  return [...tools].sort((a, b) => {
    const denied = Number(!!a.denied) - Number(!!b.denied);
    return denied !== 0 ? denied : a.name.localeCompare(b.name);
  });
}
