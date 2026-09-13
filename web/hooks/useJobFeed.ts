"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiConditional, apiJson } from "@/lib/api";
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
  type HistoryPage,
  type TransportState,
} from "@/lib/feed";
import { openJobStream } from "@/lib/stream";
import { TERMINAL } from "@/lib/state";
import type { EventsResponse, JobResponse, RunMeshEvent } from "@/lib/types";

/**
 * Everything the job detail screen is fed from: the snapshot, the timeline, the
 * live stream, the fallback poll, and an honest account of which of those is
 * currently carrying it.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS IS ONE HOOK AND NOT THREE.
 *
 * The three sources are not independent, they are a cycle. The stream's status
 * sets the poll intervals (`jobPollMs`, `eventsPollMs`); the job's state
 * decides whether anything should be polled at all; and the job's state only
 * arrives through the very query whose interval is in question. Split across
 * `useJob`, `useJobEvents` and `useJobStream`, the caller has to thread one
 * hook's output into the next hook's input in an order that JavaScript's
 * declaration order does not permit, and the usual fix — a render's worth of
 * lag between the transport dropping and the poll speeding up — is a second of
 * blankness at exactly the moment the screen degraded.
 *
 * So the cycle is closed inside one hook, which is also the only place that can
 * honestly answer the question the chrome asks: how is this screen being fed
 * right now.
 * ---------------------------------------------------------------------------
 *
 * THE STREAM AND THE POLL ARE NOT ALTERNATIVES. The stream carries EVENTS. It
 * does not carry the job row, and the job row holds three things the events do
 * not: `blocked_by`, which is derived per request and stored nowhere, `result`,
 * which is the tool's output, and the rolled-up job state. So the snapshot poll
 * never goes away even when the stream is healthy; it slows from one second to
 * five. Saying that plainly beats pretending a live stream removed it.
 */

export interface JobFeed {
  job?: JobResponse;
  /** The accumulated timeline, ascending by the per-job `seq`. */
  events: RunMeshEvent[];
  /**
   * History before `oldestSeq` is permanently gone. LATCHED across pages —
   * see HistoryPage in lib/feed.ts: the wire flag describes one request's
   * cursor, this describes the accumulated list, and the two diverge on the
   * poll immediately after the cursor clears the eviction point.
   */
  truncated: boolean;
  oldestSeq: number;
  /** How the screen is being fed, and why, if it is not the stream. */
  transport: TransportState;
  /** The job has settled. Polling has stopped and the chip reads FINAL. */
  terminal: boolean;
  /** Epoch ms of the most recent successful response from either source. */
  updatedAt: number | null;
  /** The snapshot's failure. A timeline failure alone must not blank the page. */
  error: unknown;
  isLoading: boolean;
  refetch: () => void;
}

/** The events page size. The endpoint's ceiling is 1000 and a catch-up read of
 *  a heavily retried job is the case worth one request instead of five. */
const EVENTS_PAGE_LIMIT = 1000;

/** The settled job's transport, as a constant so the derivation below cannot
 *  allocate a new object on every render and re-run every memo that reads it. */
const TRANSPORT_FINAL: TransportState = { mode: "final", permanent: false };

