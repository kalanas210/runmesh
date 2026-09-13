"use client";

import { useState } from "react";
import type { ReactNode } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { Button } from "@/components/ui/button";
import { Card, surface } from "@/components/ui/card";
import { Kicker } from "@/components/ui/kicker";
import { StatePill } from "@/components/ui/state-pill";
import { StateGlyph } from "@/components/ui/state-glyph";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorState } from "@/components/ui/error-state";
import { Skeleton, SkeletonLines } from "@/components/ui/skeleton";
import { Freshness } from "@/components/ui/freshness";
import { ApiError } from "@/lib/api";
import { JOB_STATES, STEP_STATES } from "@/lib/state";
import * as Icons from "@/components/ui/icons";

/**
 * The primitives gallery.
 *
 * It exists so the agents building the waterfall and the screens can see what
 * already exists rather than rebuilding it, and so a token that silently
 * compiled to nothing is visible immediately. That second job is why every
 * swatch below is painted with a real Tailwind utility written as a LITERAL
 * class string rather than an inline style built from a variable: Tailwind v4
 * scans source text, so a class assembled at runtime is a class that does not
 * exist, and an unstyled swatch here is the cheapest possible way to discover
 * a missing `@theme inline` entry.
 *
 * This route is not linked from the primary nav on purpose. It is a workshop
 * surface, not a product screen.
 */

function Section({
  title,
  note,
  children,
}: {
  title: string;
  note?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="mt-14">
      <div className="border-t border-line pt-5">
        <Kicker as="h2">{title}</Kicker>
        {note && (
          <p className="mt-3 max-w-3xl text-[0.82rem] leading-relaxed text-muted">
            {note}
          </p>
        )}
      </div>
      <div className="mt-6">{children}</div>
    </section>
  );
}

/** Literal class strings. See the note at the top of the file. */
const STATE_SWATCHES = [
  { state: "QUEUED", hex: "#9a958a", ratio: "6.60:1", bg: "bg-st-queued", fill: "bg-st-queued-fill", text: "text-st-queued" },
  { state: "SCHEDULED", hex: "#c9c5bb", ratio: "11.42:1", bg: "bg-st-scheduled", fill: "bg-st-scheduled-fill", text: "text-st-scheduled" },
  { state: "RUNNING", hex: "#f4f2ec", ratio: "17.57:1", bg: "bg-st-running", fill: "bg-st-running-fill", text: "text-st-running" },
  { state: "RETRYING", hex: "#eda23a", ratio: "9.21:1", bg: "bg-st-retrying", fill: "bg-st-retrying-fill", text: "text-st-retrying" },
  { state: "SUCCEEDED", hex: "#5cefae", ratio: "13.36:1", bg: "bg-st-succeeded", fill: "bg-st-succeeded-fill", text: "text-st-succeeded" },
  { state: "FAILED", hex: "#ff5c5c", ratio: "6.50:1", bg: "bg-st-failed", fill: "bg-st-failed-fill", text: "text-st-failed" },
  { state: "TIMED_OUT", hex: "#e48ada", ratio: "8.38:1", bg: "bg-st-timedout", fill: "bg-st-timedout-fill", text: "text-st-timedout" },
  { state: "CANCELLED", hex: "#7e8288", ratio: "5.09:1", bg: "bg-st-cancelled", fill: "bg-st-cancelled-fill", text: "text-st-cancelled" },
] as const;

const CANVAS_SWATCHES = [
  { name: "--ink", hex: "#0b0b0c", cls: "bg-ink" },
  { name: "--ink-2", hex: "#101012", cls: "bg-ink-2" },
  { name: "--ink-3", hex: "#17171a", cls: "bg-ink-3" },
  { name: "--ink-4", hex: "#1f1f23", cls: "bg-ink-4" },
  { name: "--bone", hex: "#f4f2ec", cls: "bg-bone" },
  { name: "--bone-2", hex: "#e7e4da", cls: "bg-bone-2" },
  { name: "--muted", hex: "#9a958a", cls: "bg-muted" },
  { name: "--faint", hex: "#8a857b", cls: "bg-faint" },
  { name: "--accent", hex: "#3aa0ff", cls: "bg-accent" },
] as const;

