import type { NextConfig } from "next";

/**
 * Deliberately smaller than ApexTick's. Two things are dropped:
 *
 * `output: "standalone"` — that exists so ApexTick can be copied into a thin
 * runtime image. This console is served next to the Go binary it talks to and
 * has no container story of its own yet; turning it on now would produce a
 * build artefact nothing consumes.
 *
 * `images.remotePatterns` — there are no remote images. Every graphic on this
 * product is an inline SVG or a CSS gradient, so an allowlist here would be a
 * list of hosts we never contact.
 */
const nextConfig: NextConfig = {
  reactStrictMode: true,
};

export default nextConfig;
