"use client";

import { useCallback, useMemo, useState, type CSSProperties } from "react";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { useNow } from "@/lib/now";
import { useMediaQuery } from "@/hooks/useMediaQuery";
import type { ScaleMode } from "@/lib/scale";
import { STATE_LABEL, STATE_VAR } from "@/lib/state";
import type { JobResponse, RunMeshEvent } from "@/lib/types";
import {
  buildHistory,
  buildLanes,
  busyIntervals,
  depthBands,
  describeLane,
  laneSegments,
  parseInstant,
  queuedAnchor,
  waterfallDomain,
  type Segment,
} from "@/lib/waterfall";
import { AttemptList } from "./attempt-list";
import { ChartBody } from "./chart-body";
import { layout } from "./geometry";
import type { WaterfallSelection } from "./selection";
import { WaterfallTable } from "./waterfall-table";

/**
 * The execution waterfall: one lane per step, time on X, reconstructed entirely
 * from the event stream.
 *
 * ---------------------------------------------------------------------------
 * This component draws; it does not decide what is true. Every number it puts
 * on screen comes out of `lib/waterfall.ts` and `lib/scale.ts`, both of which
 * are pure, clock-free and tested against fixed histories. That split is not
 * tidiness — it is the only way the correctness of this screen is testable at
 * all, because the part that matters is the reconstruction and the part that
 * cannot be unit-tested is the pixels.
 *
 * The reconstruction is necessary because the job snapshot only ever describes
 * the CURRENT attempt: a claim overwrites scheduled_at, nulls started_at and
 * ended_at and increments attempt, and a release or a lease expiry nulls them
 * outright. A chart built from `stepResponse` timestamps draws one clean bar
 * for a step that actually failed twice — a confident lie, told on precisely
 * the jobs someone opened this screen to understand.
 *
 * Three decisions carry the rest of the design, and each has a cheaper wrong
 * answer that was rejected:
 *
 *   THE IDLE GUTTER. A 3ms echo and a 40-minute backoff share this axis. On a
 *   linear scale the echo is 0.002px wide. The scale collapses every stretch in
 *   which nothing was claimed or executing into a fixed 24px marked band, so
 *   proportions inside the active periods stay true and the dead time is
 *   visible AS dead time. `l` switches to a real linear axis, which answers a
 *   different question honestly rather than this one dishonestly.
 *
 *   EDGES ONLY ON SELECTION. Drawing every dependency turns a ten-step fan-out
 *   into a weave and a hundred-step job into a solid mat. The DAG's shape is
 *   carried statically by the topological depth bands and by the `blocked_by`
 *   chip; the edges appear for one lane at a time, which is the only form in
 *   which anyone has ever read them.
 *
 *   THE TABLE IS NOT A FALLBACK. Six hundred positioned rectangles cannot be
 *   made comprehensible by adding ARIA to the rectangles: length and position
 *   do not survive being read aloud. So `WaterfallTable` renders the same
 *   segments as real markup, sr-only beside the chart and visible on request,
 *   and the chart is the twin of it rather than the other way round.
 * ---------------------------------------------------------------------------
 *
 * This half does the reduction and owns the controls. Everything that is a
 * function of PIXELS lives in `ChartBody`, which mounts only when there is a
 * chart to draw — see the comment there for why that separation is load-bearing
 * rather than cosmetic.
 */

export type { WaterfallSelection } from "./selection";

export interface WaterfallProps {
  job: JobResponse;
  /** The accumulated event stream. Order does not matter: the reducer sorts by
   *  the per-job `seq`, which is gap-free and is the only total order the
   *  runtime guarantees. */
  events: readonly RunMeshEvent[];
  /** From the events endpoint. `true` means attempts are PERMANENTLY gone — the
   *  in-memory ring evicts — and the chart says so at its left edge rather than
   *  redrawing the job as though it began later than it did. */
  truncated?: boolean;
  oldestSeq?: number;

