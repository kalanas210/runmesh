"use client";

import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from "react";
import { useElementWidth } from "@/hooks/useElementWidth";
import { usePrefersReducedMotion } from "@/hooks/useMediaQuery";
import { makeScale, type ScaleMode } from "@/lib/scale";
import type { RunMeshEvent } from "@/lib/types";
import { attemptAt, type Lane, type Segment, type WaterfallHistory } from "@/lib/waterfall";
import { DependencyOverlay, type LaneEdges } from "./dependency-overlay";
import { AXIS_H, GUTTER_W, layout } from "./geometry";
import { LaneRow } from "./lane";
import { OverviewBrush } from "./overview-brush";
import { SegmentPopover } from "./segment-popover";
import { TimeAxis } from "./time-axis";
import type { WaterfallSelection } from "./selection";

/**
 * The plotted chart: the measured part.
 *
 * It is a separate component from `Waterfall` for one concrete reason, and the
 * reason is worth stating because the symptom it fixes is silent. `Waterfall`
 * chooses between this and the narrow card list from a media query, which
 * resolves to "narrow" on the server and during the first client render. So
 * this subtree MOUNTS one render late. `useElementWidth` measures in an effect
 * keyed on the ref — which is stable — so if that hook lived in `Waterfall` it
 * would run exactly once, on a render where the plot element did not exist yet,
 * find nothing, and hold a width of zero for ever. Every bar would then map to
 * x=0 and be clamped to the 3px minimum: a chart that renders, throws no error,
 * logs nothing, and is a column of identical stubs.
 *
 * Putting the measurement in the component that owns the element makes the
 * mount and the measurement the same event, which is the only arrangement that
 * cannot drift.
 *
 * It also owns everything that is a function of pixels — the scale, the lane
 * geometry, the pointer, the keyboard — so that the reducing half above stays
 * free of the DOM.
 */
