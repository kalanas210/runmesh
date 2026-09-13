import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { openJobStream, type StreamStatus } from "./stream";
import {
  TRANSPORT_CONNECTING,
  eventsPollMs,
  streamFailureReason,
  transportFromStream,
  type TransportState,
} from "./feed";

/**
 * The reader's liveness contract, which is the one thing about this file that
 * a reader of the dashboard can be actively misled by.
 *
 * The failure these tests exist for is not a crash. It is a connection that is
 * ESTABLISHED and then silent — a dropped NAT mapping, a balancer that stopped
 * forwarding without a FIN, a laptop resumed onto a different network, a Go
 * process SIGKILLed mid-stream. `reader.read()` never resolves and there is no
 * error to catch, so the old reader sat on it for ever while the chip said
 * LIVE and the fallback poll stayed switched off, because eventsPollMs("live")
 * is `false`. The waterfall froze and the Freshness component — which exists
 * for precisely this — had nothing to say.
 *
 * So the assertions below are not about statuses in the abstract. Each one
 * folds the reader's statuses through the same two functions useJobFeed folds
 * them through, and then asks the question the operator is really asking: is
 * this screen still being fed, and does it know.
 *
 * There is no real waiting anywhere here. Time advances through vitest's fake
 * timers, so "forty seconds of silence" is an exact number of milliseconds and
 * the test costs a millisecond of wall clock.
 */

/** A body whose chunks this test enqueues by hand, one connection's worth. */
function sseSource(): {
  body: ReadableStream<Uint8Array>;
  push: (text: string) => void;
  close: () => void;
} {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  return {
    body,
    push: (text) => controller.enqueue(encoder.encode(text)),
    close: () => controller.close(),
  };
}

/**
 * Run every pending microtask.
 *
 * The reader is a chain of awaits and nothing in its happy path is scheduled
 * on a timer, so there is nothing to sleep for — draining the microtask queue
 * is the whole of "let it get on with it", and it is exact rather than
 * probabilistic.
 */
async function settle(): Promise<void> {
  for (let index = 0; index < 20; index += 1) await Promise.resolve();
}

/** The frame the Go handler writes the instant a subscriber connects. */
const SNAPSHOT = 'id: 1\nevent: snapshot\ndata: {"events":[],"truncated":false,"oldest_seq":1}\n\n';

/**
 * A reader wired up the way useJobFeed wires one: statuses and errors folded
 * into a TransportState, because "what does the chip say" is the assertion and
 * the chip reads the transport, not the status.
 */
