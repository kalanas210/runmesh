import { describe, expect, it } from "vitest";
import { addressedHost, crossSiteRefusal } from "./same-origin";

/**
 * The proxy's origin guard, tested as a pure function against the exact header
 * combinations a real caller sends.
 *
 * Every case below is named after the CALLER rather than after the branch,
 * because the question this function answers is "who sent this" and the only
 * way to be sure the answer is right is to spell out the four callers that
 * actually exist:
 *
 *   - the console's own page, which sends Sec-Fetch-Site: same-origin;
 *   - an attacker's page, which cannot send anything but cross-site;
 *   - a sandboxed iframe on an attacker's page, whose Origin is `null`;
 *   - curl and the health probe, which send neither header.
 *
 * The attacker cases are the load-bearing ones: `Origin` and `Sec-Fetch-Site`
 * are forbidden header names, so a page's script cannot set either of them,
 * which is the only reason a header can be trusted as evidence of origin at
 * all.
 */

/** A header bag shaped like `Headers.get`: case-insensitive, null when absent. */
function headers(bag: Record<string, string>): (name: string) => string | null {
  const lower = new Map(Object.entries(bag).map(([k, v]) => [k.toLowerCase(), v]));
  return (name) => lower.get(name.toLowerCase()) ?? null;
}

const HOST = "console.example:3000";

function check(method: string, bag: Record<string, string>): string | null {
  return crossSiteRefusal({ method, header: headers(bag), host: HOST });
}

describe("crossSiteRefusal: the console's own page", () => {
  it("forwards a POST that declares itself same-origin", () => {
    expect(
      check("POST", {
        "sec-fetch-site": "same-origin",
        origin: `http://${HOST}`,
        "content-type": "application/json",
      }),
    ).toBeNull();
  });

  it("forwards a POST whose Origin scheme differs from the one this handler sees", () => {
    // TLS terminates at the proxy: the browser saw https, Node sees http, and
    // the host is the only half of the origin that survives that hop intact.
    // Comparing full origin strings would 403 every real request in exactly
    // the deployment shape that needs the guard.
    expect(
      check("POST", { "sec-fetch-site": "same-origin", origin: `https://${HOST}` }),
    ).toBeNull();
  });

  it("forwards a navigation-initiated request, which carries site=none", () => {
    // The operator typed the URL or used a bookmark. No page initiated it, so
    // no page can have initiated it maliciously.
    expect(check("POST", { "sec-fetch-site": "none" })).toBeNull();
  });
});

describe("crossSiteRefusal: an attacker's page", () => {
  /**
   * The exact request the guard exists for. `text/plain` is CORS-safelisted,
   * so this is a SIMPLE request: no preflight, the handler runs, and before
   * this function existed the server's key went upstream with it. decodeJSON
   * (internal/httpapi/jobs.go:253) never looks at Content-Type, so the
   * `text/plain` label costs the attacker nothing.
   */
  it("refuses the simple cross-site POST that needs no preflight", () => {
    const reason = check("POST", {
      "sec-fetch-site": "cross-site",
      origin: "https://evil.example",
      "content-type": "text/plain;charset=UTF-8",
    });
    expect(reason).toContain("cross-site");
  });

  it("refuses a same-SITE sibling subdomain, which is not this origin", () => {
    // A subdomain takeover or an unrelated app on a sibling host is not this
    // app. `same-site` is a weaker relationship than `same-origin` and the
    // guard must not conflate them.
    expect(check("POST", { "sec-fetch-site": "same-site" })).toContain("same-site");
  });

  it("refuses a cross-origin POST from a client that sends no fetch metadata", () => {
    // The Origin half of the guard, for a browser old enough to omit
    // Sec-Fetch-Site. Without it the refusal above is defeated by the absence
    // of one header.
    expect(check("POST", { origin: "https://evil.example" })).toContain("evil.example");
  });

  it("refuses the opaque origin a sandboxed iframe sends", () => {
    // `<iframe sandbox="allow-scripts" srcdoc="…fetch(…)…">` on an attacker's
    // page sends `Origin: null`. Reading that as "no origin" would hand the
    // hole straight back to the one caller that can choose not to have one.
    expect(check("POST", { origin: "null" })).toContain("null");
  });

  it("refuses an Origin that will not parse rather than ignoring it", () => {
    expect(check("POST", { origin: "://" })).toContain("://");
  });

  it("guards every unsafe method, including ones the route does not export yet", () => {
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
      expect(check(method, { "sec-fetch-site": "cross-site" })).not.toBeNull();
    }
  });

  it("is not defeated by the method's case", () => {
    // `new Request()` normalises the method, but this function takes a string
    // from a caller and a guard that can be stepped around with `post` is not
    // a guard.
    expect(check("post", { "sec-fetch-site": "cross-site" })).not.toBeNull();
  });
});