export function ChartBody({
  lanes,
  order,
  rows,
  laneTop,
  laneMid,
  height,
  segmentsByLane,
  laneLabels,
  busy,
  fullDomain,
  zoom,
  onZoom,
  mode,
  onMode,
  selected,
  onSelect,
  onActivate,
  history,
  events,
  maxBodyHeight,
}: {
  lanes: readonly Lane[];
  /** Lanes in visual order, which is the keyboard order. */
  order: readonly Lane[];
  rows: ReturnType<typeof layout>["rows"];
  laneTop: ReadonlyMap<string, number>;
  laneMid: ReadonlyMap<string, number>;
  height: number;
  segmentsByLane: ReadonlyMap<string, readonly Segment[]>;
  laneLabels: ReadonlyMap<string, string>;
  busy: ReadonlyArray<readonly [number, number]>;
  fullDomain: [number, number];
  zoom: [number, number] | null;
  onZoom: (window: [number, number] | null) => void;
  mode: ScaleMode;
  onMode: (mode: ScaleMode) => void;
  selected: WaterfallSelection | null;
  onSelect: (selection: WaterfallSelection | null) => void;
  onActivate?: (selection: WaterfallSelection) => void;
  history: WaterfallHistory;
  events: readonly RunMeshEvent[];
  maxBodyHeight: number;
}) {
  const plotRef = useRef<HTMLDivElement | null>(null);
  const plotWidth = useElementWidth(plotRef);

  // Memoised rather than built inline, so the scale — and with it every tick
  // and every gutter — is not rebuilt on each render of a component that
  // re-renders once a second by design.
  const domain = useMemo<[number, number]>(() => zoom ?? fullDomain, [zoom, fullDomain]);
  const scale = useMemo(
    () => makeScale({ domain, width: plotWidth, mode, busy }),
    [domain, plotWidth, mode, busy],
  );

  /* ------------------------------------------------------------ selection */

  const selectedLane = selected
    ? (lanes.find((lane) => lane.stepId === selected.stepId) ?? null)
    : null;

  const dependents = useMemo(() => {
    if (!selectedLane) return new Set<string>();
    const set = new Set<string>();
    for (const lane of lanes) {
      if (lane.dependsOn.includes(selectedLane.stepId)) set.add(lane.stepId);
    }
    return set;
  }, [lanes, selectedLane]);

  const dependencies = useMemo(
    () => new Set(selectedLane?.dependsOn ?? []),
    [selectedLane],
  );

  /** Where each lane's drawn WORK starts and ends, for the dependency edges. */
  const laneEdges = useMemo(() => {
    const map = new Map<string, LaneEdges>();
    for (const lane of lanes) {
      const segments = segmentsByLane.get(lane.stepId) ?? [];
      // Measured across the work rather than across the queued baseline. An
      // edge landing on the left edge of a queued bar would point at the moment
      // the step became ELIGIBLE, which is inferred, instead of at the moment it
      // was claimed, which is recorded.
      const work = segments.filter((segment) => segment.kind !== "queued");
      const first = work[0] ?? segments[0];
      const last = work[work.length - 1] ?? segments[segments.length - 1];
      if (!first || !last) continue;
      map.set(lane.stepId, { startX: scale.x(first.from), endX: scale.x(last.to) });
    }
    return map;
  }, [lanes, segmentsByLane, scale]);

  /* ------------------------------------------------------- keyboard focus */

  const rowRefs = useRef(new Map<string, HTMLDivElement>());
  const [lastFocused, setLastFocused] = useState(0);

  // The roving tab stop is DERIVED from the selection rather than synchronised
  // to it in an effect. The host keeps the selection in the URL, so a shared
  // link has to put the keyboard in the right place as well as the highlight;
  // an effect copying one into the other would render the chart once with the
  // tab stop on row one and again with it in the right place, which is a
  // cascading render on every navigation for a value already known during the
  // first one. `lastFocused` is only the fallback for when nothing is selected.
  const selectedIndex = selected
    ? order.findIndex((lane) => lane.stepId === selected.stepId)
    : -1;
  const cursor = selectedIndex >= 0 ? selectedIndex : lastFocused;

  const focusLane = useCallback(
    (index: number) => {
      const lane = order[index];
      if (!lane) return;
      setLastFocused(index);
      rowRefs.current.get(lane.stepId)?.focus();
      // Arrowing selects. A keyboard reader who could move between lanes but
      // never see the dependency overlay or the tint follow would be getting a
      // strictly worse chart than the pointer reader, for no reason.
      onSelect({ stepId: lane.stepId, attempt: null });
    },
    [order, onSelect],
  );

  const panBy = useCallback(
    (fraction: number) => {
      const [from, to] = zoom ?? fullDomain;
      const width = to - from;
      const lo = Math.max(fullDomain[0], from + width * fraction);
      const hi = Math.min(fullDomain[1], to + width * fraction);
      // Refuse a pan that would shrink the window against a domain edge: the
      // reader asked to move, not to zoom.
      if (hi - lo < width - 1) return;
      onZoom([lo, hi]);
    },
    [zoom, fullDomain, onZoom],
  );

  const zoomBy = useCallback(
    (factor: number) => {
      const [from, to] = zoom ?? fullDomain;
      const centre = (from + to) / 2;
      const half = ((to - from) * factor) / 2;
      const lo = Math.max(fullDomain[0], centre - half);
      const hi = Math.min(fullDomain[1], centre + half);
      if (hi - lo <= 0) return;
      // Zooming back out to the whole run clears the window rather than
      // setting one identical to the domain, so the brush and the "full run"
      // control agree about whether there is a window at all.
      onZoom(lo <= fullDomain[0] && hi >= fullDomain[1] ? null : [lo, hi]);
    },
    [zoom, fullDomain, onZoom],
  );

  const onKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLDivElement>) => {
      const lane = order[cursor];

      switch (event.key) {
        case "ArrowDown":
          event.preventDefault();
          focusLane(Math.min(cursor + 1, order.length - 1));
          return;
        case "ArrowUp":
          event.preventDefault();
          focusLane(Math.max(cursor - 1, 0));
          return;
        case "Home":
          event.preventDefault();
          focusLane(0);
          return;
        case "End":
          event.preventDefault();
          focusLane(order.length - 1);
          return;
        case "ArrowRight":
        case "ArrowLeft": {
          if (!lane || lane.attempts.length === 0) return;
          event.preventDefault();
          const attempts = lane.attempts;
          const current = attempts.findIndex((attempt) => attempt.n === selected?.attempt);
          const step = event.key === "ArrowRight" ? 1 : -1;
          // From no attempt, right enters at the first and left at the last, so
          // both directions have somewhere to go from the bare lane.
          const next =
            current === -1
              ? step > 0
                ? 0
                : attempts.length - 1
              : Math.min(Math.max(current + step, 0), attempts.length - 1);
          onSelect({ stepId: lane.stepId, attempt: attempts[next].n });
          return;
        }
        case "Enter":
        case " ":
          if (!lane) return;
          event.preventDefault();
          onActivate?.(selected ?? { stepId: lane.stepId, attempt: null });
          return;
        case "Escape":
          event.preventDefault();
          onSelect(null);
          return;
        case "[":
          event.preventDefault();
          panBy(-0.25);
          return;
        case "]":
          event.preventDefault();
          panBy(0.25);
          return;
        case "+":
        case "=":
          event.preventDefault();
          zoomBy(0.5);
          return;
        case "-":
        case "_":
          event.preventDefault();
          zoomBy(2);
          return;
        case "0":
          event.preventDefault();
          onZoom(null);
          return;
        case "l":
        case "L":
          event.preventDefault();
          onMode(mode === "compress" ? "linear" : "compress");
          return;
        default:
          return;
      }
    },
    [cursor, order, selected, onSelect, onActivate, focusLane, panBy, zoomBy, onZoom, onMode, mode],
  );

  /* ----------------------------------------------------------- the pointer */

  const [hover, setHover] = useState<{ lane: Lane; segment: Segment } | null>(null);
  const [pointerAt, setPointerAt] = useState<number | null>(null);
  const plotBox = useRef<DOMRect | null>(null);

  // The plot's left edge is cached on entry rather than read on every move.
  // getBoundingClientRect inside a pointermove handler forces a layout on a
  // component that already re-renders once a second, and the left edge cannot
  // change while the pointer is inside: this chart never scrolls horizontally,
  // and vertical scroll moves only `top`.
  const refreshBox = useCallback(() => {
    plotBox.current = plotRef.current?.getBoundingClientRect() ?? null;
  }, []);

  const onPointerMove = useCallback((event: React.PointerEvent<HTMLDivElement>) => {
    const box = plotBox.current;
    if (!box) return;
    setPointerAt(event.clientX - box.left);
  }, []);

  const cursorTime = pointerAt === null ? null : scale.invert(pointerAt);

  /* -------------------------------------------------------------- the flash */

  const reducedMotion = usePrefersReducedMotion();
  // 400ms of movement, or four seconds of a static rail. The cue has to last
  // long enough to be FOUND when it is not allowed to move, which is longer
  // than it needs to be when it is.
  useEventFlash(events, reducedMotion ? 4_000 : 400, rowRefs);

  /* --------------------------------------------------------------- drawing */

  const truncatedX =
    history.truncated && history.earliestAt !== undefined
      ? scale.x(history.earliestAt)
      : 0;

  return (
    <>
      <div
        className="relative overflow-y-auto overflow-x-hidden"
        style={{ maxHeight: maxBodyHeight }}
        onPointerEnter={refreshBox}
        onPointerMove={onPointerMove}
        onPointerLeave={() => {
          setPointerAt(null);
          setHover(null);
        }}
      >
        <div className="sticky top-0 z-30 flex bg-ink-2">
          <div
            className="sticky left-0 z-10 flex shrink-0 items-end border-b border-r border-line bg-ink-2 pb-1.5 pl-3"
            style={{ width: GUTTER_W, height: AXIS_H }}
          >
            <span className="kicker text-[0.5625rem]">step · tool</span>
          </div>
          <div ref={plotRef} className="relative min-w-0 flex-1">
            <TimeAxis scale={scale} cursor={cursorTime} />
          </div>
        </div>

        <div
          role="grid"
          aria-label={`Execution waterfall, ${lanes.length} steps`}
          aria-rowcount={rows.length}
          className="relative outline-none"
          onKeyDown={onKeyDown}
        >
          {/* Everything that spans lanes lives in one overlay pinned to the
              plot's origin, so the edges, the time rule and the hover card all
              use the same coordinate system the bars do. */}
          <div
            className="pointer-events-none absolute top-0 z-20"
            style={{ left: GUTTER_W, width: scale.width, height }}
          >
            {truncatedX > 0 && (
              // The evicted stretch, hatched. The axis still starts at the
              // job's creation: starting it at the oldest surviving event
              // would silently redraw the job as though it had begun later
              // than it did, which is the one thing this band exists to stop.
              <div
                className="hatch absolute inset-y-0 left-0 border-r border-dashed border-st-retrying/50"
                style={{
                  width: truncatedX,
                  ["--hatch" as string]: "var(--st-retrying)",
                  opacity: 0.5,
                }}
              />
            )}

            {pointerAt !== null && (
              <div className="absolute inset-y-0 w-px bg-bone/20" style={{ left: pointerAt }} />
            )}

            {selectedLane && (
              <DependencyOverlay
                lane={selectedLane}
                edges={laneEdges}
                laneMid={laneMid}
                width={scale.width}
                height={height}
              />
            )}

            {hover && (
              <SegmentPopover
                lane={hover.lane}
                attempt={attemptAt(hover.lane, hover.segment)}
                segments={segmentsByLane.get(hover.lane.stepId) ?? []}
                segment={hover.segment}
                scale={scale}
                laneTop={laneTop.get(hover.lane.stepId) ?? 0}
                bodyHeight={height}
              />
            )}
          </div>

          {/* The vertical time gridlines get their own layer, pinned to the
              plot's origin rather than applied to the rows container: a
              background on the rows would start at the LABEL gutter's left edge
              and every line would sit 264px from the tick it belongs to. */}
          <div
            aria-hidden
            className="lane-grid pointer-events-none absolute top-0 z-0"
            style={{
              left: GUTTER_W,
              width: scale.width,
              height,
              ["--grid-step" as string]: `${gridStep(scale.ticks)}px`,
            }}
          />

          <div className="relative">
            {rows.map((row, index) =>
              row.kind === "band" ? (
                <div
                  key={`band-${row.depth}`}
                  role="row"
                  aria-rowindex={index + 1}
                  className="flex items-end"
                  style={{ height: row.height }}
                >
                  <div
                    role="rowheader"
                    className="sticky left-0 z-10 flex h-full shrink-0 items-end border-r border-line bg-ink pb-1 pl-3"
                    style={{ width: GUTTER_W }}
                  >
                    <span className="kicker text-[0.5rem]">
                      depth {row.depth} · {row.count} {row.count === 1 ? "step" : "steps"}
                    </span>
                  </div>
                  <div role="gridcell" className="relative h-full min-w-0 flex-1">
                    <span aria-hidden className="hairline absolute inset-x-0 bottom-0 h-px" />
                  </div>
                </div>
              ) : (
                <LaneRow
                  key={row.lane.stepId}
                  lane={row.lane}
                  segments={segmentsByLane.get(row.lane.stepId) ?? []}
                  scale={scale}
                  rowIndex={index + 1}
                  ariaLabel={laneLabels.get(row.lane.stepId) ?? row.lane.stepId}
                  selected={selected?.stepId === row.lane.stepId}
                  selectedAttempt={
                    selected?.stepId === row.lane.stepId ? selected.attempt : null
                  }
                  dimmed={
                    selectedLane !== null &&
                    selectedLane.stepId !== row.lane.stepId &&
                    !dependencies.has(row.lane.stepId) &&
                    !dependents.has(row.lane.stepId)
                  }
                  isDependency={dependencies.has(row.lane.stepId)}
                  isDependent={dependents.has(row.lane.stepId)}
                  hovered={hover?.lane.stepId === row.lane.stepId ? hover.segment : null}
                  tabbable={row.ordinal === cursor}
                  rowRef={(element) => {
                    if (element) rowRefs.current.set(row.lane.stepId, element);
                    else rowRefs.current.delete(row.lane.stepId);
                  }}
                  onHover={(lane, segment) => setHover(segment ? { lane, segment } : null)}
                  onSelect={(lane, segment) => {
                    const next: WaterfallSelection = {
                      stepId: lane.stepId,
                      attempt: segment?.attempt ?? null,
                    };
                    setLastFocused(row.ordinal);
                    // A second click on what is already selected is the pointer
                    // equivalent of Enter. A first click must not open a panel,
                    // or every glance at a bar costs a dismiss.
                    const same =
                      selected?.stepId === next.stepId && selected?.attempt === next.attempt;
                    onSelect(next);
                    if (same) onActivate?.(next);
                  }}
                  onFocus={(lane) => {
                    const at = order.findIndex((entry) => entry.stepId === lane.stepId);
                    if (at >= 0) setLastFocused(at);
                  }}
                />
              ),
            )}
          </div>
        </div>
      </div>

      <OverviewBrush domain={fullDomain} window={zoom} onWindow={onZoom} busy={busy} />
    </>
  );
}

