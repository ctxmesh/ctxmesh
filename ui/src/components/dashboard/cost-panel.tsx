import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { CostResponse, MetricPoint } from "@/lib/api";

// CostPanel renders the native cost/usage view from the BFF's /api/cost DTO
// (Langfuse rollup + Prometheus latency/scale). Fully on-theme: cards and the
// bar chart use SEMANTIC tokens only (bg-primary, text-muted-foreground, …).

function formatUSD(n: number): string {
  return `$${n.toFixed(2)}`;
}

function formatCompact(n: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact" }).format(n);
}

// StatCard is a single headline metric (token-driven surface).
function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <Card>
      <CardContent className="pt-6">
        <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {label}
        </p>
        <p className="mt-1 text-2xl font-semibold tracking-tight">{value}</p>
      </CardContent>
    </Card>
  );
}

// BarChart is a minimal, dependency-free horizontal bar chart. Every color is a
// SEMANTIC token (bg-primary / bg-muted / text-muted-foreground) so it re-themes
// with the token layer. Values are normalized to the series max.
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
          <div key={p.label} className="grid grid-cols-[8rem_1fr_4rem] items-center gap-2">
            <span className="truncate text-xs text-muted-foreground" title={p.label}>
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
  const { summary, latency, scale } = cost;
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <StatCard label="Total cost" value={formatUSD(summary.totalCostUSD)} />
        <StatCard label="Tokens" value={formatCompact(summary.totalTokens)} />
        <StatCard
          label="Observations"
          value={formatCompact(summary.observations)}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Cost by model</CardTitle>
          </CardHeader>
          <CardContent>
            <BarChart
              points={summary.byModel}
              unit={formatUSD}
              emptyLabel="No cost data in the recent window."
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Scale &amp; latency</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <p className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Replicas per agent
              </p>
              <BarChart
                points={scale}
                emptyLabel="No scale metrics (Prometheus not wired)."
              />
            </div>
            <div>
              <p className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                p95 latency (ms)
              </p>
              <BarChart
                points={latency}
                unit={(v) => `${Math.round(v)}ms`}
                emptyLabel="No latency metrics (Prometheus not wired)."
              />
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