describe("crossSiteRefusal: a reader, and a program", () => {
  it("never refuses a safe read, whatever its origin", () => {
    // A cross-site GET cannot change anything, and the browser will not hand
    // the body back to the initiator. Refusing it would break nothing an
    // attacker cares about and would break an embedded status page.
    for (const method of ["GET", "HEAD", "OPTIONS", "TRACE"]) {
      expect(check(method, { "sec-fetch-site": "cross-site", origin: "https://evil.example" })).toBeNull();
    }
  });

  it("forwards a POST from a caller that sends neither header", () => {
    // curl, a probe, an operator's script. The confused-deputy problem needs a
    // victim's browser to supply the ambient authority; a program that can
    // already reach this endpoint is inside the boundary that holds the key.
    expect(check("POST", { "content-type": "application/json" })).toBeNull();
  });
});

describe("addressedHost: which host the guard compares against", () => {
  it("reads the Host header in preference to a framework-normalised fallback", () => {
    // OBSERVED, not theorised. `next start -p 3117` reports
    // `nextUrl.host === "localhost:3117"` for a request addressed to
    // `127.0.0.1:3117`, because Next normalises nextUrl against its own base
    // URL. Comparing the Origin against that 403s a page that is
    // unambiguously same-origin, which is how a security fix becomes an
    // outage.
    expect(addressedHost(headers({ host: "127.0.0.1:3117" }), "localhost:3117")).toBe(
      "127.0.0.1:3117",
    );
  });

  it("prefers X-Forwarded-Host, which is the public name the browser dialled", () => {
    // A reverse proxy rewrites Host to the upstream's name; the browser's
    // Origin still carries the public one. Without this the guard refuses
    // every request in the deployment shape that most needs it.
    expect(
      addressedHost(
        headers({ "x-forwarded-host": "console.example", host: "10.0.0.4:3000" }),
        "localhost:3000",
      ),
    ).toBe("console.example");
  });

  it("takes the first hop of a comma-separated X-Forwarded-Host", () => {
    expect(
      addressedHost(headers({ "x-forwarded-host": "console.example, inner.svc" }), "x"),
    ).toBe("console.example");
  });

  it("falls back only when the request names no host at all", () => {
    expect(addressedHost(headers({}), "localhost:3000")).toBe("localhost:3000");
    expect(addressedHost(headers({ host: "  " }), "localhost:3000")).toBe("localhost:3000");
  });
});

describe("crossSiteRefusal: the host it is given is the host it uses", () => {
  it("accepts a same-origin POST addressed by IP rather than by name", () => {
    // The end-to-end shape of the bug above: browser at http://127.0.0.1:3117
    // posting to the page it is on.
    expect(
      crossSiteRefusal({
        method: "POST",
        header: headers({ origin: "http://127.0.0.1:3117", host: "127.0.0.1:3117" }),
        host: addressedHost(headers({ origin: "http://127.0.0.1:3117", host: "127.0.0.1:3117" }), "localhost:3117"),
      }),
    ).toBeNull();
  });

  it("still refuses a cross-origin POST once the host is read off the request", () => {
    const bag = headers({ origin: "https://evil.example", host: "127.0.0.1:3117" });
    expect(
      crossSiteRefusal({
        method: "POST",
        header: bag,
        host: addressedHost(bag, "localhost:3117"),
      }),
    ).toContain("evil.example");
  });
});
