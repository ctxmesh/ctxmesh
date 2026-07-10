import * as React from "react";

import { cn } from "@/lib/utils";

// Skeleton — the ONE loading primitive (kit, m13.1 design → m13.4 real).
// Token-driven shimmer over --surface-2; every surface shows skeletons on load
// instead of a bare "Loading…" string (design principle: honest loading).
//
// Compose the shaped helpers (SkeletonText / SkeletonTable / SkeletonCard) for
// common layouts so each surface doesn't reinvent loading geometry.

export interface SkeletonProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Disable the shimmer animation (e.g. reduced-motion / static wireframe). */
  static?: boolean;
}

export function Skeleton({ className, static: noAnim, ...props }: SkeletonProps) {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading"
      className={cn(
        "rounded-md bg-surface-2",
        !noAnim && "animate-pulse",
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
}: {
  lines?: number;
  className?: string;
}) {
  return (
    <div className={cn("space-y-2", className)}>
      {Array.from({ length: lines }).map((_, i) => (
        <Skeleton
          key={i}
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
    <div className={cn("space-y-3", className)}>
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex items-center gap-4">
          {Array.from({ length: cols }).map((_, c) => (
            <Skeleton
              key={c}
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
    <div className={cn("rounded-lg border bg-card p-6 shadow-card", className)}>
      <Skeleton className="mb-4 h-5 w-1/3" />
      <SkeletonText lines={3} />
    </div>
  );
}