/**
 * The vertical gridline spacing, in pixels.
 *
 * Taken from the first two ticks rather than from the tick step in
 * milliseconds, because on a compressed axis a duration does not correspond to
 * a fixed number of pixels — the gutters swallow time without swallowing width.
 * Two adjacent ticks inside one active piece do.
 */
function gridStep(ticks: ReadonlyArray<{ x: number }>): number {
  const [first, second] = ticks;
  if (!first || !second) return 120;
  const step = second.x - first.x;
  return step > 8 ? step : 120;
}

/**
 * Flash the rows that received an event since the last poll.
 *
 * The cue exists because the chart updates by polling, and a bar that silently
 * grows a few pixels between two frames is a change nobody sees. It is keyed on
 * the highest `seq` per step, not on a count or on array identity: React Query
 * hands back a new array on every poll whether anything changed or not, and
 * flashing all hundred lanes once a second would be strictly worse than
 * flashing none.
 *
 * It writes `data-flash` onto the row element DIRECTLY rather than holding a
 * set in state, and that is the right shape rather than a shortcut. A flash is
 * a transient presentational cue that React renders nothing from; routing it
 * through state would re-render a hundred lanes and rebuild the scale, twice,
 * in order to fade one 2px rail. The CSS that reads the attribute lives beside
 * the keyframes in globals.css, including the reduced-motion branch where the
 * rail stops moving and becomes a static inset rule rather than disappearing.
 *
 * The first pass never flashes. Mounting a chart of a finished job and having
 * every row light up would say "these all just changed", which is false.
 */
