/**
 * The proxy's confused-deputy guard, as a pure function.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS EXISTS, and why the proxy is not safe without it.
 *
 * `app/api/rm/[...path]/route.ts` is a deputy by construction: it holds
 * RUNMESH_API_KEY, it attaches `Authorization: Bearer` to whatever arrives,
 * and it forwards the result upstream. Everything it does on a caller's behalf
 * it does with the server's own credential, so the only question that matters
 * is whether the caller is this app's own page. Nothing in the request body
 * answers that, and until this function existed nothing asked.
 *
 * The attack is a SIMPLE cross-site request, not a preflighted one, and that
 * distinction is the whole of it. `text/plain` is one of the three
 * CORS-safelisted request content types, so
 *
 *   fetch("https://console.example/api/rm/jobs", {
 *     method: "POST",
 *     headers: { "content-type": "text/plain" },
 *     body: JSON.stringify({ goal: "…" }),
 *   })
 *
 * from any page the operator happens to have open is dispatched with no
 * preflight at all. The browser refuses to hand the RESPONSE back to the
 * attacker's script, which is the only thing CORS promises — but the request
 * has already run, the key has already been attached, and the job has already
 * been submitted. `decodeJSON` (internal/httpapi/jobs.go:253) never inspects
 * Content-Type, so the `text/plain` label costs the attacker nothing: the Go
 * side parses the body as JSON regardless. `POST /jobs` and
 * `POST /jobs/{id}/cancel` are therefore both reachable, which is submit and
 * cancel on someone else's runtime, from a page that never saw the key.
 *
 * WHY A SAME-ORIGIN CHECK AND NOT A CSRF TOKEN.
 *
 * A synchroniser token is the textbook answer and it is the wrong shape here,
 * because this app has no session of its own to bind one to. There is no
 * login, no cookie, no per-user server state — the single credential is an
 * environment variable, shared by every reader of the console. A token minted
 * by this server and handed to the page would therefore be minted for
 * ANYBODY who can load the page, and a value that is issued on request to all
 * comers is not a secret; it is a second round trip pretending to be one. The
 * double-submit variant is worse still: it needs a cookie, and this app
 * deliberately sets none (the proxy's own FORWARD_UP list refuses to pass
 * `cookie` upstream precisely so that no credential can travel that way).
 *
 * What actually distinguishes the two callers is ORIGIN, and the browser is
 * the only party that can attest to it — an attacker's script cannot forge
 * `Origin` or `Sec-Fetch-Site`, because both are forbidden header names that
 * `fetch` and `XMLHttpRequest` refuse to let script set. So the check is: a
 * state-changing request must either declare itself same-origin or declare
 * nothing at all.
 *
 * TWO HEADERS, BECAUSE NEITHER IS SUFFICIENT ALONE.
 *
 *   - `Sec-Fetch-Site` is the precise signal and is checked first. It is sent
 *     on every fetch by every current browser engine, it distinguishes
 *     `same-origin` from `same-site` (a sibling subdomain is NOT this app) and
 *     from `cross-site`, and `none` means the user typed the URL or opened a
 *     bookmark, which no attacker's page can cause.
 *   - `Origin` is the fallback, for a client old enough or odd enough to omit
 *     the fetch-metadata header while still being a browser. It is checked by
 *     HOST rather than by full origin string: this app is routinely served
 *     behind a proxy that terminates TLS, so the scheme the browser saw
 *     (`https`) and the scheme this handler sees (`http`) legitimately differ,
 *     and a strict origin comparison would 403 every real request in exactly
 *     the deployment that needs the guard most. The host cannot differ that
 *     way — it is the name the browser dialled, echoed back in the request
 *     line — so it is the half of the origin that is actually load-bearing.
 *
 * Both absent is ALLOWED, and that is deliberate rather than an oversight. A
 * request with neither header is not a browser request: curl, a health probe,
 * a Playwright API call, an operator's own script. Those are not
 * confused-deputy vectors, because the deputy problem requires a victim's
 * browser to supply the ambient authority, and a program that can reach this
 * endpoint at all is already inside the trust boundary that holds the key. A
 * guard that refused them would break every non-browser caller to defend
 * against an attacker who, by construction, cannot be one of them.
 *
 * WHAT IS DELIBERATELY NOT DONE HERE.
 *
 * No `OPTIONS` handler is exported from the route, and that is not an
 * omission. Next answers an unexported OPTIONS itself — verified against
 * `next start`: `204` with `Allow: GET, HEAD, OPTIONS, POST` and, decisively,
 * NO `Access-Control-Allow-Origin`. A preflight whose response omits that
 * header fails, so the moment an attacker reaches for anything that needs one
 * — an `application/json` content type, an `Idempotency-Key`, any custom
 * header — the real request never leaves the browser. The simple-request path
 * closed below is the only one that ever existed.
 * ---------------------------------------------------------------------------
 */

