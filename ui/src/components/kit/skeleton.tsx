import * as React from "react";

import { cn } from "@/lib/utils";

// Skeleton — the ONE loading primitive (kit, m13.1 design → m13.4 real).
// Token-driven shimmer over --surface-2; every surface shows skeletons on load
// instead of a bare "Loading…" string (design principle: honest loading).
//
// Compose the shaped helpers (SkeletonText / SkeletonTable / SkeletonCard) for
// common layouts so each surface doesn't reinvent loading geometry.
//
// a11y (m13.4): a bare <Skeleton> is a self-announcing busy region. The shaped
// helpers wrap their many blocks in ONE labelled busy region and mark the inner
// bars `aria-hidden` — so a screen reader hears "Loading" once, not once per
// bar. `static` (or a reduced-motion preference) disables the pulse.

export interface SkeletonProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Disable the shimmer animation (e.g. reduced-motion / static wireframe). */
  static?: boolean;
  /** Internal: this bar is part of a larger busy region — don't self-announce. */
  decorative?: boolean;
}

export function Skeleton({
  className,
  static: noAnim,
  decorative,
  ...props
}: SkeletonProps) {
  const a11y = decorative
    ? { "aria-hidden": true as const }
    : { role: "status", "aria-busy": true as const, "aria-label": "Loading" };
  return (
    <div
      {...a11y}
      className={cn(
        "rounded-md bg-surface-2",
        // motion-reduce: honor the user's OS "reduce motion" setting.
        !noAnim && "animate-pulse motion-reduce:animate-none",
        className,
      )}
      {...props}
    />
  );
}

/** N lines of body text; the last line is short (natural paragraph ragged edge). */
export function SkeletonText({
  lines = 3,
  className,
  decorative,
}: {
  lines?: number;
  className?: string;
  /** Internal: part of a larger busy region — don't self-announce. */
  decorative?: boolean;
}) {
  const region = decorative
    ? { "aria-hidden": true as const }
    : { role: "status", "aria-busy": true as const, "aria-label": "Loading" };
  return (
    <div {...region} className={cn("space-y-2", className)}>
      {Array.from({ length: lines }).map((_, i) => (
        <Skeleton
          key={i}
          decorative
          className={cn("h-3.5", i === lines - 1 ? "w-2/5" : "w-full")}
        />
      ))}
    </div>
  );
}

/** A rows×cols grid of cells — the DataTable's loading state. */
export function SkeletonTable({
  rows = 6,
  cols = 4,
  className,
}: {
  rows?: number;
  cols?: number;
  className?: string;
}) {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading table"
      className={cn("space-y-3", className)}
    >
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex items-center gap-4">
          {Array.from({ length: cols }).map((_, c) => (
            <Skeleton
              key={c}
              decorative
              className={cn("h-4", c === 0 ? "w-1/3" : "flex-1")}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

/** A card-shaped block: title + a few body lines inside a bordered surface. */
export function SkeletonCard({ className }: { className?: string }) {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading"
      className={cn("rounded-lg border bg-card p-6 shadow-card", className)}
    >
      <Skeleton decorative className="mb-4 h-5 w-1/3" />
      <SkeletonText lines={3} decorative />
    </div>
  );
}
