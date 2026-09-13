"use client";

import { useSyncExternalStore } from "react";

/**
 * One clock for the whole page.
 *
 * Several components need to re-render once a second against wall-clock time —
 * the freshness chip, a retry countdown, any relative timestamp. The obvious
 * implementation is a `useState` plus a `setInterval` in each of them, and it
 * is wrong twice over. It runs one timer per mounted component, so a job with
 * forty steps showing forty countdowns runs forty timers that all want the same
 * number; and calling setState from an effect body is a cascading render, which
 * React 19's lint rules reject outright.
 *
 * A clock is an external system, so this is `useSyncExternalStore`: one shared
 * interval that exists only while something is subscribed, one cached value
 * that every reader sees, and a server snapshot of `null` so the server renders
 * a stable placeholder instead of a timestamp the browser would immediately
 * disagree with. That last part is the whole reason this is not simply
 * `Date.now()` in the render body — a time read during SSR and read again
 * during hydration are two different numbers, and React throws the server's
 * markup away over it.
 */

let cached = Date.now();
let timer: ReturnType<typeof setInterval> | null = null;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (timer === null) {
    // Refresh on the way in: the module may have been loaded well before the
    // first subscriber mounted, and the first paint should not be a second late.
    cached = Date.now();
    timer = setInterval(() => {
      cached = Date.now();
      for (const l of listeners) l();
    }, 1000);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };
}

// Must return the CACHED value, never a fresh Date.now(). A getSnapshot that
// returns a new value on every call never compares equal to itself and React
// re-renders for ever.
function getSnapshot(): number {
  return cached;
}

function getServerSnapshot(): null {
  return null;
}

/** Epoch milliseconds, ticking once a second. Null during server render. */
export function useNow(): number | null {
  return useSyncExternalStore<number | null>(subscribe, getSnapshot, getServerSnapshot);
}
