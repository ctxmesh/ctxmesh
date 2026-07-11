import { Link } from "react-router-dom";

import { Card, CardContent } from "@/components/ui/card";
import { buttonVariants } from "@/components/ui/button";
import type { RunSummary } from "@/lib/api";

// RecentRuns lists the latest traced runs (from the BFF's /api/runs, Langfuse-
// backed). Each row navigates to the native trace page (/traces/:id, m16.7).
// A "View all runs" link leads to the native runs browser (/runs, m16.8).
// Token-driven throughout; no embedded iframe (demoted in m16.11).

function formatUSD(n: number): string {
  return `$${n.toFixed(4)}`;
}

export function RecentRuns({ runs }: { runs: RunSummary[] }) {
  if (runs.length === 0) {
    return (
      <Card>
        <CardContent className="py-8 text-center text-sm text-muted-foreground">
          No runs in the recent window.
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-2">
      <div className="divide-y rounded-lg border bg-card">
        {runs.map((run) => (
          <Link
            key={run.traceId}
            to={`/traces/${encodeURIComponent(run.traceId)}`}
            className="flex w-full items-center justify-between gap-4 px-4 py-3 text-left transition-colors hover:bg-accent/60"
          >
            <div className="min-w-0">
              <p className="truncate text-sm font-medium text-card-foreground">
                {run.name || "unnamed run"}
              </p>
              <p className="truncate font-mono text-xs text-muted-foreground">
                {run.traceId}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-4 text-xs text-muted-foreground">
              <span className="font-mono">{formatUSD(run.costUSD)}</span>
              <span className="font-mono">{run.tokens} tok</span>
              <span className="font-mono">{Math.round(run.latencyMs)}ms</span>
              {/* Visual affordance only — the whole row is the Link, so this
                  is a styled span (a nested interactive element is invalid). */}
              <span className={buttonVariants({ variant: "outline", size: "sm" })}>
                View trace
              </span>
            </div>
          </Link>
        ))}
      </div>
      <div className="flex justify-end">
        <Link
          to="/runs"
          data-testid="view-all-runs"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          View all runs
        </Link>
      </div>
    </div>
  );
}
