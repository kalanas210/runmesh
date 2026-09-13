// The smoke run: thirty seconds, five virtual users, every endpoint the other
// three scripts touch, once each.
//
// It is `make load`'s default (K6_SCRIPT in the Makefile) and it is deliberately
// not a load test. Its job is to answer "is the thing I am about to spend ten
// minutes loading actually wired up" — the key has the right scopes, the tool
// this profile submits is registered, the submit path returns a job id, the read
// paths return the job back, and the scrape endpoint answers. Every one of those
// is a mistake somebody makes once per machine, and discovering them from a soak
// summary an hour later is the expensive way to find out.
//
// The thresholds are therefore about CORRECTNESS rather than about capacity:
// zero failures and zero failed checks, at a rate no server should notice. A
// latency bar here would only encode how busy the laptop was.

import { sleep } from 'k6';
import { preflight, submitJob, get } from './lib/runmesh.js';

export const options = {
  vus: 5,
  duration: __ENV.RUNMESH_LOAD_SMOKE_DURATION || '30s',

  thresholds: {
    // Nothing may fail. At five VUs there is no capacity story to tell, so any
    // non-2xx that is not deliberate backpressure is a wiring fault.
    http_req_failed: [{ threshold: 'rate==0', abortOnFail: true }],
    checks: [{ threshold: 'rate==1', abortOnFail: true }],
    runmesh_api_errors: [{ threshold: 'count==0', abortOnFail: true }],

    // A generous ceiling that still catches a server which is fundamentally
    // broken — talking to a database that is not there, say — rather than
    // merely busy.
    runmesh_submit_duration: [{ threshold: 'p(99)<2000', abortOnFail: true }],
  },

  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max', 'count'],
};

export function setup() {
  return preflight();
}

export default function () {
  const id = submitJob(`load-smoke-${__VU}-${__ITER}`);
  if (id) {
    get(`/api/v1/jobs/${id}`, 'GET /api/v1/jobs/{id}');
    get(`/api/v1/jobs/${id}/events?limit=200`, 'GET /api/v1/jobs/{id}/events');
  }
  get('/api/v1/jobs?limit=10', 'GET /api/v1/jobs');
  // Paced rather than open-loop: this script is not trying to saturate
  // anything, and an arrival-rate executor here would turn a wiring check into
  // a capacity test nobody asked for.
  sleep(1);
}
