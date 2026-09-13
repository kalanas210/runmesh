import { describe, expect, it } from "vitest";
import {
  HISTORY_PAGE_FRESH,
  TRANSPORT_CONNECTING,
  cursorOf,
  eventsPollMs,
  jobPollMs,
  latchHistoryPage,
  mergeEvents,
  streamFailureReason,
  transportFromStream,
  type TransportState,
} from "./feed";
import type { RunMeshEvent } from "./types";

function event(seq: number, type: RunMeshEvent["type"] = "STEP_STARTED"): RunMeshEvent {
  return { global_seq: seq + 1000, seq, job_id: "j1", type, at: "2026-09-12T03:00:00Z" };
}

describe("mergeEvents", () => {
  it("deduplicates on seq and orders ascending", () => {
    // The overlap is the normal case, not the edge case: the stream's snapshot
    // page and the poller's ?after= page are both catch-up reads of the same
    // rows, and a reconnect replays from the cursor on purpose.
    const merged = mergeEvents([event(3), event(1)], [event(2), event(3)]);
    expect(merged.map((e) => e.seq)).toEqual([1, 2, 3]);
  });

  it("lets the later copy of a seq win", () => {
    const first = { ...event(1), type: "STEP_SCHEDULED" as const };
    const second = { ...event(1), type: "STEP_STARTED" as const };
    expect(mergeEvents([first], [second])[0].type).toBe("STEP_STARTED");
  });

  it("returns the same array reference when nothing is new", () => {
    // Reference equality is load-bearing rather than an optimisation: buildLanes
    // is memoised on the events array, so a fresh array on every empty poll
    // would re-reduce the whole history once a second on an idle job.
    const current = [event(1), event(2)];
    expect(mergeEvents(current, [])).toBe(current);
    expect(mergeEvents(current, [event(2)])).toBe(current);
  });

  it("does not mistake an empty accumulator for an empty page", () => {
    expect(mergeEvents([], [event(1)]).map((e) => e.seq)).toEqual([1]);
  });

  it("applies a correction that differs only inside attrs", () => {
    // The reference-equality shortcut has to compare BODIES and not only seqs,
    // or a resync — which exists so a client can be handed a corrected history
    // — could never correct anything. attrs is map[string]any with no schema,
    // so a difference can live entirely inside it.
    const held = { ...event(1, "STEP_LEASE_EXPIRED"), attrs: { owner: "worker-1" } };
    const corrected = { ...event(1, "STEP_LEASE_EXPIRED"), attrs: { owner: "worker-2" } };
    expect(mergeEvents([held], [corrected])[0].attrs).toEqual({ owner: "worker-2" });
  });

  it("still returns the same reference when a duplicate is byte-identical", () => {
    // The other half of the same rule: a reconnect's snapshot overlaps what the
    // poller already has, and re-reducing the whole history because the
    // transport wobbled would be the expensive answer to nothing happening.
    const held = [{ ...event(1), attrs: { steps: 2 } }];
    expect(mergeEvents(held, [{ ...event(1), attrs: { steps: 2 } }])).toBe(held);
  });
});

describe("cursorOf", () => {
  it("is the highest seq, because ?after= is exclusive", () => {
    expect(cursorOf([event(4), event(9), event(2)])).toBe(9);
  });

  it("is 0 for an empty history, which asks for everything retained", () => {
    expect(cursorOf([])).toBe(0);
  });
});

describe("transportFromStream", () => {
  it("reports live only while the connection is open", () => {
    expect(transportFromStream("open", TRANSPORT_CONNECTING).mode).toBe("live");
  });

  it("degrades to polling when the stream drops, and says why", () => {
    const live: TransportState = { mode: "live", permanent: false };
    const next = transportFromStream("reconnecting", live);
    expect(next.mode).toBe("polling");
    expect(next.reason).toBeTruthy();
  });

  it("does not flip a degraded screen back to connecting on a retry", () => {
    // The whole point of the chip is that a reader can trust it. A reconnect
    // attempt cycling the label back through a hopeful "connecting" would make
    // a permanently broken stream look like it was always just about to work.
    const degraded: TransportState = { mode: "polling", permanent: true, reason: "gone" };
    expect(transportFromStream("connecting", degraded)).toBe(degraded);
  });

  it("leaves a finished stream final rather than calling it degraded", () => {
    const final: TransportState = { mode: "final", permanent: false };
    expect(transportFromStream("closed", final)).toBe(final);
  });
});