function useEventFlash(
  events: readonly RunMeshEvent[],
  holdMs: number,
  rows: RefObject<Map<string, HTMLElement>>,
): void {
  const seen = useRef(new Map<string, number>());
  const primed = useRef(false);
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  useEffect(() => {
    const latest = new Map<string, number>();
    for (const event of events) {
      if (!event.step_id) continue;
      const previous = latest.get(event.step_id);
      if (previous === undefined || event.seq > previous) latest.set(event.step_id, event.seq);
    }

    if (!primed.current) {
      primed.current = true;
      seen.current = latest;
      return;
    }

    const changed: string[] = [];
    for (const [stepId, seq] of latest) {
      const previous = seen.current.get(stepId);
      if (previous === undefined || seq > previous) changed.push(stepId);
    }
    seen.current = latest;

    for (const stepId of changed) {
      const row = rows.current.get(stepId);
      if (!row) continue;

      // Removed and re-added a frame later rather than simply set. A row that
      // receives two events inside one hold would otherwise keep the animation
      // it is already running and show nothing at all for the second, and a
      // frame is well below the threshold at which anyone notices the gap.
      row.removeAttribute("data-flash");
      requestAnimationFrame(() => {
        rows.current.get(stepId)?.setAttribute("data-flash", "");
      });

      const running = timers.current.get(stepId);
      if (running) clearTimeout(running);
      timers.current.set(
        stepId,
        setTimeout(() => {
          timers.current.delete(stepId);
          rows.current.get(stepId)?.removeAttribute("data-flash");
        }, holdMs),
      );
    }
  }, [events, holdMs, rows]);

  useEffect(() => {
    const pending = timers.current;
    return () => {
      for (const timer of pending.values()) clearTimeout(timer);
      pending.clear();
    };
  }, []);
}