  /** Controlled when provided; the host keeps it in the URL as ?step=&attempt=. */
  selected?: WaterfallSelection | null;
  onSelect?: (selection: WaterfallSelection | null) => void;
  /** Enter, or a click on a segment that is already selected. The host opens its
   *  inspector here. Selection alone must not open a panel, or arrowing down the
   *  lanes would fling one open on every row. */
  onActivate?: (selection: WaterfallSelection) => void;

  /** The zoom window, in epoch ms. Controlled when provided. */
  window?: [number, number] | null;
  onWindow?: (window: [number, number] | null) => void;

  /** Epoch ms. Provide it to freeze the chart — a test, a screenshot, a terminal
   *  job. Omitted, the shared one-second page clock is used. */
  now?: number;

  /** Vertical room for the lane body before it scrolls internally. The sticky
   *  axis stays put while it does, which is the whole reason it is sticky. */
  maxBodyHeight?: number;
  className?: string;
}

export function Waterfall({
  job,
  events,
  truncated = false,
  oldestSeq = 0,
  selected: selectedProp,
  onSelect,
  onActivate,
  window: windowProp,
  onWindow,
  now: nowProp,
  maxBodyHeight = 560,
  className,
}: WaterfallProps) {
  /* ---------------------------------------------------------------- clock */

  // The shared page clock, not a timer of this component's own. Read
  // unconditionally, because hooks must be, and used only where the caller has
  // not frozen the chart by passing `now`.
  const tick = useNow();
  const now =
    nowProp ??
    tick ??
    // The server render, where there is no clock at all. Anchoring on the job's
    // own timestamps produces identical markup on both sides of hydration;
    // Date.now() would produce two different charts and React would throw the
    // server's away.
    parseInstant(job.ended_at) ??
    parseInstant(job.updated_at) ??
    parseInstant(job.created_at) ??
    0;

  /* ------------------------------------------------------------ reduction */

  const lanes = useMemo(() => buildLanes(job, events), [job, events]);
  const bands = useMemo(() => depthBands(lanes), [lanes]);
  const view = useMemo(() => layout(bands), [bands]);
  const history = useMemo(
    () => buildHistory(job, events, { truncated, oldest_seq: oldestSeq }),
    [job, events, truncated, oldestSeq],
  );

  const segmentsByLane = useMemo(() => {
    const map = new Map<string, Segment[]>();
    for (const lane of lanes) {
      map.set(lane.stepId, laneSegments(lane, { anchor: queuedAnchor(lane, job, lanes).at, now }));
    }
    return map;
  }, [lanes, job, now]);

  const allSegments = useMemo(() => [...segmentsByLane.values()].flat(), [segmentsByLane]);
  const busy = useMemo(() => busyIntervals(allSegments), [allSegments]);
  const fullDomain = useMemo(
    () => waterfallDomain(job, allSegments, now),
    [job, allSegments, now],
  );

  /** One spoken sentence per lane: the whole row, for a reader who has no
   *  chart. Assembled in the pure module, where the branches are tested. */
  const laneLabels = useMemo(() => {
    const map = new Map<string, string>();
    for (const lane of lanes) {
      map.set(
        lane.stepId,
        describeLane(
          lane,
          segmentsByLane.get(lane.stepId) ?? [],
          (state) => STATE_LABEL[state],
          (ms) => formatDuration(ms),
        ),
      );
    }
    return map;
  }, [lanes, segmentsByLane]);

  /* ------------------------------------------------------------- controls */

  const [mode, setMode] = useState<ScaleMode>("compress");
  const [windowState, setWindowState] = useState<[number, number] | null>(null);
  const [selectedState, setSelectedState] = useState<WaterfallSelection | null>(null);
  const [asTable, setAsTable] = useState(false);

  const zoom = windowProp !== undefined ? windowProp : windowState;
  const selected = selectedProp !== undefined ? selectedProp : selectedState;

  const setZoom = useCallback(
    (next: [number, number] | null) => {
      if (windowProp === undefined) setWindowState(next);
      onWindow?.(next);
    },
    [windowProp, onWindow],
  );

  const select = useCallback(
    (next: WaterfallSelection | null) => {
      if (selectedProp === undefined) setSelectedState(next);
      onSelect?.(next);
    },
    [selectedProp, onSelect],
  );

  /* --------------------------------------------------------- the rendering */

  // False on the server and on the first client render, so a device that has
  // not told us otherwise gets the narrow list. That is the safe guess: the
  // card list is a complete rendering at any width, whereas the chart on a
  // phone is not a rendering at all.
  const wide = useMediaQuery("(min-width: 900px)");

  const caption =
    `Execution waterfall for job ${job.name}: ${lanes.length} ` +
    `${lanes.length === 1 ? "step" : "steps"} over ` +
    `${formatDuration(fullDomain[1] - fullDomain[0])}, reconstructed from ` +
    `${events.length} events. Each lane shows one step's attempts as queued, ` +
    `pending and running intervals. The same data follows as a table.`;

  return (
    <figure className={cn("m-0 rounded-2xl border border-line bg-ink-2", className)}>
      <figcaption className="sr-only">{caption}</figcaption>

      <Toolbar
        mode={mode}
        onMode={setMode}
        zoomed={zoom !== null}
        onResetZoom={() => setZoom(null)}
        asTable={asTable}
        onAsTable={setAsTable}
        showAxisControls={wide && !asTable}
      />

      {history.truncated && (
        // Mandatory, not decorative. With RUNMESH_STORE=memory the per-job ring
        // holds 512 events and evicts, so a chart that renders the survivors as
        // though they were the whole story is worse than no chart at all.
        <p className="flex items-start gap-2 border-b border-line bg-st-retrying/[0.06] px-4 py-2 text-[0.65rem] leading-snug text-st-retrying">
          <span aria-hidden>⚠</span>
          <span>
            History truncated before seq <span className="tnum">{history.oldestSeq}</span> —
            earlier attempts were evicted from the in-memory event ring and cannot be
            drawn. This chart is incomplete on the left.
          </span>
        </p>
      )}

      {history.missingJobCreated && !history.truncated && (
        // A different bug with the same consequence for the chart: the caller
        // paged with ?after= and never asked for the beginning. Reported
        // separately so whoever reads it looks in the right place.
        <p className="border-b border-line px-4 py-2 text-[0.65rem] leading-snug text-faint">
          The stream does not include JOB_CREATED, so it was paged from a cursor
          rather than from the beginning. Anything before seq{" "}
          <span className="tnum">{history.oldestSeq}</span> is missing.
        </p>
      )}

      {asTable ? (
        <div className="overflow-x-auto p-4">
          <WaterfallTable job={job} lanes={lanes} segmentsByLane={segmentsByLane} />
        </div>
      ) : wide ? (
        <>
          <ChartBody
            lanes={lanes}
            order={view.order}
            rows={view.rows}
            laneTop={view.laneTop}
            laneMid={view.laneMid}
            height={view.height}
            segmentsByLane={segmentsByLane}
            laneLabels={laneLabels}
            busy={busy}
            fullDomain={fullDomain}
            zoom={zoom}
            onZoom={setZoom}
            mode={mode}
            onMode={setMode}
            selected={selected}
            onSelect={select}
            onActivate={onActivate}
            history={history}
            events={events}
            maxBodyHeight={maxBodyHeight}
          />
          <Legend />
        </>
      ) : (
        <div className="p-3">
          <AttemptList
            job={job}
            bands={bands}
            segmentsByLane={segmentsByLane}
            domain={fullDomain}
            selected={selected?.stepId ?? null}
            onSelect={(stepId) => select(stepId ? { stepId, attempt: null } : null)}
          />
        </div>
      )}

      {/* The same markup the visible table uses, and only while the visible one
          is not showing, so assistive technology is never handed the job twice. */}
      {!asTable && (
        <WaterfallTable
          job={job}
          lanes={lanes}
          segmentsByLane={segmentsByLane}
          className="sr-only"
        />
      )}
    </figure>
  );
}