function harness(idleTimeoutMs = 40_000) {
  const sources: ReturnType<typeof sseSource>[] = [];
  const urls: string[] = [];
  const statuses: StreamStatus[] = [];
  const errors: unknown[] = [];
  let transport: TransportState = TRANSPORT_CONNECTING;

  const signals: AbortSignal[] = [];

  const fetchStub = vi.fn(async (url: string, init: RequestInit) => {
    urls.push(url);
    // Kept so a test can assert the socket was torn down. A stubbed body has
    // no connection to cancel, so the signal is the only observable the reader
    // actually controls — and in production it is the whole mechanism: it is
    // what reaches r.Context().Done() on the Go side.
    if (init.signal) signals.push(init.signal);
    const source = sseSource();
    sources.push(source);
    return { ok: true, status: 200, body: source.body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetchStub);

  const handle = openJobStream({
    jobId: "job_01",
    idleTimeoutMs,
    onStatus: (status) => {
      statuses.push(status);
      transport = transportFromStream(status, transport);
    },
    onError: (error) => {
      errors.push(error);
      const { reason, permanent } = streamFailureReason(error);
      transport = { mode: transport.mode === "final" ? "final" : "polling", reason, permanent };
    },
  });

  return {
    handle,
    sources,
    urls,
    statuses,
    errors,
    signals,
    fetchStub,
    transport: () => transport,
    /** What the fallback poll would be doing right now, for a live job. */
    pollMs: () => eventsPollMs(transport.mode, false),
  };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("openJobStream: a connection that opens and then goes silent", () => {
  it("does not stay live for ever, and turns the fallback poll back on", async () => {
    const h = harness();
    await settle();

    // A real, working stream: the snapshot frame arrives and the chip earns
    // the word.
    h.sources[0].push(SNAPSHOT);
    await settle();
    expect(h.transport().mode).toBe("live");
    // And the poll is off, which is what makes the next part dangerous.
    expect(h.pollMs()).toBe(false);

    // Now the far end goes away without saying so. No FIN, no error, no
    // heartbeat — the socket is simply never written to again.
    await vi.advanceTimersByTimeAsync(39_000);
    expect(h.transport().mode).toBe("live");

    await vi.advanceTimersByTimeAsync(2_000);
    await settle();

    // THE ASSERTION. Before the idle deadline existed this read "live" until
    // the tab was closed.
    expect(h.transport().mode).not.toBe("live");
    expect(h.transport().mode).toBe("polling");
    // And the poller is carrying the screen again, which is the part the
    // reader actually depends on.
    expect(h.pollMs()).toBe(1000);
    expect(h.transport().reason).toBeTruthy();

    // The failure is named rather than generic: "the stream stopped sending"
    // is a diagnosis an operator cannot reach any other way.
    expect(h.errors).toHaveLength(1);
    expect((h.errors[0] as Error).name).toBe("StreamIdleError");
    expect((h.errors[0] as Error).message).toContain("not even a heartbeat");

    h.handle.close();
  });

  it("reopens from the cursor rather than re-reading the history", async () => {
    const h = harness();
    await settle();
    h.sources[0].push('id: 7\nevent: event\ndata: {"seq":7,"type":"STEP_STARTED"}\n\n');
    await settle();
    expect(h.urls[0]).toContain("after=0");

    await vi.advanceTimersByTimeAsync(41_000);
    await settle();
    // The backoff, then the reconnect. maxDelayMs bounds it; base is 500 and
    // the jitter is uniform below the ceiling, so a full second covers it.
    await vi.advanceTimersByTimeAsync(1_000);
    await settle();

    expect(h.urls).toHaveLength(2);
    expect(h.urls[1]).toContain("after=7");
    h.handle.close();
  });

  it("aborts the wedged connection instead of leaving the server holding it", async () => {
    // A stream handler's only cancellation signal on the Go side is
    // r.Context().Done(). A reader that walked away from a dead socket without
    // aborting it would leak a subscriber per reconnect, and the Go side would
    // hold each one until its own absolute write deadline collected it.
    const h = harness();
    await settle();
    h.sources[0].push(SNAPSHOT);
    await settle();
    expect(h.signals[0].aborted).toBe(false);

    await vi.advanceTimersByTimeAsync(41_000);
    await settle();
    expect(h.signals[0].aborted).toBe(true);

    // And the reconnect gets its OWN signal: a shared one would mean the first
    // idle timeout permanently poisoned every connection after it.
    await vi.advanceTimersByTimeAsync(1_000);
    await settle();
    expect(h.signals).toHaveLength(2);
    expect(h.signals[1].aborted).toBe(false);

    h.handle.close();
    expect(h.signals[1].aborted).toBe(true);
  });
});

describe("openJobStream: what `live` is allowed to mean", () => {
  it("does not claim to be open before a frame has been read", async () => {
    const h = harness();
    await settle();

    // The response headers have arrived and `response.ok` is true. That is
    // the exact moment the old reader said "open" and switched the poll off,
    // and it is a claim about the connection rather than about the data.
    expect(h.fetchStub).toHaveBeenCalledTimes(1);
    expect(h.statuses).toEqual(["connecting"]);
    expect(h.transport().mode).toBe("connecting");
    expect(h.pollMs()).toBe(1000);

    h.sources[0].push(SNAPSHOT);
    await settle();
    expect(h.statuses).toEqual(["connecting", "open"]);
    expect(h.transport().mode).toBe("live");

    h.handle.close();
  });

  it("counts the bare `: ping` heartbeat as progress, though the parser drops it", async () => {
    // The comment carries no fields, so parseFrame returns null and no handler
    // ever hears about it. If the deadline were armed on frames rather than on
    // bytes, a healthy stream over a job that simply has nothing to say would
    // be torn down every forty seconds for ever.
    const h = harness();
    await settle();
    h.sources[0].push(SNAPSHOT);
    await settle();

    for (let tick = 0; tick < 8; tick += 1) {
      await vi.advanceTimersByTimeAsync(15_000);
      h.sources[0].push(": ping\n\n");
      await settle();
    }

    // Two minutes of an idle job on a perfectly good connection.
    expect(h.errors).toEqual([]);
    expect(h.transport().mode).toBe("live");
    expect(h.fetchStub).toHaveBeenCalledTimes(1);

    h.handle.close();
  });

  it("keeps a slow job live: the deadline measures the gap, not the age", async () => {
    const h = harness();
    await settle();
    h.sources[0].push(SNAPSHOT);
    await settle();

    for (let tick = 0; tick < 5; tick += 1) {
      await vi.advanceTimersByTimeAsync(30_000);
      h.sources[0].push(`id: ${tick + 2}\nevent: event\ndata: {"seq":${tick + 2}}\n\n`);
      await settle();
    }

    expect(h.errors).toEqual([]);
    expect(h.transport().mode).toBe("live");
    h.handle.close();
  });
});
