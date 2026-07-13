import { Link } from "react-router-dom";

import { ResourceLink, type ResourceKind } from "@/components/kit";
import type { TopologyResponse, TopologyNode, TopologyHealth } from "@/lib/api";

// TopologySummary — the scale-first dashboard topology (M22 / U5, decided
// 2026-07-13). A dashboard "at a glance" card cannot draw every node at 100s of
// registries / 1000s of agents, so it SUMMARIZES: counts, health rollups, and
// the hotspots that need attention, with a drill-in to the full grouped graph on
// /topology (M15). It replaces the old flat 3-column node grid that became an
// unreadable wall at scale.

// Worst health first when ordering hotspots.
const HEALTH_RANK: Record<TopologyHealth, number> = {
  notReady: 0,
  pending: 1,
  unknown: 2,
  ready: 3,
};

// Only registries + agents have detail pages; tools are shown as plain text.
function linkKind(kind: TopologyNode["kind"]): ResourceKind | null {
  if (kind === "agent") return "agent";
  if (kind === "registry") return "registry";
  return null;
}

function Stat({ label, n }: { label: string; n: number }) {
  return (
    <div className="flex flex-col">
      <span className="text-lg font-semibold tabular-nums leading-none">{n}</span>
      <span className="text-xs text-muted-foreground">{label}</span>
    </div>
  );
}

export function TopologySummary({ topology }: { topology: TopologyResponse }) {
  const nodes = topology.nodes;

  const counts = { registry: 0, agent: 0, tool: 0 };
  const health = { ready: 0, notReady: 0, pending: 0, unknown: 0 };
  for (const n of nodes) {
    counts[n.kind] += 1;
    health[n.health] += 1;
  }

  // Hotspots: the resources that need attention (notReady first, then pending),
  // capped — a summary points you at problems, it doesn't list everything.
  const hotspots = nodes
    .filter((n) => n.health === "notReady" || n.health === "pending")
    .sort((a, b) => HEALTH_RANK[a.health] - HEALTH_RANK[b.health])
    .slice(0, 6);

  if (nodes.length === 0) {
    return (
      <div
        className="flex h-full items-center justify-center text-sm text-muted-foreground"
        data-testid="topology-summary-empty"
      >
        No registries, agents, or tools yet.
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col gap-3 p-1" data-testid="topology-summary">
      <div className="flex gap-6">
        <Stat label="Registries" n={counts.registry} />
        <Stat label="Agents" n={counts.agent} />
        <Stat label="Tools" n={counts.tool} />
      </div>

      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        <span className="text-success">● {health.ready} ready</span>
        {health.pending > 0 && (
          <span className="text-warning-foreground">◒ {health.pending} pending</span>
        )}
        {health.notReady > 0 && (
          <span className="text-destructive">✕ {health.notReady} not ready</span>
        )}
        {health.unknown > 0 && (
          <span className="text-muted-foreground">? {health.unknown} unknown</span>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-auto">
        {hotspots.length > 0 ? (
          <>
            <p className="mb-1 text-xs font-medium text-muted-foreground">
              Needs attention
            </p>
            <ul className="space-y-1" data-testid="topology-hotspots">
              {hotspots.map((n) => {
                const kind = linkKind(n.kind);
                return (
                  <li
                    key={n.id}
                    className="flex items-center justify-between gap-2 text-xs"
                  >
                    {kind ? (
                      <ResourceLink
                        kind={kind}
                        namespace={n.namespace}
                        name={n.name}
                        className="truncate font-mono"
                      />
                    ) : (
                      <span className="truncate font-mono">{n.name}</span>
                    )}
                    <span
                      className={
                        n.health === "notReady"
                          ? "shrink-0 text-destructive"
                          : "shrink-0 text-warning-foreground"
                      }
                    >
                      {n.health === "notReady" ? "not ready" : "pending"}
                    </span>
                  </li>
                );
              })}
            </ul>
          </>
        ) : (
          <p className="text-xs text-success">All resources healthy.</p>
        )}
      </div>

      <Link
        to="/topology"
        className="text-xs text-primary hover:underline"
        data-testid="open-full-topology"
      >
        Open full topology →
      </Link>
    </div>
  );
}
