import { NextRequest } from "next/server";
import { addressedHost, crossSiteRefusal } from "@/lib/same-origin";

/**
 * The RunMesh API proxy. Every call the browser makes lands here, is given a
 * bearer token from the server environment, and is forwarded to the Go API.
 *
 * This one file replaces a CORS middleware that would otherwise have to be
 * written in Go, and it is the better trade for reasons that are specific to
 * that codebase rather than general:
 *
 *   - Auth runs OUTSIDE the mux (api.go:180), and a CORS preflight carries no
 *     Authorization header by spec. A header-less OPTIONS would therefore be
 *     401'd before routing ever happened, so the CORS layer could not be a
 *     route — it would have to be a seventh middleware wrapping the other six.
 *   - The API sets five headers a cross-origin browser cannot read without
 *     being explicitly granted them: ETag, X-Request-ID, Location, Retry-After
 *     and Idempotency-Replayed. ETag matters most, because it is the whole
 *     mechanism by which the job poll is cheap.
 *   - Sending a bearer from the browser means shipping the bearer to the
 *     browser, and a key in a bundle is a published key.
 *
 * Same-origin through a route handler answers all three and needs zero Go
 * changes. RUNMESH_API_URL and RUNMESH_API_KEY are read HERE, in Node, and are
 * not `NEXT_PUBLIC_`, so they never reach the client bundle.
 *
 * AND THAT MAKES THIS FILE A CONFUSED DEPUTY, which is not a figure of speech.
 * Holding the key and attaching it to whatever arrives is the definition: the
 * handler acts with the server's authority on a caller's instruction, so it
 * must establish that the caller is this app's own page before it acts at all.
 * It did not, and for a while the FORWARD_UP note below claimed a hole was
 * closed that was in fact wide open — a cross-site `POST` with a
 * CORS-safelisted `text/plain` body is a SIMPLE request, needs no preflight,
 * and so ran the whole way through to `POST /jobs` carrying the key. The guard
 * is `crossSiteRefusal` in lib/same-origin.ts, applied first in `proxy` below;
 * that file argues the shape of it at length, including why a CSRF token is
 * the wrong instrument for an app with no session of its own.
 *
 * They are also deliberately NOT in the repository's .env.example. That file
 * asserts a 1:1 correspondence with internal/config's loader, maintained by
 * discipline rather than by a test, and these two variables are read by Node
 * and by nothing in internal/config. Adding them would quietly make the file's
 * own header comment false.
 */

// Nothing about a runtime console may be cached or statically rendered: every
// response is a live answer about a job that is moving while it is read.
export const dynamic = "force-dynamic";
export const fetchCache = "force-no-store";
// Node, not Edge: the SSE pass-through below hands a ReadableStream straight
// back out, and the Node runtime is where that is known to stay unbuffered.
export const runtime = "nodejs";

/** The API's own prefix. The browser addresses /api/rm/jobs; upstream is /api/v1/jobs. */
const UPSTREAM_PREFIX = "/api/v1";

/**
 * Request headers worth forwarding, lowercased.
 *
 * An allowlist rather than a copy of everything, because a blanket forward
 * sends the browser's Cookie and its own Authorization upstream — the first is
 * a credential the API never asked for, and the second would let a caller
 * override the server-side key with one of their own choosing, which is
 * exactly the hole this proxy exists to close.
 */
const FORWARD_UP = [
  "accept",
  "content-type",
  // The conditional job poll. Without this the ETag round trip is pointless.
  "if-none-match",
  // Replay protection on POST /jobs.
  "idempotency-key",
  // A client-minted id is sanitised and echoed by the Go side, which is what
  // lets one browser action be found in the server logs.
  "x-request-id",
  // SSE resume, for a reconnect that goes through EventSource semantics.
  "last-event-id",
] as const;

/** Response headers worth forwarding back down. */
const FORWARD_DOWN = [
  "content-type",
  "cache-control",
  // The job Version. The cheap poll depends entirely on this surviving.
  "etag",
  "x-request-id",
  "location",
  "retry-after",
  "idempotency-replayed",
] as const;

/**
 * A misconfiguration answered in the API's own envelope shape, so the client's
 * error path renders it like any other failure instead of throwing on a parse.
 *
 * Naming the variable is the point. Forwarding an empty bearer instead would
 * produce a 401, and "unauthenticated" tells an operator their key is wrong
 * when in fact their .env.local is missing.
 */
function misconfigured(variable: string): Response {
  return Response.json(
    {
      error: {
        code: "internal",
        message: `the dashboard is not configured: ${variable} is not set in the server environment`,
      },
    },
    { status: 500 },
  );
}

/**
 * A cross-site state change, refused in the API's own envelope shape.
 *
 * 403 with `permission_denied`, which is the code the Go side uses for "the
 * credential is real and does not cover this" (errors.go:19). That is exactly
 * the situation: the key is present and valid, and it is not lent to this
 * caller. `unauthenticated` would be a lie — nothing about the key is missing
 * — and a bare 403 with no body would render as an unexplained failure in the
 * client's error path, which reads every failure through this envelope.
 */
