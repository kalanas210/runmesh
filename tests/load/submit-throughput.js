// Job-submission throughput.
//
// THE QUESTION. How many plans a second can this process admit, and what does
// the latency of POST /api/v1/jobs look like while it is admitting them? That
// path is the only one in the API that does real work before it answers: an
// admission-control queue-depth query, the full plan validation, id minting,
// the Build, and a CreateJob that writes a job, its steps and its first events
// in one transaction. Every other endpoint is a read.
//
// WHY IT RAMPS. A fixed VU count answers "what happens at N" and nothing else.
// The interesting number is where the latency curve bends, so this walks the
// arrival rate up in stages with a constant-arrival-rate executor, which sends
// requests at a RATE rather than as fast as the responses come back. That
// distinction is the whole point: a closed-loop VU loop cannot overload a
// server, because it slows down exactly as much as the server does and reports
// a flattering latency the whole way. An open-loop arrival rate can, and
// dropped iterations are then a real signal rather than a measurement artefact.
//
// WHAT MAKES IT FAIL. Thresholds, not eyeballs. A script that prints numbers is
// a script whose regression nobody notices, so every claim this file makes is
// an abortOnFail threshold: the error rate, the submission latency at p95 and
// p99, and — from the other side — the backpressure rate, because a run in
// which the queue was full throughout measured admission control rather than
// throughput and must not be read as either.

import { sleep } from 'k6';
import {
  preflight,
  submitJob,
  intEnv,
} from './lib/runmesh.js';

const RATE = intEnv('RUNMESH_LOAD_RATE', 200); // submissions per second at the peak
const VUS = intEnv('RUNMESH_LOAD_VUS', 100);
const STAGE = __ENV.RUNMESH_LOAD_STAGE || '30s';

export const options = {
  scenarios: {
    submit: {
      executor: 'ramping-arrival-rate',
      startRate: Math.max(1, Math.floor(RATE / 8)),
      timeUnit: '1s',
      // preAllocatedVUs has to cover the peak concurrency, or k6 spends the
      // run allocating VUs and the arrival rate it reports is not the arrival
      // rate it achieved.
      preAllocatedVUs: VUS,
      maxVUs: VUS * 2,
      stages: [
        { target: Math.max(1, Math.floor(RATE / 4)), duration: STAGE },
        { target: Math.max(1, Math.floor(RATE / 2)), duration: STAGE },
        { target: RATE, duration: STAGE },
        { target: RATE, duration: STAGE },
      ],
    },
  },

  thresholds: {
    // A 4xx that is not a 429 and any 5xx at all. http_req_failed counts
    // non-2xx, and the 429s admission control returns are the server working,
    // so this is deliberately loose and runmesh_backpressure_rate below is
    // where the refusals are actually judged.
    http_req_failed: [{ threshold: 'rate<0.02', abortOnFail: true }],

    // The API's own promise. Submission is a write with a store round trip in
    // it; anything past a quarter of a second means the admission query, the
    // validation or the insert has stopped being cheap.
    runmesh_submit_duration: [
      { threshold: 'p(95)<250', abortOnFail: true },
      { threshold: 'p(99)<1000', abortOnFail: true },
    ],

    // Every check in lib/runmesh.js is an assertion about the wire contract —
    // the status, the job id, the shape of a refusal. One failing is a broken
    // response, not a slow one.
    checks: [{ threshold: 'rate>0.99', abortOnFail: true }],

    // Backpressure, judged from both directions. Some refusals under a ramp
    // are legitimate and expected. A run that is mostly refusals measured a
    // full queue: raise RUNMESH_MAX_QUEUE_DEPTH, give the server workers, or
    // lower the rate — but do not read the latency numbers as throughput.
    runmesh_backpressure_rate: [{ threshold: 'rate<0.5', abortOnFail: true }],

    // Dropped iterations are k6 telling you it could not keep to the arrival
    // rate with the VUs it was given. That invalidates the rate axis, so it is
    // a failure of the TEST rather than of the server — and it still has to
    // fail, because a quiet one produces a number labelled 200/s that was
    // never 200/s.
    dropped_iterations: [{ threshold: 'count<100', abortOnFail: true }],
  },

  // Percentiles that match the thresholds above. Without this k6 reports
  // p(90)/p(95) and a p(99) threshold is evaluated against a summary the
  // reader cannot see.
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max', 'count'],
};

export function setup() {
  return preflight();
}

export default function () {
  submitJob(`load-submit-${__VU}-${__ITER}`);
  // No sleep: the arrival-rate executor owns the pacing, and a sleep here
  // would fight it for control of the schedule.
}

export function teardown() {
  // Deliberately empty, and deliberately present. The jobs this script
  // submitted are left in the store: they are the load, and deleting them
  // would need a DELETE endpoint that does not exist and should not exist just
  // so a benchmark can tidy up. Use a fresh database or a fresh memstore per
  // run; docs/benchmarks says the same.
  sleep(0);
}
