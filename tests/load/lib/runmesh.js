// Shared helpers for every k6 script in this directory.
//
// It exists for one reason that is not "avoid repetition": the THRESHOLDS and
// the plan shapes have to be the same across the three scripts, or the numbers
// they produce cannot be compared with each other. A soak that submits a
// different DAG from the throughput run is a soak that answers a different
// question, and nobody notices until the two disagree.
//
// Everything configurable is an environment variable, because that is how the
// rest of RunMesh is configured and because a load script with a hard-coded
// host is a load script that gets edited before every run and committed with
// somebody's laptop in it.

import http from 'k6/http';
import { check, fail } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// ─── configuration ───────────────────────────────────────────────────────────

// RUNMESH_URL is accepted as well as RUNMESH_BASE_URL because the Makefile's
// `load` target documents the shorter spelling, and a script whose variable
// name disagrees with the target that runs it is a script that fails with a
// connection refused to the default host.
export const BASE_URL = (__ENV.RUNMESH_BASE_URL || __ENV.RUNMESH_URL || 'http://127.0.0.1:8080')
  .replace(/\/+$/, '');
export const API_KEY = __ENV.RUNMESH_API_KEY || '';

// TOOL is which builtin the submitted steps run. `echo` returns immediately and
// makes the measurement about the API and the store; `sleep` holds a worker and
// makes it about the pool. The default is echo, because a load test whose
// bottleneck is a tool it chose is not measuring the server.
export const TOOL = __ENV.RUNMESH_LOAD_TOOL || 'echo';
export const STEP_PARAMS = TOOL === 'sleep'
  ? { duration: __ENV.RUNMESH_LOAD_SLEEP || '20ms' }
  : { msg: 'load' };

export function intEnv(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') return fallback;
  const n = parseInt(raw, 10);
  if (Number.isNaN(n) || n < 1) {
    fail(`${name} must be a positive integer, got ${JSON.stringify(raw)}`);
  }
  return n;
}

// ─── custom metrics ──────────────────────────────────────────────────────────
//
// These exist because k6's built-in http_req_failed counts any non-2xx as a
// failure, and for this API that is wrong in one specific and important way:
// a 429 from admission control is the server WORKING. RUNMESH_MAX_QUEUE_DEPTH
// is a deliberate ceiling, and a runtime that pushes back instead of accepting
// unbounded work is the behaviour these scripts are partly here to prove. So
// backpressure gets its own counter, is excluded from the error rate, and gets
// its own threshold from the other direction — a run in which EVERY submission
// was refused has not measured throughput, it has measured a full queue, and
// that must fail too.

export const submitted = new Counter('runmesh_jobs_submitted');
export const backpressure = new Rate('runmesh_backpressure_rate');
export const submitLatency = new Trend('runmesh_submit_duration', true);
export const readLatency = new Trend('runmesh_read_duration', true);
export const apiErrors = new Counter('runmesh_api_errors');

// ─── requests ────────────────────────────────────────────────────────────────

export function headers(extra) {
  return Object.assign(
    {
      Authorization: `Bearer ${API_KEY}`,
      'Content-Type': 'application/json',
    },
    extra || {},
  );
}

// diamond is the shape the repository's own end-to-end test submits: a fan-out
// joined by a report. It is used rather than a flat list of independent steps
// because a flat plan never exercises the dependency predicate, and the
// readiness predicate is evaluated on every claim and on every readiness probe.
export function diamond(name) {
  return {
    name: name,
    steps: [
      { id: 'fetch', tool: TOOL, params: STEP_PARAMS },
      { id: 'a', tool: TOOL, params: STEP_PARAMS, depends_on: ['fetch'] },
      { id: 'b', tool: TOOL, params: STEP_PARAMS, depends_on: ['fetch'] },
      { id: 'report', tool: TOOL, params: STEP_PARAMS, depends_on: ['a', 'b'] },
    ],
  };
}