/**
 * Methods that cannot change state and so need no origin proof.
 *
 * This is the HTTP definition, not an inventory of what the route happens to
 * export today. The route exports GET, POST and HEAD; the day it grows DELETE
 * or PATCH for a cancel-by-verb, the new method is guarded by default rather
 * than by whoever remembers to come back here. Defaulting the other way is how
 * a guard survives one refactor and not the second.
 */
const SAFE_METHODS: ReadonlySet<string> = new Set(["GET", "HEAD", "OPTIONS", "TRACE"]);

export interface OriginCheckRequest {
  /** The HTTP method, in any case. */
  method: string;
  /** Case-insensitive header lookup — `Headers.get`, or anything shaped like it. */
  header: (name: string) => string | null | undefined;
  /**
   * The host:port the request was ADDRESSED to, as the browser wrote it. Use
   * `addressedHost` below to derive it; do not pass a configured value. A
   * guard compared against configuration answers "is this the host I
   * expected", which is a different and much less useful question than "did
   * the page that sent this live at the same place it sent it to".
   */
  host: string;
}

/**
 * The host the browser actually dialled.
 *
 * This is NOT `request.nextUrl.host`, and that is a trap worth naming because
 * the two look interchangeable and are not. Next normalises `nextUrl` against
 * its own base URL, so a production server started on port 3117 reports
 * `localhost:3117` for a request that was addressed to `127.0.0.1:3117` — and
 * a guard built on it 403s a page that is unambiguously same-origin, which is
 * how a security fix becomes an outage. That was observed, not theorised.
 *
 * `X-Forwarded-Host` wins where it is present, because the browser's Origin
 * carries the PUBLIC name and a reverse proxy routinely rewrites `Host` to the
 * upstream's. Neither header is a weakness: a browser cannot set either from
 * script — `Host` is forbidden outright and `X-Forwarded-Host` is not
 * CORS-safelisted, so reaching for it triggers a preflight, and this route's
 * preflight answer carries no `Access-Control-Allow-Origin` (see the note
 * above), which fails it.
 *
 * The first value of a comma-separated `X-Forwarded-Host` is the client's own,
 * the rest being appended by each hop.
 */
export function addressedHost(
  header: (name: string) => string | null | undefined,
  fallback: string,
): string {
  const forwarded = header("x-forwarded-host");
  if (forwarded) {
    const first = forwarded.split(",")[0]?.trim();
    if (first) return first;
  }
  return header("host")?.trim() || fallback;
}

/**
 * Why this request must not be forwarded, as the sentence the caller returns,
 * or null when it may proceed.
 *
 * A sentence rather than a boolean because the refusal is rendered to a human:
 * the likeliest reader of a 403 from this function is not an attacker but a
 * developer who has just put the console behind a new proxy, and "which header
 * disagreed, and what it said" is the entire content of the answer they need.
 */
export function crossSiteRefusal(request: OriginCheckRequest): string | null {
  if (SAFE_METHODS.has(request.method.toUpperCase())) return null;

  // Checked before `Origin` because it is strictly more informative: it is the
  // browser's own classification of the relationship between the initiator and
  // the target, so it catches a same-SITE sibling subdomain that an Origin
  // host comparison would also catch but that a naive eTLD+1 comparison would
  // wave through.
  const site = request.header("sec-fetch-site");
  if (site && site !== "same-origin" && site !== "none") {
    return `this request declared Sec-Fetch-Site: ${site}, and the API proxy forwards the server's key only for this app's own pages`;
  }

  const origin = request.header("origin");
  if (origin) {
    // `Origin: null` is the opaque origin, and it is REFUSED rather than
    // treated as "no origin". A sandboxed iframe — `<iframe sandbox=
    // "allow-scripts" srcdoc="…">` on an attacker's page — is exactly how a
    // cross-site request acquires one, so reading it as absent would reopen
    // the hole for the one caller that can choose to have no origin at all. A
    // present-but-opaque origin is definitively not this app's.
    //
    // An `Origin` that will not parse falls into the same branch for the same
    // reason: a value this function cannot reason about is refused rather than
    // ignored, because the only reason to send one on a POST is to find out
    // which way this line falls.
    let host: string;
    try {
      host = new URL(origin).host;
    } catch {
      return `this request carried an Origin the proxy cannot attribute to this app (${origin})`;
    }
    if (host !== request.host) {
      return `this request came from ${origin}, which is not ${request.host}, and the API proxy forwards the server's key only for this app's own pages`;
    }
  }

  return null;
}
