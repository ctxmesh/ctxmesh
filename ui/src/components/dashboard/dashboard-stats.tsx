import type { ComponentType } from "react";
import { Link } from "react-router-dom";
import { Boxes, Coins, Gauge, ListChecks } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { formatLatency, latencyStats } from "@/lib/format";
import type { RunSummary, TopologyResponse } from "@/lib/api";

// DashboardStats is the headline metric row: run volume, latency, and fleet size —
// sourced from the data available WITHOUT a tenant (the runs list + live topology). Spend
// and tokens are now TENANT-SCOPED (ADR 0077) and cannot be shown on the tenant-less
// dashboard, so those two cards are a calm "per-tenant → Cost page" pointer rather than a
// number. Each live stat renders a muted placeholder until its own source loads (the feeds
// resolve independently).

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

// CostPointerCard replaces the Cost + Tokens numeric stat cards: spend is now
// per-tenant (ADR 0077) and the dashboard has no tenant, so this card points to
// the /cost page rather than showing a tenant-less (or fabricated) number.
function CostPointerCard() {
  return (
    <Card data-testid="cost-stat-pointer">
      <CardContent className="p-4">
        <div className="flex items-center justify-between">
          <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            Cost
          </p>
          <Coins className="h-4 w-4 shrink-0 text-muted-foreground" />
        </div>
        <p className="mt-2 text-sm text-muted-foreground">
          Spend &amp; forecasts
        </p>
        <Link
          to="/cost"
          className="mt-0.5 inline-block text-xs text-primary underline-offset-4 hover:underline"
        >
          View on Cost page
        </Link>
      </CardContent>
    </Card>
  );
}

export function DashboardStats({
  runs,
  topology,
}: {
  runs?: RunSummary[];
  topology?: TopologyResponse;
}) {
  const lat = runs ? latencyStats(runs.map((r) => r.latencyMs)) : undefined;
  const agents = topology?.nodes.filter((n) => n.kind === "agent") ?? [];
  const readyAgents = agents.filter((n) => n.health === "ready").length;

  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <CostPointerCard />
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