function refused(reason: string): Response {
  return Response.json(
    { error: { code: "permission_denied", message: reason } },
    { status: 403 },
  );
}

async function proxy(
  request: NextRequest,
  context: { params: Promise<{ path: string[] }> },
): Promise<Response> {
  // FIRST, before the environment is even read. A cross-site POST must not be
  // able to learn whether this deployment is configured — answering it with
  // the 500 from `misconfigured` would turn the guard into an oracle for
  // whether a key is present, which is a thing an attacker would like to know
  // and has no business finding out from a request that is being refused.
  const header = (name: string) => request.headers.get(name);
  const refusal = crossSiteRefusal({
    method: request.method,
    header,
    // The host the browser addressed, read off the request rather than out of
    // `nextUrl` — which Next normalises against its own base URL and which
    // therefore answers `localhost:<port>` for a request addressed to
    // `127.0.0.1:<port>`. `nextUrl.host` is the last-resort fallback only.
    // See the note on addressedHost, and on why the comparison is by host and
    // not by full origin: TLS terminates at the proxy in every real
    // deployment, so the scheme this handler sees is not the one the browser
    // saw.
    host: addressedHost(header, request.nextUrl.host),
  });
  if (refusal) return refused(refusal);

  const base = process.env.RUNMESH_API_URL;
  if (!base) return misconfigured("RUNMESH_API_URL");
  const key = process.env.RUNMESH_API_KEY;
  if (!key) return misconfigured("RUNMESH_API_KEY");

  const { path } = await context.params;
  const search = request.nextUrl.search;
  const target = `${base.replace(/\/+$/, "")}${UPSTREAM_PREFIX}/${path.join("/")}${search}`;

  const headers = new Headers();
  for (const name of FORWARD_UP) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  headers.set("Authorization", `Bearer ${key}`);

  // Request bodies are read whole rather than streamed. The API caps a request
  // at 1 MiB through its BodyLimit middleware, so there is nothing here worth
  // the duplex half-stream complexity, and buffering the request says nothing
  // about the response — which is the direction that has to stay unbuffered.
  const method = request.method;
  const body =
    method === "GET" || method === "HEAD" ? undefined : await request.arrayBuffer();

  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method,
      headers,
      body,
      cache: "no-store",
      // A 201 carries Location as a resource pointer, not a redirect, but
      // manual keeps fetch from ever chasing one on our behalf.
      redirect: "manual",
      // Client disconnect must propagate. A stream handler's only cancellation
      // signal on the Go side is r.Context().Done(), so a browser closing a tab
      // has to reach it or the connection leaks until a timeout collects it.
      signal: request.signal,
    });
  } catch (cause) {
    // The API being down is the single most common state during local
    // development, and it deserves a better answer than an unhandled throw.
    if (request.signal.aborted) {
      // The reader left. There is nobody to answer.
      return new Response(null, { status: 499 });
    }
    return Response.json(
      {
        error: {
          code: "internal",
          message: `the RunMesh API at ${base} did not answer: ${
            cause instanceof Error ? cause.message : "unknown transport failure"
          }`,
        },
      },
      { status: 502 },
    );
  }

  const out = new Headers();
  for (const name of FORWARD_DOWN) {
    const value = upstream.headers.get(name);
    if (value) out.set(name, value);
  }

  // Location points into /api/v1 upstream, which is not a route in this app. A
  // client that followed it verbatim would get a Next 404 rather than the job
  // it had just created.
  const location = out.get("location");
  if (location) {
    out.set("location", location.replace(UPSTREAM_PREFIX, "/api/rm"));
  }

  const contentType = upstream.headers.get("content-type") ?? "";
  if (contentType.startsWith("text/event-stream")) {
    // The stream pass-through. `upstream.body` is handed straight out without
    // being read here: awaiting it in any form — .text(), a TransformStream
    // that accumulates, even a stray tee — would buffer the whole stream and
    // the dashboard would show nothing until the job ended, which is the exact
    // opposite of the endpoint's purpose.
    //
    // no-transform is not decoration. A compressing proxy between here and the
    // browser will otherwise hold frames until its window fills, and
    // X-Accel-Buffering is the same instruction spelled the way nginx reads it.
    out.set("cache-control", "no-cache, no-store, no-transform");
    out.set("x-accel-buffering", "no");
    return new Response(upstream.body, { status: upstream.status, headers: out });
  }

  // 304 is the success case of the conditional job poll, and 204 has no body
  // by definition. Both MUST be constructed with a null body: handing a
  // Response a body for a status that forbids one is a TypeError.
  if (upstream.status === 304 || upstream.status === 204) {
    return new Response(null, { status: upstream.status, headers: out });
  }

  return new Response(upstream.body, { status: upstream.status, headers: out });
}

export const GET = proxy;
export const POST = proxy;
export const HEAD = proxy;
