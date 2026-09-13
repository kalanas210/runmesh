"use client";

import { useCallback, useSyncExternalStore } from "react";

/**
 * A media query as a boolean, through useSyncExternalStore rather than an
 * effect.
 *
 * matchMedia is an external system and the useState-plus-useEffect version of
 * this hook has a real bug, not a stylistic one: the first render returns the
 * default, the effect then sets the true value, and anything that renders
 * during that gap — the waterfall deciding between the chart and the narrow
 * attempt list — mounts the wrong subtree, tears it down, and mounts the other
 * one. That is a full remount of the most expensive component on the page on
 * every single navigation.
 *
 * The server snapshot is `false` for every query. That is a choice: the caller
 * asks `(min-width: 900px)`, so `false` means "assume narrow", and the narrow
 * rendering is the one that is correct at any width — it is a complete
 * alternative view of the same data, not a degraded chart. Guessing wide on
 * the server and landing on a phone would mean shipping the chart's markup to
 * a device that cannot use it.
 */
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (onChange: () => void) => {
      if (typeof window === "undefined" || !window.matchMedia) return () => {};
      const list = window.matchMedia(query);
      list.addEventListener("change", onChange);
      return () => list.removeEventListener("change", onChange);
    },
    [query],
  );

  const getSnapshot = useCallback(() => {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia(query).matches;
  }, [query]);

  return useSyncExternalStore(subscribe, getSnapshot, () => false);
}

/**
 * The reduced-motion preference, for the one case CSS cannot reach.
 *
 * Almost everything on this product is handled by the global
 * `prefers-reduced-motion` block in globals.css, which is where it belongs. A
 * hook is needed only where the DECISION differs rather than the duration —
 * for example a cue that must be held on screen longer when it is not allowed
 * to move.
 */
export function usePrefersReducedMotion(): boolean {
  return useMediaQuery("(prefers-reduced-motion: reduce)");
}
