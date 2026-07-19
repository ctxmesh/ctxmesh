import type { ComponentType } from "react";
import { Activity, Boxes, Coins, Gauge, ListChecks } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import {
  formatCompact,
  formatLatency,
  formatUSD,
  latencyStats,
} from "@/lib/format";
import type { CostResponse, RunSummary, TopologyResponse } from "@/lib/api";

// DashboardStats is the headline metric row: the five numbers an operator wants at a
// glance — spend, tokens, run volume, latency, fleet size — sourced from the data that is
// actually available (Langfuse cost rollup + the runs list + live topology), NOT the
// unwired Prometheus latency/scale that used to leave the cards blank. Each stat renders a
// muted placeholder until its own source loads (the three feeds resolve independently).

interface StatCardProps {
  label: string;
  value: string;
  sub?: string;
  icon: ComponentType<{ className?: string }>;
  loading?: boolean;
}

function StatCard({ label, value, sub, icon: Icon, loading }: StatCardProps) {
  return (
    <Card>
      <CardContent className="p-4">
        <div className="flex items-center justify-between">
          <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {label}
          </p>
          <Icon className="h-4 w-4 shrink-0 text-muted-foreground" />
        </div>
        <p
          className={cn(
            "mt-2 text-2xl font-semibold tracking-tight tabular-nums",
            loading && "text-muted-foreground",
          )}
        >
          {loading ? "…" : value}
        </p>
        <p className="mt-0.5 h-4 text-xs text-muted-foreground">
          {loading ? "" : (sub ?? "")}
        </p>
      </CardContent>
    </Card>
  );
}

export function DashboardStats({
  cost,
  runs,
  topology,
}: {
  cost?: CostResponse;
  runs?: RunSummary[];
  topology?: TopologyResponse;
}) {
  const lat = runs ? latencyStats(runs.map((r) => r.latencyMs)) : undefined;
  const agents = topology?.nodes.filter((n) => n.kind === "agent") ?? [];
  const readyAgents = agents.filter((n) => n.health === "ready").length;

  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
      <StatCard
        label="Cost"
        icon={Coins}
        loading={!cost}
        value={cost ? formatUSD(cost.summary.totalCostUSD) : ""}
        sub="last 30 days"
      />
      <StatCard
        label="Tokens"
        icon={Activity}
        loading={!cost}
        value={cost ? formatCompact(cost.summary.totalTokens) : ""}
        sub={cost ? `${formatCompact(cost.summary.observations)} observations` : ""}
      />
      <StatCard
        label="Recent runs"
        icon={ListChecks}
        loading={!runs}
        value={runs ? String(runs.length) : ""}
        sub="in recent window"
      />
      <StatCard
        label="Avg latency"
        icon={Gauge}
        loading={!runs}
        value={lat ? formatLatency(lat.avgMs) : ""}
        sub={lat && lat.p95Ms > 0 ? `p95 ${formatLatency(lat.p95Ms)}` : ""}
      />
      <StatCard
        label="Active agents"
        icon={Boxes}
        loading={!topology}
        value={topology ? String(agents.length) : ""}
        sub={topology ? `${readyAgents}/${agents.length} ready` : ""}
      />
    </div>
  );
}
