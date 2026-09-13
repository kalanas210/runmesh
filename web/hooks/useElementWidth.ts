"use client";

import { useEffect, useState, type RefObject } from "react";

/**
 * The content width of an element, tracked through a ResizeObserver.
 *
 * The waterfall's whole layout is a function of this number — the scale maps
 * time onto it, the idle gutters are subtracted from it, the tick ladder is
 * chosen against it — so it cannot come from a window resize listener. The
 * plot is inside a sidebar layout and a right-hand inspector that opens and
 * closes at 1280px, and both change the plot's width without the window
 * changing at all.
 *
 * It returns 0 until the first observation, which is correct rather than
 * merely safe: the scale is total at width 0 and simply produces an empty
 * chart, so the first paint is an empty plot of the right height instead of a
 * chart drawn against a guessed width that reflows a frame later.
 */
export function useElementWidth<T extends HTMLElement>(ref: RefObject<T | null>): number {
  const [width, setWidth] = useState(0);

  useEffect(() => {
    const element = ref.current;
    if (!element) return;

    // Older Safari and any SSR-adjacent environment without the constructor:
    // one measurement is better than none, and the chart stays usable.
    if (typeof ResizeObserver === "undefined") {
      setWidth(element.clientWidth);
      return;
    }

    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        // contentRect, not offsetWidth: the border and padding of the plot
        // frame are not time, and including them would shift every bar right
        // by a pixel or two against the axis above it.
        const next = Math.floor(entry.contentRect.width);
        setWidth((current) => (current === next ? current : next));
      }
    });
    observer.observe(element);
    setWidth(Math.floor(element.getBoundingClientRect().width));

    return () => observer.disconnect();
  }, [ref]);

  return width;
}
