"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * The console chrome, built on ApexTick's AdminShell geometry rather than its
 * marketing header: a sticky 15rem sidebar at lg and above, collapsing to a
 * horizontally scrolling strip below it.
 *
 * The marketing header is a scroll-reactive fixed bar built around a mobile
 * drawer. An operator wants their sections visible at all times and does not
 * want a second focus trap between them and a running job, which is why the
 * narrow layout here is a strip and not a drawer.
 *
 * Note what is absent and deliberately so. There is no Chrome/BARE_PREFIXES
 * switch, because this app has no marketing surface to switch away from. There
 * is no ScrollProgress, because nobody scrubs a job list. There is no Lenis,
 * because it hijacks document-wide wheel events and would fight the
 * waterfall's own horizontal scroll region and its sticky time axis — ApexTick
 * already excludes it from /admin for exactly that reason.
 */

interface NavItem {
  href: string;
  label: string;
  /** Match the pathname exactly rather than by prefix. Only the root needs it. */
  exact?: boolean;
}

const NAV: NavItem[] = [
  { href: "/", label: "Overview", exact: true },
  { href: "/jobs", label: "Jobs" },
  { href: "/submit", label: "Submit" },
  { href: "/tools", label: "Tools" },
  { href: "/runtime", label: "Runtime" },
];

export function AppShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();

  return (
    <div className="lg:flex lg:min-h-screen">
      <aside className="border-b border-line lg:sticky lg:top-0 lg:h-screen lg:w-60 lg:shrink-0 lg:border-b-0 lg:border-r">
        <div className="flex items-center justify-between gap-4 px-5 py-4 lg:block lg:px-6 lg:py-7">
          <div>
            <Link href="/" className="display text-[1.15rem] tracking-tight text-bone">
              RunMesh
            </Link>
            <p className="kicker mt-2 hidden lg:block">Console</p>
          </div>
        </div>

        <nav aria-label="Console sections" className="px-3 pb-3 lg:px-3 lg:pb-6">
          <ul className="flex gap-1 overflow-x-auto lg:block lg:space-y-1 lg:overflow-visible">
            {NAV.map((item) => {
              // Overview is `exact` because every other href starts with "/".
              // Without it the root entry would be permanently active and the
              // sidebar would show two current pages at once.
              const active = item.exact
                ? pathname === item.href
                : pathname.startsWith(item.href);
              return (
                <li key={item.href}>
                  <Link
                    href={item.href}
                    aria-current={active ? "page" : undefined}
                    className={cn(
                      "block whitespace-nowrap rounded-lg px-3 py-2 text-[0.86rem] transition-colors",
                      active
                        ? "bg-bone/[0.06] text-bone lg:rounded-l-none lg:border-l-2 lg:border-accent"
                        : "text-muted hover:bg-bone/[0.03] hover:text-bone",
                    )}
                  >
                    {item.label}
                  </Link>
                </li>
              );
            })}
          </ul>
        </nav>

        <div className="hidden border-t border-line px-6 py-5 lg:block">
          <Link href="/design" className="ulink text-[0.78rem] text-muted">
            Primitives
          </Link>
        </div>
      </aside>

      {/* No fixed header, so none of the marketing pages' top padding. */}
      <main id="main" tabIndex={-1} className="min-w-0 flex-1 px-5 pb-20 pt-8 outline-none md:px-8 lg:px-10">
        {children}
      </main>
    </div>
  );
}
