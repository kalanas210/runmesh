// A mixed read/write profile against the API.
//
// THE QUESTION. The throughput script measures one endpoint in isolation, which
// is the one thing a real deployment never does. A dashboard polls
// GET /api/v1/jobs/{id} on an ETag, renders a timeline from
// GET /api/v1/jobs/{id}/events, and lists work with GET /api/v1/jobs — all
// while agents submit. Those reads and that write share a store, and on the
// in-memory store they share ONE MUTEX, the same mutex Claim, Finish and
// Heartbeat take. This is the script that can show a read starving a write.
//
// THE SHAPE. Two scenarios running concurrently rather than one scenario with a
// weighted random branch. The difference is not cosmetic: a random branch makes
// the read and write rates covary, so a slowdown on either side hides itself by
// reducing the other's load, and neither threshold fires. Separate scenarios
// with separate arrival rates keep the two independent, and give each its own
// latency threshold — which is what lets this script say WHICH side degraded.
//
// The read scenario reads jobs the write scenario created. Reading a fabricated
// id would exercise the 404 path, which is a different and much cheaper code
// path than fetching a job and rendering its steps; the ids are shared through
// setup()'s return value, which k6 hands to every VU.

import { group } from 'k6';
import {
  preflight,
  submitJob,
  get,
  intEnv,
  safeJSON,
} from './lib/runmesh.js';

const WRITE_RATE = intEnv('RUNMESH_LOAD_WRITE_RATE', 30);
const READ_RATE = intEnv('RUNMESH_LOAD_READ_RATE', 300);
const DURATION = __ENV.RUNMESH_LOAD_DURATION || '1m';
const SEED_JOBS = intEnv('RUNMESH_LOAD_SEED_JOBS', 20);

export const options = {
  scenarios: {
    writers: {
      executor: 'constant-arrival-rate',
      rate: WRITE_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(4, WRITE_RATE),
      maxVUs: Math.max(8, WRITE_RATE * 2),
      exec: 'write',
    },
    readers: {
      executor: 'constant-arrival-rate',
      rate: READ_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(10, Math.floor(READ_RATE / 4)),
      maxVUs: Math.max(20, READ_RATE),
      exec: 'read',
    },
  },

  thresholds: {
    http_req_failed: [{ threshold: 'rate<0.02', abortOnFail: true }],
    checks: [{ threshold: 'rate>0.99', abortOnFail: true }],

    // The two halves are judged separately, which is the entire reason this
    // script has two scenarios. Reads are pure lookups and are held to a much
    // tighter bar than the write, which does admission control and a store
    // transaction before it can answer.
    runmesh_read_duration: [
      { threshold: 'p(95)<100', abortOnFail: true },
      { threshold: 'p(99)<500', abortOnFail: true },
    ],
    runmesh_submit_duration: [
      { threshold: 'p(95)<250', abortOnFail: true },
      { threshold: 'p(99)<1000', abortOnFail: true },
    ],

    // Per-endpoint duration, so a summary says which read got slow rather than
    // that reads got slow. The list endpoint renders every step of every job in
    // the page and is expected to be the expensive one; it gets its own, looser
    // bar rather than dragging the others' threshold up to meet it.
    'http_req_duration{endpoint:GET /api/v1/jobs/{id}}': ['p(95)<100'],
    'http_req_duration{endpoint:GET /api/v1/jobs/{id}/events}': ['p(95)<150'],
    'http_req_duration{endpoint:GET /api/v1/jobs}': ['p(95)<300'],

    runmesh_backpressure_rate: [{ threshold: 'rate<0.5', abortOnFail: true }],
    dropped_iterations: [{ threshold: 'count<100', abortOnFail: true }],
  },

  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max', 'count'],
};

// setup submits a handful of jobs and returns their ids, so the readers have
// something real to read from their very first iteration. Without it the read
// scenario spends its opening seconds on 404s and its p95 is measured against
// a path that does no work.
export function setup() {
  const env = preflight();
  const ids = [];
  for (let i = 0; i < SEED_JOBS; i++) {
    const id = submitJob(`load-mixed-seed-${i}`);
    if (id) ids.push(id);
  }
  if (ids.length === 0) {
    throw new Error('not one seed job was accepted; the readers would have nothing to read');
  }
  return Object.assign(env, { ids: ids });
}

export function write() {
  submitJob(`load-mixed-${__VU}-${__ITER}`);
}

export function read(data) {
  const id = data.ids[__ITER % data.ids.length];

  group('dashboard poll', function () {
    // The three calls a waterfall makes, in the order it makes them. Fetching
    // the job, then its timeline, then the list — the same request pattern the
    // Week-6 dashboard produces, so the ratio between them is realistic rather
    // than uniform.
    get(`/api/v1/jobs/${id}`, 'GET /api/v1/jobs/{id}');
    get(`/api/v1/jobs/${id}/events?limit=200`, 'GET /api/v1/jobs/{id}/events');

    // Only every fourth iteration lists. A dashboard refreshes one job far more
    // often than it re-lists the fleet, and weighting the expensive endpoint
    // equally would make this script mostly a benchmark of ListJobs.
    if (__ITER % 4 === 0) {
      const res = get('/api/v1/jobs?limit=25', 'GET /api/v1/jobs');
      const body = safeJSON(res);
      if (body && body.jobs && body.jobs.length > 0 && !body.jobs[0].steps) {
        // The divergence internal/storetest added a case for in Week 2: a
        // store that returns jobs without their steps renders an empty
        // waterfall. It is cheap to check here and it is the kind of thing a
        // load script is uniquely placed to catch, because it only shows up
        // against the store a load run actually uses.
        throw new Error('GET /api/v1/jobs returned a job with no steps array');
      }
    }
  });
}
