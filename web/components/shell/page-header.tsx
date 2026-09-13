import type { ReactNode } from "react";

/**
 * The per-screen title block, so every route opens the same way.
 *
 * ApexTick's AdminHeader plus one slot: `freshness`. It sits beside the title
 * rather than beside the data it describes, because on this product almost
 * every screen is a view of something that is still moving, and a chip that
 * moved around the page depending on which screen you were on would be a chip
 * nobody learned to look at.
 */
export function PageHeader({
  kicker,
  title,
  description,
  action,
  freshness,
}: {
  kicker: ReactNode;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  freshness?: ReactNode;
}) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-4 border-b border-line pb-6">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-3">
          <span className="kicker">{kicker}</span>
          {freshness}
        </div>
        <h1 className="display mt-2 text-[clamp(1.5rem,3vw,2.2rem)]">{title}</h1>
        {description && <p className="mt-2 text-[0.86rem] text-muted">{description}</p>}
      </div>
      {action}
    </header>
  );
}
