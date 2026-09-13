"use client";

import { useState } from "react";
import { cn } from "@/lib/cn";
import {
  declaredContract,
  grantedPolicy,
  sandboxCaveat,
  schemaFields,
  type ContractRow,
} from "@/lib/tools-policy";
import type { ToolDescriptor } from "@/lib/types";
import { Code } from "@/components/ui/code";
import { Notice } from "@/components/ui/notice";

/**
 * One tool: what it says about itself, and what this deployment will actually
 * let it do.
 *
 * ---------------------------------------------------------------------------
 * THE ASK-VERSUS-GRANT DISTINCTION IS THE POINT OF THIS SCREEN, AND THE ASK IS
 * NOT ON THE WIRE.
 *
 * `GET /api/v1/tools` returns `policy.Engine.Effective(d)` — the GRANT.
 * Effective overwrites Execution, CPU, Memory, EphemeralStorage, Network and
 * Image with the deployment's resolved values, caps MaxOutputBytes, and lowers
 * MaxAttempts to the policy ceiling when the tool asked for more. The clamp is
 * silent by design: a portable tool descriptor meeting a smaller cluster should
 * run smaller rather than have its plan refused.
 *
 * So a catalogue CANNOT print "asked 8 CPU, got 1" — the request is not
 * observable from here, and any UI that implied otherwise would be inventing
 * the left-hand column. What it can do, and what this card does, is separate
 * the fields that are the tool's own from the fields policy decided, label the
 * second group as resolved, and say plainly that the request itself is not on
 * the wire. The tempting alternative — one flat table headed "limits" — reads
 * as the tool's declaration, which is exactly the misunderstanding the endpoint
 * invites.
 *
 * A DENIED TOOL IS SHOWN, NEVER HIDDEN. Omitting it makes `unknown_tool` at
 * submission time the only evidence that a tool exists and is switched off,
 * which sends the reader looking for a typo in a name that is spelled
 * correctly.
 * ---------------------------------------------------------------------------
 */
export function ToolCard({ descriptor }: { descriptor: ToolDescriptor }) {
  const [showSchema, setShowSchema] = useState(false);

  const denied = !!descriptor.denied;
  const caveat = sandboxCaveat(descriptor);
  const fields = schemaFields(descriptor.input_schema);

  return (
    <article
      className={cn(
        "rounded-2xl border border-line bg-ink-2 p-6",
        // Greyed, not hidden, and not removed from the tab order.
        denied && "opacity-60",
      )}
    >
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h2 className="tnum text-[1rem] text-bone">{descriptor.name}</h2>
        <span className="tnum text-[0.65rem] text-faint">v{descriptor.version}</span>
        <span
          className={cn(
            "ml-auto rounded-full border px-2.5 py-0.5 font-mono text-[0.6rem] uppercase tracking-[0.14em]",
            denied
              ? "border-[#ff6b6b]/45 text-[#ff6b6b]"
              : descriptor.execution === "container"
                ? "border-st-succeeded/45 text-st-succeeded"
                : "border-line-2 text-muted",
          )}
        >
          {denied ? "denied" : descriptor.execution === "container" ? "sandboxed" : "in process"}
        </span>
      </header>

      <p className="mt-2 text-[0.8rem] leading-relaxed text-muted">{descriptor.description}</p>

      {denied && (
        <Notice tone="error" className="mt-4" title="This deployment refuses this tool">
          {descriptor.denied_reason ??
            "The execution policy refuses it and gave no reason."}{" "}
          A plan naming it is rejected at submission time with tool_denied, not
          at execution time.
        </Notice>
      )}

      <div className="mt-5 grid gap-5 sm:grid-cols-2">
        <Half
          title="declared by the tool"
          hint="the tool's own statement about itself"
          rows={declaredContract(descriptor)}
        />
        <Half
          title="granted by this deployment"
          hint="resolved by the execution policy — not what the tool requested"
          rows={grantedPolicy(descriptor)}
        />
      </div>

      <p className="mt-4 text-[0.65rem] leading-snug text-faint">
        The catalogue publishes the grant, not the request. A tool that asked for
        more than it was given is indistinguishable here from one that asked for
        exactly this — the API carries no field for the ask.
      </p>

      {caveat && (
        <Notice tone="warn" className="mt-4">
          {caveat}
        </Notice>
      )}

      {fields.length > 0 && (
        <section className="mt-5">
          <h3 className="kicker text-[0.5625rem]">input</h3>
          <ul className="mt-2 space-y-1.5">
            {fields.map((field) => (
              <li key={field.name} className="text-[0.72rem] leading-snug">
                <span className="tnum text-bone-2">{field.name}</span>
                <span className="ml-2 text-faint">{field.type}</span>
                {field.required && (
                  <span className="ml-2 text-st-retrying">required</span>
                )}
                {field.defaultValue !== undefined && (
                  <span className="tnum ml-2 text-faint">default {field.defaultValue}</span>
                )}
                {field.description && (
                  <p className="mt-0.5 text-[0.68rem] text-faint">{field.description}</p>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}

      <div className="mt-5 border-t border-line pt-3">
        <button
          type="button"
          onClick={() => setShowSchema((open) => !open)}
          aria-expanded={showSchema}
          className="text-[0.68rem] text-muted underline-offset-4 hover:text-bone hover:underline"
        >
          {showSchema ? "Hide" : "Show"} the raw input schema
        </button>
        {showSchema && (
          <Code
            className="mt-3"
            maxHeight={240}
            label={`JSON Schema for ${descriptor.name}`}
            // Passed through untouched by the Go side, so it is genuinely
            // unknown here: a tool may ship a $ref, or a schema nothing in this
            // console has ever seen. Printing it verbatim is the only rendering
            // that cannot be wrong.
            value={JSON.stringify(descriptor.input_schema ?? null, null, 2)}
          />
        )}
      </div>
    </article>
  );
}

function Half({
  title,
  hint,
  rows,
}: {
  title: string;
  hint: string;
  rows: readonly ContractRow[];
}) {
  return (
    <section className="rounded-xl border border-line bg-ink px-4 py-3.5">
      <h3 className="kicker text-[0.5rem]">{title}</h3>
      <p className="mt-1 text-[0.62rem] leading-snug text-faint">{hint}</p>

      <dl className="mt-3 space-y-2.5">
        {rows.map((row) => (
          <div key={row.label}>
            <div className="flex items-baseline justify-between gap-3">
              <dt className="text-[0.7rem] text-muted">{row.label}</dt>
              <dd className="tnum text-right text-[0.72rem] text-bone-2">
                {row.value}
                {/* `clamped` is its own word because it means both things at
                    once: the tool declared this number AND policy may have
                    lowered it. Calling it either "tool" or "policy" alone would
                    be half right. */}
                {row.origin === "clamped" && (
                  <span className="ml-2 text-[0.55rem] uppercase tracking-[0.14em] text-faint">
                    capped
                  </span>
                )}
              </dd>
            </div>
            {row.note && (
              <p className="mt-0.5 text-[0.62rem] leading-snug text-faint">{row.note}</p>
            )}
          </div>
        ))}
      </dl>
    </section>
  );
}