export function Gallery() {
  // Three ages, frozen relative to mount, so the chip's three appearances are
  // all on screen at once instead of one at a time over a minute.
  const [mounted] = useState(() => Date.now());

  const notFound = new ApiError(
    404,
    "job not found",
    { error: { code: "not_found", message: "job not found", request_id: "req_2f8c1a" } },
    "req_2f8c1a",
  );

  return (
    <>
      <PageHeader
        kicker="Design"
        title="Primitives"
        description="Every token, pill and primitive the console is built from. Not linked from the nav; it is a workshop surface."
      />

      <Section
        title="Canvas, ink and accent"
        note={
          <>
            Taken token for token from ApexTick except the accent. Hairlines are
            alpha-over-bone — <span className="tnum">rgba(244, 242, 236, α)</span> at
            0.10, 0.18 and 0.32 — never an opaque grey, because the design
            expects the pixels underneath to show through wherever a border
            crosses a filled bar.
          </>
        }
      >
        <div className="grid gap-3 sm:grid-cols-3 lg:grid-cols-5">
          {CANVAS_SWATCHES.map((s) => (
            <div key={s.name} className={surface + " overflow-hidden"}>
              <div className={`h-14 ${s.cls}`} />
              <div className="border-t border-line px-3 py-2">
                <p className="tnum text-[0.7rem] text-bone">{s.name}</p>
                <p className="tnum text-[0.65rem] text-faint">{s.hex}</p>
              </div>
            </div>
          ))}
        </div>

        <Card className="mt-4">
          <p className="text-[0.82rem] leading-relaxed text-muted">
            The accent is{" "}
            <span className="tnum text-accent">#3aa0ff</span>, arc blue, at{" "}
            <span className="tnum">7.17:1</span> on the canvas — AAA for body
            text, and the same ratio again for{" "}
            <span className="tnum">--accent-ink #0b0b0c</span> sitting on top of
            it, which is what lets the filled button and the skip link
            transplant from ApexTick unchanged. It was chosen last and by
            elimination: the state palette took its hues first, and this is the
            arc that was left. It is interactive chrome and nothing else. It is
            never a state, never a bar fill, and never means delete — that is
            the hardcoded <span className="tnum">#ff6b6b</span>.
          </p>
        </Card>
      </Section>

      <Section
        title="The state palette"
        note={
          <>
            Eight values, fixed by specification. They are ordered by relative
            luminance with at least 1.4:1 between adjacent pairs, because under
            deuteranopia and protanopia amber, green and red collapse into one
            yellow-olive region and separate by luminance alone. RUNNING is bone
            rather than the conventional cyan: moving it out of hue space
            entirely is what freed 190°–230° for the accent, and it makes the
            one state operators scan for the brightest thing on screen.
          </>
        }
      >
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {STATE_SWATCHES.map((s) => (
            <div key={s.state} className={surface + " overflow-hidden"}>
              <div className={`h-10 ${s.bg}`} />
              <div className={`h-6 ${s.fill}`} />
              <div className="border-t border-line px-3 py-2.5">
                <p className={`tnum text-[0.7rem] ${s.text}`}>{s.state}</p>
                <p className="tnum mt-1 text-[0.65rem] text-faint">
                  {s.hex} · {s.ratio}
                </p>
              </div>
            </div>
          ))}
        </div>
        <p className="mt-4 max-w-3xl text-[0.78rem] leading-relaxed text-faint">
          The accepted compromise: QUEUED at 6.60:1 and FAILED at 6.50:1 sit at
          effectively the same luminance and are identical under full
          achromatopsia. They are separated by chroma for every other vision
          type and by treatment for all of them — dotted-hollow with a ring
          glyph against solid-capped with a cross. Do not uniform the pill
          strokes.
        </p>
      </Section>

      <Section
        title="StatePill"
        note={
          <>
            Four carriers, and colour is only the fourth: the word, the glyph,
            the stroke treatment, then the hue. RUNNING is the one filled pill
            in the system. CANCELLING is derived by the caller from{" "}
            <span className="tnum">cancel_requested_at</span> and is not a
            domain state — the runtime honestly reports a cancelling job as
            RUNNING while its leased steps drain.
          </>
        }
      >
        <Card>
          <Kicker>Step scope, all eight</Kicker>
          <div className="mt-4 flex flex-wrap gap-2.5">
            {STEP_STATES.map((s) => (
              <StatePill key={s} state={s} />
            ))}
          </div>

          <Kicker className="mt-8 block">Job scope, six — never SCHEDULED or RETRYING</Kicker>
          <div className="mt-4 flex flex-wrap gap-2.5">
            {JOB_STATES.map((s) => (
              <StatePill key={s} state={s} scope="job" />
            ))}
          </div>

          <Kicker className="mt-8 block">Derived, and small</Kicker>
          <div className="mt-4 flex flex-wrap items-center gap-2.5">
            <StatePill state="RUNNING" scope="job" cancelling />
            <StatePill state="SUCCEEDED" size="sm" />
            <StatePill state="FAILED" size="sm" />
          </div>
        </Card>
      </Section>

      <Section
        title="StateGlyph"
        note="The non-colour carrier at 10×10. Hand-drawn, never emoji: an emoji ignores currentColor and renders as a different image on every platform, which defeats the only job a glyph has here."
      >
        <Card>
          <div className="flex flex-wrap gap-6">
            {STEP_STATES.map((s) => (
              <div key={s} className="flex flex-col items-center gap-2">
                <span className="flex h-8 w-8 items-center justify-center rounded-full border border-line-2 text-bone">
                  <StateGlyph state={s} />
                </span>
                <span className="tnum text-[0.6rem] text-faint">{s}</span>
              </div>
            ))}
          </div>
        </Card>
      </Section>

      <Section
        title="Waterfall textures"
        note={
          <>
            The third carrier, for the chart. Hatch is claimed-but-not-started —
            the pod-pending gap, which must never be merged into the running bar
            because it is often the most valuable interval on the screen. The
            dotted baseline is queued. The grid is the time axis behind the
            lanes.
          </>
        }
      >
        <Card>
          <div className="space-y-5">
            <div>
              <p className="tnum text-[0.65rem] text-faint">.hatch — SCHEDULED</p>
              <div
                className="hatch mt-2 h-7 rounded-sm border border-st-scheduled/50 bg-st-scheduled-fill"
                style={{ ["--hatch" as string]: "var(--st-scheduled)" }}
              />
            </div>
            <div>
              <p className="tnum text-[0.65rem] text-faint">.dotline — QUEUED baseline</p>
              <div
                className="dotline mt-2 h-[2px]"
                style={{ ["--hatch" as string]: "var(--st-queued)" }}
              />
            </div>
            <div>
              <p className="tnum text-[0.65rem] text-faint">.lane-grid — time gridlines</p>
              <div
                className="lane-grid mt-2 h-7 rounded-sm border border-line"
                style={{ ["--grid-step" as string]: "56px" }}
              />
            </div>
            <div>
              <p className="tnum text-[0.65rem] text-faint">
                solid bar — an outcome, 1px stroke over a 16% fill
              </p>
              <div className="mt-2 flex gap-2">
                <div className="h-7 w-24 rounded-sm border border-st-succeeded bg-st-succeeded-fill" />
                <div className="h-7 w-16 rounded-sm border border-st-failed bg-st-failed-fill" />
                <div className="h-7 w-20 rounded-sm border border-st-timedout bg-st-timedout-fill" />
                <div className="h-7 w-32 rounded-sm border border-st-running bg-st-running-fill" />
              </div>
            </div>
          </div>
        </Card>
      </Section>

      <Section
        title="Button"
        note="Variants are a plain lookup table composed through cn(). Add one by adding a key, not by installing cva. The danger variant is #ff6b6b and never the accent: arc blue is chrome, and FAILED red means a step failed, which is not the same statement as a control that cancels a job."
      >
        <Card>
          <div className="flex flex-wrap items-center gap-3">
            <Button size="sm">Primary</Button>
            <Button variant="outline" size="sm">
              Outline
            </Button>
            <Button variant="ghost" size="sm">
              Ghost
            </Button>
            <Button variant="danger" size="sm">
              Cancel job
            </Button>
            <Button size="sm" disabled>
              Disabled
            </Button>
          </div>
          <div className="mt-5 flex flex-wrap items-center gap-3">
            <Button size="sm" arrow>
              Small
            </Button>
            <Button size="md" arrow>
              Medium
            </Button>
            <Button size="lg" arrow>
              Large
            </Button>
          </div>
        </Card>
      </Section>

      <Section
        title="Freshness"
        note="A dashboard showing a green live dot over a stalled poll is worse than one that polls visibly. The age is always on screen and always counting, and it changes character as it grows. A terminal job reads FINAL, because finished data cannot go stale and a ticking age on one would measure nothing."
      >
        <Card>
          <div className="flex flex-wrap items-center gap-3">
            <Freshness updatedAt={mounted - 2_000} />
            <Freshness updatedAt={mounted - 18_000} />
            <Freshness updatedAt={mounted - 42_000} />
            <Freshness updatedAt={mounted - 90_000} />
            <Freshness updatedAt={mounted} terminal />
            <Freshness updatedAt={mounted} error={notFound} />
            <Freshness updatedAt={null} />
          </div>
          <p className="mt-4 text-[0.78rem] text-faint">
            Past 60 seconds the caller should also dim the content region it
            describes to 60%; <span className="tnum">stalenessOpacity()</span> is
            exported for that.
          </p>
        </Card>
      </Section>

      <Section title="Card, Kicker and the type scale">
        <div className="grid gap-4 lg:grid-cols-2">
          <Card>
            <Kicker>Kicker</Kicker>
            <p className="display mt-3 text-[clamp(1.5rem,3vw,2.2rem)]">
              Display, clamped
            </p>
            <p className="mt-3 text-[0.9rem] leading-relaxed text-muted">
              Body copy at 0.9rem with leading-relaxed and --muted, the default
              pairing for anything explanatory.
            </p>
            <p className="mt-2 text-[0.78rem] leading-relaxed text-faint">
              A hint at 0.78rem and --faint, for the line below the line.
            </p>
            <p className="tnum mt-4 text-[0.86rem] text-bone">
              1 234 567 · 03:24:11 UTC · 4m 12s
            </p>
          </Card>
          <Card pad="lg">
            <Kicker>Padding</Kicker>
            <p className="mt-3 text-[0.82rem] leading-relaxed text-muted">
              p-6 for a tile, p-10 for an empty state. The surface itself is the
              ApexTick literal{" "}
              <span className="tnum text-bone">
                rounded-2xl border border-line bg-ink-2
              </span>
              , exported as <span className="tnum text-bone">surface</span> for
              the places that need it without a wrapper element.
            </p>
          </Card>
        </div>
      </Section>

      <Section title="EmptyState, ErrorState and Skeleton">
        <div className="grid gap-4 lg:grid-cols-2">
          <EmptyState
            title="No jobs yet"
            hint="Nothing has been submitted to this runtime. A plan with an echo step and a report step is the fastest way to see the whole pipeline work."
            action={<Button size="sm">Submit a plan</Button>}
            glyph={<Icons.Scroll />}
          />
          <ErrorState error={notFound} caption="the job" onRetry={() => {}} />
          <Card>
            <Kicker>Skeleton</Kicker>
            <p className="mt-3 text-[0.78rem] leading-relaxed text-faint">
              Used only where the final geometry is already known — the job
              snapshot arrives before the events, so the waterfall can draw the
              right number of empty lanes and never reflow. It does not shimmer:
              motion on this product signals a state change, and a pulsing
              rectangle would be the only animation on screen that means nothing.
            </p>
            <div className="mt-4 space-y-1.5" aria-busy="true">
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-[28px]" rounded="sm" />
              ))}
            </div>
            <SkeletonLines className="mt-5" />
          </Card>
        </div>
      </Section>

      <Section
        title="Icons"
        note="Hand-written 24×24, fill none, stroke currentColor at 1.6. No icon library is installed and adding one would change the stroke weight and the optical size of every glyph beside it."
      >
        <Card>
          <div className="flex flex-wrap gap-5 text-bone">
            {Object.entries(Icons).map(([name, Icon]) => (
              <div key={name} className="flex w-16 flex-col items-center gap-2">
                <Icon className="h-5 w-5" />
                <span className="text-center text-[0.55rem] text-faint">{name}</span>
              </div>
            ))}
          </div>
        </Card>
      </Section>
    </>
  );
}