// submitJob posts one plan and classifies the answer into three outcomes rather
// than two: accepted, refused by admission control, or broken. Only the third
// is an error.
//
// Returns the job id on acceptance and null otherwise, so a caller that wants
// to read back what it wrote can tell whether there is anything to read.
export function submitJob(name) {
  const res = http.post(`${BASE_URL}/api/v1/jobs`, JSON.stringify(diamond(name)), {
    headers: headers(),
    tags: { endpoint: 'POST /api/v1/jobs' },
  });

  submitLatency.add(res.timings.duration);

  if (res.status === 429) {
    backpressure.add(true);
    // A 429 must still be a well-formed envelope naming the reason. A server
    // that refuses work without saying why is as much a bug as one that
    // accepts too much.
    check(res, {
      'backpressure uses the error envelope': (r) => {
        const body = safeJSON(r);
        return body && body.error && body.error.code === 'resource_exhausted';
      },
    });
    return null;
  }

  backpressure.add(false);

  const ok = check(res, {
    'submit answered 201': (r) => r.status === 201,
    'submit returned a job id': (r) => {
      const body = safeJSON(r);
      return !!(body && typeof body.id === 'string' && body.id.length > 0);
    },
  });
  if (!ok) {
    apiErrors.add(1, { endpoint: 'POST /api/v1/jobs', status: String(res.status) });
    return null;
  }

  submitted.add(1);
  return safeJSON(res).id;
}

// get performs a read and records it under the shared read trend, so the
// mixed script's read latency is comparable with the soak's.
export function get(path, endpoint) {
  const res = http.get(`${BASE_URL}${path}`, {
    headers: headers(),
    tags: { endpoint: endpoint },
  });
  readLatency.add(res.timings.duration);
  const ok = check(res, {
    [`${endpoint} answered 200`]: (r) => r.status === 200,
  });
  if (!ok) {
    apiErrors.add(1, { endpoint: endpoint, status: String(res.status) });
  }
  return res;
}

// safeJSON never throws. A body that is not JSON is itself the finding, and a
// script that dies on it loses the whole run's results along with the evidence.
export function safeJSON(res) {
  try {
    return res.json();
  } catch (e) {
    return null;
  }
}

// ─── preflight ───────────────────────────────────────────────────────────────

// preflight runs once, in setup(), before any VU starts.
//
// It fails the whole run on a missing key or an unreachable server rather than
// letting every VU discover it independently. The distinction matters: without
// it, a missing RUNMESH_API_KEY produces a full run of 401s, a green
// http_req_failed threshold only if somebody forgot to set one, and a summary
// that looks like a very fast server.
export function preflight() {
  if (!API_KEY) {
    fail(
      'RUNMESH_API_KEY is not set. RunMesh has no default key — ' +
      'start the server with RUNMESH_API_KEYS=load=<key> and pass the same value here.',
    );
  }

  const ready = http.get(`${BASE_URL}/api/v1/ready`, { tags: { endpoint: 'GET /api/v1/ready' } });
  if (ready.status !== 200) {
    fail(`${BASE_URL}/api/v1/ready answered ${ready.status}; the server is not ready to be loaded`);
  }

  // One authenticated read, so an unusable credential fails here rather than
  // in every iteration. GET /api/v1/tools needs jobs.read, which every script
  // in this directory needs anyway.
  const tools = http.get(`${BASE_URL}/api/v1/tools`, {
    headers: headers(),
    tags: { endpoint: 'GET /api/v1/tools' },
  });
  if (tools.status !== 200) {
    fail(
      `GET /api/v1/tools answered ${tools.status}: the key in RUNMESH_API_KEY ` +
      'is missing, wrong, or lacks the jobs.read scope',
    );
  }

  const names = (safeJSON(tools) || { tools: [] }).tools.map((t) => t.name);
  if (names.indexOf(TOOL) < 0) {
    fail(`the server does not offer the tool ${TOOL}; it offers ${JSON.stringify(names)}`);
  }

  return { startedAt: new Date().toISOString() };
}