/* -------------------------------------------------------------------------- */

function Toolbar({
  mode,
  onMode,
  zoomed,
  onResetZoom,
  asTable,
  onAsTable,
  showAxisControls,
}: {
  mode: ScaleMode;
  onMode: (mode: ScaleMode) => void;
  zoomed: boolean;
  onResetZoom: () => void;
  asTable: boolean;
  onAsTable: (value: boolean) => void;
  showAxisControls: boolean;
}) {
  const chip =
    "rounded-full border px-2.5 py-0.5 font-mono text-[0.6rem] uppercase tracking-[0.14em] transition-colors";

  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
      <span className="kicker mr-auto text-[0.5625rem]">execution waterfall</span>

      {showAxisControls && (
        <>
          <button
            type="button"
            onClick={() => onMode(mode === "compress" ? "linear" : "compress")}
            aria-pressed={mode === "compress"}
            className={cn(
              chip,
              mode === "compress"
                ? "border-accent/45 text-accent"
                : "border-line-2 text-muted hover:text-bone-2",
            )}
            style={{ transitionDuration: "var(--dur-hover)" }}
            // Spelled out rather than left to the label: a reader has to know
            // this changes the AXIS and not the data before they will trust
            // either setting.
            title="Collapse idle stretches into marked gutters (keyboard: l)"
          >
            {mode === "compress" ? "idle collapsed" : "linear time"}
          </button>

          <button
            type="button"
            onClick={onResetZoom}
            disabled={!zoomed}
            className={cn(
              chip,
              "border-line-2 text-muted hover:text-bone-2 disabled:opacity-40 disabled:hover:text-muted",
            )}
            style={{ transitionDuration: "var(--dur-hover)" }}
            title="Reset the zoom window (keyboard: 0)"
          >
            full run
          </button>
        </>
      )}

      <button
        type="button"
        onClick={() => onAsTable(!asTable)}
        aria-pressed={asTable}
        className={cn(
          chip,
          asTable ? "border-accent/45 text-accent" : "border-line-2 text-muted hover:text-bone-2",
        )}
        style={{ transitionDuration: "var(--dur-hover)" }}
      >
        {asTable ? "chart" : "table"}
      </button>
    </div>
  );
}

