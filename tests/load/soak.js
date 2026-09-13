// A soak: modest, constant load held long enough for a leak to show.
//
// THE QUESTION IS NOT THROUGHPUT. It is whether anything drifts. The two other
// scripts answer "how fast" over half a minute, which is a window in which a
// goroutine leak, an unbounded map, a connection pool that never returns a
// connection and a subscriber channel that is never closed all look exactly
// like a healthy server. This one runs at a rate the server is comfortably
// inside and watches the numbers that should be FLAT.
//
// WHAT IT WATCHES, AND WHY THOSE. RunMesh exports its own state at
// GET /api/v1/metrics, so a soak does not have to infer health from response
// times. A sampler VU scrapes it once every few seconds and turns four gauges
// into k6 trends:
//
//   go_goroutines            — the engine's package doc fixes the census at
//                              2 + Workers plus one per in-flight step. A
//                              number that climbs is a goroutine per request
//                              or per stream that nobody is stopping.
//   runmesh_queue_depth      — claimable steps. Flat means the pool keeps up;
//                              a ramp means work is arriving faster than it is
//                              being finished and every other number in the run
//                              is about to become meaningless.
//   runmesh_store_events_dropped_total
//                            — the fan-out drops rather than blocks, by design,
//                              and the drop counter is the only signal. In a
//                              soak with no dashboard attached it should never
//                              move at all.
//   go_memory_heap_objects   — the coarsest leak detector there is, and still
//                              the one that catches an unbounded map.
//
// The gauges are recorded as trends rather than asserted per sample, because a
// single spike is not a leak. The thresholds below are on the MAXIMUM, which is
// the shape that catches monotonic growth without failing on one busy moment.
//
// WHY THE RATE IS LOW BY DEFAULT. A soak at the throughput script's peak would
// fill the queue and then measure admission control for an hour. The default
// here is deliberately inside what a laptop sustains; the point is duration,
// not pressure.

import { sleep } from 'k6';
import { Trend } from 'k6/metrics';
import http from 'k6/http';
import {
  BASE_URL,
  preflight,
  submitJob,
  get,
  headers,
  intEnv,
} from './lib/runmesh.js';

const RATE = intEnv('RUNMESH_LOAD_SOAK_RATE', 20);
const DURATION = __ENV.RUNMESH_LOAD_SOAK_DURATION || '10m';
const SAMPLE_EVERY = intEnv('RUNMESH_LOAD_SAMPLE_SECONDS', 5);

const goroutines = new Trend('runmesh_soak_goroutines');
const queueDepth = new Trend('runmesh_soak_queue_depth');
const heapInUse = new Trend('runmesh_soak_heap_bytes');
const eventDrops = new Trend('runmesh_soak_event_drops');

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(10, RATE),
      maxVUs: Math.max(20, RATE * 3),
      exec: 'steady',
    },
    // One VU, doing nothing but scraping. Separate from the load scenario so a
    // slow scrape cannot consume an iteration the load scenario was scheduled
    // to spend, and so the sample interval stays regular however busy the
    // server gets.
    sampler: {
      executor: 'constant-vus',
      vus: 1,
      duration: DURATION,
      exec: 'sample',
    },
  },

  thresholds: {
    http_req_failed: [{ threshold: 'rate<0.01', abortOnFail: true }],
    checks: [{ threshold: 'rate>0.99', abortOnFail: true }],

    // Latency over a long run, with a tighter bar than the ramp scripts: this
    // rate is meant to be comfortable, so a p99 out here is drift rather than
    // load.
    runmesh_submit_duration: [{ threshold: 'p(99)<1000', abortOnFail: true }],
    runmesh_read_duration: [{ threshold: 'p(99)<500', abortOnFail: true }],

    // The leak thresholds. These are on max rather than on a percentile,
    // because what is being caught is a number that only ever goes up.
    //
    // The goroutine ceiling is generous on purpose — net/http runs a goroutine
    // per connection and k6 holds many — but it is FINITE, which is the whole
    // property: a server leaking one goroutine per request crosses any finite
    // line eventually, and a soak is exactly the run long enough for it to.
    runmesh_soak_goroutines: [{ threshold: 'max<2000', abortOnFail: false }],

    // The fan-out drops silently by design; the counter is the only evidence.
    // Nothing in this script subscribes to the event stream, so a non-zero
    // value means something inside the process is a slow subscriber.
    runmesh_soak_event_drops: [{ threshold: 'max<1', abortOnFail: false }],

    dropped_iterations: [{ threshold: 'count<100', abortOnFail: true }],
  },

  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max', 'count'],
};

export function setup() {
  return preflight();
}

export function steady() {
  const id = submitJob(`load-soak-${__VU}-${__ITER}`);
  if (id) {
    // Read back what was written. A soak that only writes never exercises the
    // read path's allocations, and the read path is where a per-request cache
    // that never evicts would live.
    get(`/api/v1/jobs/${id}`, 'GET /api/v1/jobs/{id}');
  }
}

export function sample() {
  const res = http.get(`${BASE_URL}/api/v1/metrics`, {
    headers: headers(),
    tags: { endpoint: 'GET /api/v1/metrics' },
  });

  if (res.status === 200) {
    const body = res.body;
    record(body, 'go_goroutines', goroutines);
    record(body, 'runmesh_queue_depth', queueDepth);
    record(body, 'go_memory_heap_objects_bytes', heapInUse);
    record(body, 'runmesh_store_events_dropped_total', eventDrops);
  } else if (res.status === 403) {
    // The scrape endpoint carries its own scope. Say so once, clearly, rather
    // than reporting a soak with every gauge silently missing — a soak whose
    // leak detectors never ran is a soak that proves nothing.
    throw new Error(
      'GET /api/v1/metrics answered 403: the key in RUNMESH_API_KEY needs the ' +
      'metrics.read scope (RUNMESH_API_KEYS=load:jobs.read+jobs.write+metrics.read=<key>)',
    );
  }

  sleep(SAMPLE_EVERY);
}

// record pulls one unlabelled sample out of the Prometheus text exposition.
//
// A line-scan rather than a parser, and the narrowness is deliberate: it only
// handles a bare `name value` line with no labels, which is exactly what the
// four gauges above are. Anything cleverer would be a second implementation of
// a format internal/metrics already has a real parser test for, living in a
// load script where its bugs would be attributed to the server.
function record(body, name, trend) {
  const lines = body.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.length === 0 || line.charAt(0) === '#') continue;
    const sp = line.indexOf(' ');
    if (sp < 0) continue;
    if (line.substring(0, sp) !== name) continue;
    const v = parseFloat(line.substring(sp + 1));
    if (!Number.isNaN(v)) trend.add(v);
    return;
  }
}
