import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET, POST } from "./route";

/**
 * The guard, exercised through the handler rather than through the pure
 * function it delegates to.
 *
 * lib/same-origin.test.ts already proves the decision; this file proves the
 * WIRING, which is the half that actually shipped the vulnerability. The
 * function could have been perfect and unreferenced. So every assertion below
 * is about the upstream `fetch`: whether it happened, and whether it carried
 * the server's key. A 403 with the request already forwarded would be a pass
 * for a test that only read the status code, and it would be no fix at all —
 * the job would have been submitted.
 */

const UPSTREAM = "http://127.0.0.1:8080";
const KEY = "0123456789abcdef0123456789abcdef";
const HOST = "console.example:3000";

/** The route reads `context.params` as a promise, the way Next hands it over. */
function params(path: string[]) {
  return { params: Promise.resolve({ path }) };
}

function post(headers: Record<string, string>): NextRequest {
  return new NextRequest(`http://${HOST}/api/rm/jobs`, {
    method: "POST",
    headers,
    body: JSON.stringify({ goal: "delete everything" }),
  });
}

let upstream: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.stubEnv("RUNMESH_API_URL", UPSTREAM);
  vi.stubEnv("RUNMESH_API_KEY", KEY);
  upstream = vi.fn(
    async () =>
      new Response(JSON.stringify({ id: "job_01" }), {
        status: 201,
        headers: { "content-type": "application/json", location: "/api/v1/jobs/job_01" },
      }),
  );
  vi.stubGlobal("fetch", upstream);
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe("the API proxy as a confused deputy", () => {
  it("refuses a cross-site POST without forwarding it", async () => {
    // The whole attack in one request: a CORS-safelisted content type makes
    // this a SIMPLE request, so no preflight stands between an attacker's page
    // and this handler.
    const response = await POST(
      post({
        "sec-fetch-site": "cross-site",
        origin: "https://evil.example",
        "content-type": "text/plain;charset=UTF-8",
      }),
      params(["jobs"]),
    );

    expect(response.status).toBe(403);
    // The assertion that matters. A refusal that answers 403 after submitting
    // the job has protected nobody.
    expect(upstream).not.toHaveBeenCalled();

    const body = (await response.json()) as { error: { code: string; message: string } };
    expect(body.error.code).toBe("permission_denied");
    expect(body.error.message).toContain("cross-site");
  });

  it("refuses a cross-origin POST from a client that sends no fetch metadata", async () => {
    const response = await POST(
      post({ origin: "https://evil.example" }),
      params(["jobs", "job_01", "cancel"]),
    );
    expect(response.status).toBe(403);
    expect(upstream).not.toHaveBeenCalled();
  });

  it("refuses before reading the environment, so it cannot be a configuration oracle", async () => {
    // An unconfigured deployment answers `misconfigured` with a 500, and that
    // 500 distinguishes "no key here" from "key present". A refused caller
    // must not be able to tell the two apart.
    vi.stubEnv("RUNMESH_API_KEY", "");
    const response = await POST(
      post({ "sec-fetch-site": "cross-site" }),
      params(["jobs"]),
    );
    expect(response.status).toBe(403);
  });

  it("forwards the console's own POST, with the server's key attached", async () => {
    const response = await POST(
      post({
        "sec-fetch-site": "same-origin",
        origin: `http://${HOST}`,
        "content-type": "application/json",
      }),
      params(["jobs"]),
    );

    expect(response.status).toBe(201);
    expect(upstream).toHaveBeenCalledTimes(1);

    const [target, init] = upstream.mock.calls[0] as [string, RequestInit];
    expect(target).toBe(`${UPSTREAM}/api/v1/jobs`);
    expect(new Headers(init.headers).get("authorization")).toBe(`Bearer ${KEY}`);
    // And the Location rewrite still happens, so the guard has not been
    // bolted on in front of a path that no longer works.
    expect(response.headers.get("location")).toBe("/api/rm/jobs/job_01");
  });

  it("forwards a POST from curl, which declares no origin at all", async () => {
    const response = await POST(post({ "content-type": "application/json" }), params(["jobs"]));
    expect(response.status).toBe(201);
    expect(upstream).toHaveBeenCalledTimes(1);
  });

  it("still serves a cross-site GET, which cannot change anything", async () => {
    const request = new NextRequest(`http://${HOST}/api/rm/jobs`, {
      method: "GET",
      headers: { "sec-fetch-site": "cross-site", origin: "https://evil.example" },
    });
    const response = await GET(request, params(["jobs"]));
    expect(response.status).toBe(201);
    expect(upstream).toHaveBeenCalledTimes(1);
  });
});