describe("streamFailureReason", () => {
  it.each([
    [404, true],
    [405, true],
    [403, true],
    [501, true],
    [502, false],
  ])("classifies %i as permanent=%s", (status, permanent) => {
    const result = streamFailureReason({ status });
    expect(result.permanent).toBe(permanent);
    expect(result.reason).not.toBe("");
  });

  it("tolerates an error that is not an HTTP failure at all", () => {
    expect(streamFailureReason(new Error("network")).permanent).toBe(false);
  });
});

describe("poll intervals", () => {
  it("stops both polls on a terminal job", () => {
    // A finished job's ETag answers 304 for ever. A console left open on one
    // should cost the runtime nothing at all.
    expect(jobPollMs("live", true)).toBe(false);
    expect(eventsPollMs("polling", true)).toBe(false);
  });

  it("keeps polling the job row even while the stream is live", () => {
    // The stream carries events; blocked_by, result and the rolled-up job state
    // arrive only on the job row, so the poll slows down and never stops.
    expect(jobPollMs("live", false)).toBe(5000);
    expect(eventsPollMs("live", false)).toBe(false);
  });

  it("takes over the events endpoint the moment the stream degrades", () => {
    expect(eventsPollMs("polling", false)).toBe(1000);
    expect(jobPollMs("polling", false)).toBe(1000);
  });
});

describe("latchHistoryPage", () => {
  /**
   * The two facts have different scopes and that is the whole bug. `truncated`
   * on the wire compares ONE request's `?after=` cursor against the ring's
   * oldest surviving seq; the banner in waterfall.tsx describes the
   * ACCUMULATED list. Assigning the first to the second meant the warning
   * deleted itself on the next poll, because by then the cursor had advanced
   * past the eviction point and the request really was complete.
   */
  it("keeps the warning once a page has reported a hole", () => {
    const first = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: true, oldestSeq: 513 });
    expect(first).toEqual({ truncated: true, oldestSeq: 513 });

    // The next poll asks `?after=812` and every event it wants is retained, so
    // the endpoint answers honestly that THIS request lost nothing. The events
    // between 1 and 512 are still gone for ever.
    const second = latchHistoryPage(first, { truncated: false, oldestSeq: 513 });
    expect(second.truncated).toBe(true);
  });

  it("freezes the boundary at the seq that was oldest when the hole appeared", () => {
    // The ring keeps evicting while this client reads, so `oldest_seq` keeps
    // rising over events the client already holds and can plainly see drawn on
    // the chart. Following the newest value would move "truncated before seq
    // N" to the right and claim those were missing too.
    const latched = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: true, oldestSeq: 513 });
    const later = latchHistoryPage(latched, { truncated: false, oldestSeq: 1_204 });
    expect(later.oldestSeq).toBe(513);
  });

  it("tracks the newest boundary while there is no hole to describe", () => {
    const page = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: false, oldestSeq: 1 });
    expect(latchHistoryPage(page, { truncated: false, oldestSeq: 40 }).oldestSeq).toBe(40);
  });

  it("latches a hole reported by the stream's snapshot, not only by the poll", () => {
    // A reconnect's snapshot is read from a cursor that has already advanced,
    // so it is the likeliest single source of a `truncated: false` — and it
    // must also be able to RAISE the flag, since on a first connect it is the
    // only source there is.
    const page = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: true, oldestSeq: 900 });
    expect(page.truncated).toBe(true);
    expect(latchHistoryPage(page, {}).truncated).toBe(true);
  });

  it("returns the same object when nothing changed", () => {
    // This runs on every poll of an idle job. A fresh object each time is a
    // re-render of the job detail screen, and therefore of the waterfall,
    // because the server repeated itself.
    const page = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: true, oldestSeq: 7 });
    expect(latchHistoryPage(page, { truncated: true, oldestSeq: 7 })).toBe(page);
    expect(latchHistoryPage(page, { truncated: false, oldestSeq: 99 })).toBe(page);
    expect(latchHistoryPage(HISTORY_PAGE_FRESH, {})).toBe(HISTORY_PAGE_FRESH);
  });

  it("clears only by discarding the list it describes", () => {
    // The two callers that reset are a resync, which throws the accumulated
    // list away, and a change of job id. Nothing a response says can clear it.
    const page = latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: true, oldestSeq: 7 });
    expect(latchHistoryPage(HISTORY_PAGE_FRESH, { truncated: false, oldestSeq: 7 }).truncated).toBe(
      false,
    );
    expect(page.truncated).toBe(true);
  });
});