export function useJobFeed(jobId: string): JobFeed {
  const queryClient = useQueryClient();

  const [events, setEvents] = useState<RunMeshEvent[]>([]);
  const [page, setPage] = useState<HistoryPage>(HISTORY_PAGE_FRESH);
  const [streamTransport, setTransport] = useState<TransportState>(TRANSPORT_CONNECTING);

  // A mirror of `events` that the poller's cursor can be read from without
  // capturing a stale closure. The state is the source of truth; this only
  // ever follows it, one commit behind at most, and the queryFn that reads it
  // runs after the commit that wrote it.
  const eventsRef = useRef<RunMeshEvent[]>(events);
  useEffect(() => {
    eventsRef.current = events;
  }, [events]);

  /**
   * The accumulated list belongs to ONE job, so a change of id discards it.
   *
   * Adjusted during render rather than in an effect, which is React's own
   * documented answer for "reset state when a prop changes" and the only one
   * that is correct here. The alternatives both fail visibly: an effect that
   * calls setEvents is a cascading render — the same pattern this file already
   * refuses for the transport chip — and it commits one frame of the PREVIOUS
   * job's timeline under the new job's snapshot first, which draws attempts on
   * lanes that never ran them. Doing nothing at all, which is what happened
   * before, is worse again: the stream effect below reopens with
   * `cursorOf(eventsRef.current)`, so the new job's stream would resume from
   * the old job's high-water seq and silently skip everything before it.
   *
   * The MIRROR is cleared by the sync effect above and not here, because a ref
   * may not be written during render — React's own lint rule rejects it, and
   * the rule is right that a render-phase mutation is invisible to the commit.
   * Declaration order carries it instead, and that is not luck: effects in one
   * commit fire in the order they are declared, so the sync effect runs before
   * the stream effect below in the very commit this reset produces, and the
   * cursor the stream reads is already 0. Both readers of the ref — that
   * effect and the events queryFn — run after this commit, never inside it.
   */
  const [feeding, setFeeding] = useState(jobId);
  if (feeding !== jobId) {
    setFeeding(jobId);
    setEvents([]);
    setPage(HISTORY_PAGE_FRESH);
  }

  /**
   * The single entry point for both producers. `mergeEvents` deduplicates on
   * `seq` and returns the SAME array when nothing is new, so React bails out of
   * the update and `buildLanes` — the most expensive function on the page — is
   * not re-run once a second for a job where nothing is happening.
   */
  const ingest = useCallback((incoming: readonly RunMeshEvent[]) => {
    setEvents((current) => mergeEvents(current, incoming));
  }, []);

  /* ------------------------------------------------------------- the job */

  const jobKey = ["job", jobId] as const;
  const etagRef = useRef<string | null>(null);

  const jobQuery = useQuery({
    queryKey: jobKey,
    queryFn: async (): Promise<JobResponse> => {
      const previous = queryClient.getQueryData<JobResponse>(jobKey);

      // If-None-Match is sent even though the API does not yet compare it:
      // getJob sets ETag and nothing reads the request header, so a 304 cannot
      // currently happen. The branch is here because it is CORRECT — the ETag
      // is the job Version and exists for this — and the day getJob grows the
      // comparison, the saving arrives without a client change.
      const conditional = await apiConditional<JobResponse>(
        `/jobs/${jobId}`,
        previous ? etagRef.current : null,
      );
      etagRef.current = conditional.etag;

      if (conditional.data === undefined) {
        if (previous) return previous;
        // A 304 with nothing cached should be impossible — the header is only
        // sent when there is something to match — but answering it by throwing
        // would strand the screen, so drop the validator and ask again.
        etagRef.current = null;
        return apiJson<JobResponse>(`/jobs/${jobId}`);
      }

      // The saving that IS available today. A poll of an idle job returns a
      // byte-identical row; handing back the previous OBJECT rather than the
      // newly parsed one keeps reference equality, and every memo on this
      // screen — buildLanes, depthBands, laneSegments — is keyed on it.
      if (previous && previous.version === conditional.data.version) return previous;
      return conditional.data;
    },
    refetchInterval: () => {
      // Read from the cache rather than from a closed-over `job`: the interval
      // callback outlives the render that created it, and a stale capture here
      // would keep polling a job that settled a minute ago.
      const current = queryClient.getQueryData<JobResponse>(jobKey);
      return jobPollMs(streamTransport.mode, !!current && TERMINAL.has(current.state));
    },
  });

  const job = jobQuery.data;
  const terminal = !!job && TERMINAL.has(job.state);

  /**
   * A settled job is `final` by DERIVATION, not by an effect that writes it.
   *
   * The obvious implementation notices `terminal` in the stream effect and
   * calls setTransport, which is a cascading render — React's own lint rule
   * rejects it — and, worse, it makes the chip's correctness depend on an
   * effect having run. The job's state already says everything there is to
   * know: a job that has reached an absorbing state emits nothing further, so
   * "how is this screen being fed" has exactly one answer and it is a function
   * of data already in hand.
   */
  const transport: TransportState = terminal ? TRANSPORT_FINAL : streamTransport;

  /* -------------------------------------------------------- the timeline */

  const eventsQuery = useQuery({
    queryKey: ["job", jobId, "events"],
    queryFn: async () => {
      const response = await apiJson<EventsResponse>(`/jobs/${jobId}/events`, {
        // `?after=` is exclusive, so the highest seq held asks for everything
        // strictly after it. Derived from the list rather than tracked
        // separately: two numbers that must agree eventually will not.
        query: { after: cursorOf(eventsRef.current), limit: EVENTS_PAGE_LIMIT },
      });
      ingest(response.events);
      // LATCHED, not assigned. `truncated` describes this request's cursor
      // against the ring; the banner describes the accumulated list, and the
      // cursor moves past the eviction point on the very next poll. Assigning
      // it meant the incomplete-history warning deleted itself one second
      // after appearing, on a job whose history is permanently incomplete.
      setPage((current) =>
        latchHistoryPage(current, {
          truncated: response.truncated,
          oldestSeq: response.oldest_seq,
        }),
      );
      return response;
    },
    refetchInterval: () => eventsPollMs(transport.mode, terminal),
  });

  /* ----------------------------------------------------------- the stream */

  useEffect(() => {
    // A settled job emits nothing further, and a connection held open against
    // one costs the runtime a subscriber for as long as the tab is open. The
    // chip is not written here — `transport` above derives `final` from the
    // job's own state, so nothing depends on this effect having run.
    if (terminal) return;

    const handle = openJobStream({
      jobId,
      // Resume from what is already held, so a remount does not re-read a
      // history the reducer would then have to deduplicate.
      after: cursorOf(eventsRef.current),

      onSnapshot: (snapshotEvents, frame) => {
        ingest(snapshotEvents);

        // The snapshot frame carries the JOB as well as the page, and that is
        // the point of it: the alternative is "GET the job, then subscribe,
        // and hope nothing happened in between", and nothing about that gap is
        // observable to the client that fell into it. Seeding the cache from
        // the frame closes the race rather than documenting it.
        try {
          const payload = JSON.parse(frame.data) as {
            job?: JobResponse;
            truncated?: boolean;
            oldest_seq?: number;
          };
          if (payload.job && payload.job.id === jobId) {
            queryClient.setQueryData(jobKey, payload.job);
            // Keep the validator in step with the row it validates, or the
            // next poll would send an ETag for a version the cache no longer
            // holds.
            etagRef.current = String(payload.job.version);
          }
          if (payload.truncated !== undefined || payload.oldest_seq !== undefined) {
            // Through the same latch as the poll. A reconnect's snapshot is
            // read from a cursor that has already advanced past the hole, so
            // it is the likeliest single source of a `truncated: false` that
            // would otherwise have cleared a warning it knows nothing about.
            setPage((current) =>
              latchHistoryPage(current, {
                truncated: payload.truncated,
                oldestSeq: payload.oldest_seq,
              }),
            );
          }
        } catch {
          // A snapshot whose envelope this build cannot read is still a
          // snapshot: the events were already ingested above, and the job poll
          // is running regardless. There is nothing to recover and nothing to
          // report.
        }
      },

      onEvent: (event) => ingest([event]),

      onResync: () => {
        // The server dropped this subscriber's events, so the accumulated list
        // has a hole that appending can never fill. Discarding it and letting
        // both readers start again is the only answer that does not draw a
        // plausible, wrong timeline.
        setEvents([]);
        eventsRef.current = [];
        // The latch resets HERE and in exactly one other place, both of which
        // are "the accumulated list no longer exists". The warning describes
        // that list; a list that has been discarded has no hole yet, and the
        // refetch below will report a fresh one if there is one.
        setPage(HISTORY_PAGE_FRESH);
        void eventsQuery.refetch();
      },

      onEnd: () => setTransport({ mode: "final", permanent: false }),

      // A drain is not a failure: the reader reconnects on its own, and the
      // status callback below has already moved the chip to `polling`.
      onBye: () => {},

      onError: (error) => {
        const { reason, permanent } = streamFailureReason(error);
        setTransport((previous) => ({
          mode: previous.mode === "final" ? "final" : "polling",
          reason,
          permanent,
        }));
      },

      onStatus: (status) => setTransport((previous) => transportFromStream(status, previous)),
    });

    return () => handle.close();
    // `events` is deliberately absent: the cursor is read through the ref at
    // open time, and depending on the list would tear the stream down and
    // rebuild it on every arriving frame.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId, terminal, ingest, queryClient]);

  /* ------------------------------------------------------------ freshness */

  // The later of the two, because either arriving means the screen is current.
  // Taking only the job's would make a healthy stream look stale at the moment
  // the snapshot poll slowed down BECAUSE the stream was healthy.
  const updatedAt =
    Math.max(jobQuery.dataUpdatedAt, eventsQuery.dataUpdatedAt) || null;

  const refetch = useCallback(() => {
    void jobQuery.refetch();
    void eventsQuery.refetch();
  }, [jobQuery, eventsQuery]);

  return {
    job,
    events,
    truncated: page.truncated,
    oldestSeq: page.oldestSeq,
    transport,
    terminal,
    updatedAt,
    // Only the snapshot's failure. A timeline that 500s while the job row is
    // fine should cost the reader the timeline, not the screen.
    error: jobQuery.error,
    isLoading: jobQuery.isPending,
    refetch,
  };
}