/**
 * The key, and the keyboard.
 *
 * Both are text rather than an icon row, because both are things a reader looks
 * up once and then stops seeing. The four intervals are named in the same words
 * the hover card and the table use, so nothing on this screen is called two
 * different things in two places.
 */
function Legend() {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 border-t border-line px-4 py-2 text-[0.6rem] text-faint">
      <Swatch kind="queued" label="queued (inferred)" />
      <Swatch kind="pending" label="claimed, not started" />
      <Swatch kind="run" label="tool executing" />
      <Swatch kind="backoff" label="retry backoff" />
      <span className="tnum ml-auto">
        ↑↓ lane · ←→ attempt · enter inspect · esc clear · [ ] pan · + − 0 zoom · l axis
      </span>
    </div>
  );
}

function Swatch({ kind, label }: { kind: Segment["kind"]; label: string }) {
  const colour =
    kind === "queued"
      ? `var(${STATE_VAR.QUEUED})`
      : kind === "pending"
        ? `var(${STATE_VAR.SCHEDULED})`
        : kind === "backoff"
          ? `var(${STATE_VAR.RETRYING})`
          : `var(${STATE_VAR.RUNNING})`;

  return (
    <span className="flex items-center gap-1.5">
      <span
        aria-hidden
        className={cn(
          "inline-block w-6",
          (kind === "queued" || kind === "backoff") && "dotline h-[2px]",
          kind === "pending" && "hatch h-2.5 border",
          kind === "run" && "h-2.5 border",
        )}
        style={
          {
            borderColor: colour,
            ["--hatch"]: colour,
          } as CSSProperties
        }
      />
      {label}
    </span>
  );
}
