import { Coins } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { CostResponse, MetricPoint } from "@/lib/api";
import { formatUSD } from "@/lib/format";

// CostPanel renders the per-model cost breakdown from the BFF's /api/cost DTO (Langfuse
// daily-metrics rollup). The headline totals moved to the dashboard stat row (m35); this
// card is the "where did the spend go" view. Fully on-theme: SEMANTIC tokens only.

// BarChart is a minimal, dependency-free horizontal bar chart. Every color is a SEMANTIC
// token (bg-primary / bg-muted / text-muted-foreground) so it re-themes with the token
// layer. Values are normalized to the series max.
function BarChart({
  points,
  unit,
  emptyLabel,
}: {
  points: MetricPoint[];
  unit?: (v: number) => string;
  emptyLabel: string;
}) {
  if (points.length === 0) {
    return <p className="text-sm text-muted-foreground">{emptyLabel}</p>;
  }
  const max = Math.max(...points.map((p) => p.value), 0);
  const fmt = unit ?? ((v: number) => String(v));
  return (
    <div className="space-y-2">
      {points.map((p) => {
        const pct = max > 0 ? Math.round((p.value / max) * 100) : 0;
        return (
          <div
            key={p.label}
            className="grid grid-cols-[9rem_1fr_5rem] items-center gap-2"
          >
            <span
              className="truncate text-xs text-muted-foreground"
              title={p.label}
            >
              {p.label}
            </span>
            <div className="h-2 overflow-hidden rounded-full bg-muted">
              <div
                className="h-full rounded-full bg-primary"
                style={{ width: `${pct}%` }}
                role="progressbar"
                aria-valuenow={p.value}
                aria-label={p.label}
              />
            </div>
            <span className="text-right font-mono text-xs">{fmt(p.value)}</span>
          </div>
        );
      })}
    </div>
  );
}

export function CostPanel({ cost }: { cost: CostResponse }) {
  return (
    <Card className="h-full">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <Coins className="h-4 w-4 text-primary" />
          Cost by model
        </CardTitle>
      </CardHeader>
      <CardContent>
        <BarChart
          points={cost.summary.byModel}
          unit={formatUSD}
          emptyLabel="No cost data in the recent window."
        />
      </CardContent>
    </Card>
  );
}
