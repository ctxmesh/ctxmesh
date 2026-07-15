// topology-page.tsx — m15.13 Topology v2
//
// A full-page topology view with:
//   - Grouped mode: agents folded into registry/namespace groups (scale-safe at 200+)
//   - List↔graph toggle: same data, two presentations
//   - Search: ?q= filters within groups (debounced)
//   - Click group to expand/collapse (adds/removes from ?expand set)
//   - Click expanded agent node → DetailDrawer with summary + link to detail page
//   - ForbiddenInline on 403
//
// Data contract: GET /api/topology?group=registry&q=&expand=<ids>
//   → { nodes[], edges[], groups[] }
// groups[] is the scale-safe unit; nodes/edges only contain expanded members.
//
// This page uses the BFF's grouped mode ONLY — the flat/raw mode is the
// dashboard mini-graph. The two views share the same api.topology() call.

import * as React from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  Boxes,
  ChevronDown,
  ChevronRight,
  List,
  Network,
  Search,
} from "lucide-react";

import { api, ApiError } from "@/lib/api";
import type { TopologyGroup, TopologyNode, TopologyResponse } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DetailDrawer, EmptyState, ForbiddenInline } from "@/components/kit";
import { useNamespace } from "@/lib/namespace";
import { cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type ViewMode = "graph" | "list";
type LoadState =
  | { kind: "loading" }
  | { kind: "error"; message: string; forbidden: boolean }
  | { kind: "ready"; data: TopologyResponse };

// ---------------------------------------------------------------------------
// HealthDots — compact health rollup for a group card
// ---------------------------------------------------------------------------

function HealthDots({ health }: { health: TopologyGroup["health"] }) {
  const total = health.ready + health.notReady + health.pending + health.unknown;
  if (total === 0) {
    return <span className="text-[10px] text-muted-foreground">no agents</span>;
  }
  return (
    <div className="flex items-center gap-1.5 text-[10px]" data-testid="health-dots">
      {health.ready > 0 && (
        <span className="flex items-center gap-0.5">
          <span className="h-2 w-2 rounded-full bg-success" />
          {health.ready}
        </span>
      )}
      {health.pending > 0 && (
        <span className="flex items-center gap-0.5">
          <span className="h-2 w-2 rounded-full bg-warning" />
          {health.pending}
        </span>
      )}
      {health.notReady > 0 && (
        <span className="flex items-center gap-0.5">
          <span className="h-2 w-2 rounded-full bg-destructive" />
          {health.notReady}
        </span>
      )}
      {health.unknown > 0 && (
        <span className="flex items-center gap-0.5">
          <span className="h-2 w-2 rounded-full bg-muted-foreground" />
          {health.unknown}
        </span>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// GroupCard — one collapsible registry/namespace group in graph view
// ---------------------------------------------------------------------------

function GroupCard({
  group,
  expanded,
  onToggle,
}: {
  group: TopologyGroup;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      data-testid={`group-card-${group.id}`}
      onClick={onToggle}
      className="w-56 rounded-lg border border-border-strong bg-card p-3 text-left shadow-card transition-shadow hover:shadow-elevated"
    >
      <div className="mb-2 flex items-center gap-2">
        <div className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-accent-foreground">
          <Boxes className="h-4 w-4" />
        </div>
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium" data-testid={`group-label-${group.id}`}>
            {group.label}
          </p>
          <p className="truncate text-[11px] text-muted-foreground">{group.namespace}</p>
        </div>
        {expanded ? (
          <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
        ) : (
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
        )}
      </div>
      <div className="flex items-center justify-between">
        <span className="text-xs text-muted-foreground">
          {group.memberCount} {group.memberCount === 1 ? "agent" : "agents"}
        </span>
        <HealthDots health={group.health} />
      </div>
    </button>
  );
}

// ---------------------------------------------------------------------------
// AgentNodeCard — an expanded individual agent in graph view
// ---------------------------------------------------------------------------

function AgentNodeCard({
  node,
  onClick,
}: {
  node: TopologyNode;
  onClick: () => void;
}) {
  const healthColor: Record<string, string> = {
    ready: "border-success text-success",
    notReady: "border-destructive text-destructive",
    pending: "border-warning text-warning",
    unknown: "border-muted-foreground text-muted-foreground",
  };
  return (
    <button
      type="button"
      data-testid={`agent-node-${node.id}`}
      onClick={onClick}
      className={cn(
        "min-w-36 rounded-md border-2 bg-card px-3 py-2 text-left shadow-card transition-shadow hover:shadow-elevated",
        healthColor[node.health] ?? "border-muted-foreground text-muted-foreground",
      )}
    >
      <p className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">Agent</p>
      <p className="truncate text-sm font-semibold text-card-foreground">{node.name}</p>
      {node.detail && (
        <p className="truncate font-mono text-[10px] text-muted-foreground">{node.detail}</p>
      )}
    </button>
  );
}

// ---------------------------------------------------------------------------
// GroupRow — one row in the list view
// ---------------------------------------------------------------------------

function GroupRow({
  group,
  expanded,
  onToggle,
  onNodeClick,
  expandedNodes,
}: {
  group: TopologyGroup;
  expanded: boolean;
  onToggle: () => void;
  onNodeClick: (node: TopologyNode) => void;
  expandedNodes: TopologyNode[];
}) {
  return (
    <div>
      <button
        type="button"
        data-testid={`group-row-${group.id}`}
        onClick={onToggle}
        className="flex w-full items-center gap-3 rounded-md border bg-card px-4 py-3 text-left shadow-card hover:bg-surface-2"
      >
        <Boxes className="h-4 w-4 text-primary shrink-0" />
        <div className="flex-1 min-w-0">
          <p className="text-sm font-medium" data-testid={`group-row-label-${group.id}`}>
            {group.label}
          </p>
          <p className="text-xs text-muted-foreground">
            {group.namespace} · {group.memberCount}{" "}
            {group.memberCount === 1 ? "agent" : "agents"}
          </p>
        </div>
        <div className="flex items-center gap-2 text-xs">
          {group.health.ready > 0 && (
            <Badge variant="success" className="text-[10px]">
              {group.health.ready} ready
            </Badge>
          )}
          {group.health.notReady > 0 && (
            <Badge variant="destructive" className="text-[10px]">
              {group.health.notReady} failed
            </Badge>
          )}
          {group.health.pending > 0 && (
            <Badge variant="warning" className="text-[10px]">
              {group.health.pending} pending
            </Badge>
          )}
        </div>
        {expanded ? (
          <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
        ) : (
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
        )}
      </button>
      {/* Expanded member agents in list view */}
      {expanded && expandedNodes.length > 0 && (
        <div className="ml-10 mt-1 space-y-1">
          {expandedNodes.map((n) => (
            <button
              key={n.id}
              type="button"
              data-testid={`list-agent-node-${n.id}`}
              onClick={() => onNodeClick(n)}
              className="flex w-full items-center gap-3 rounded-md border border-border-subtle bg-surface-2 px-4 py-2 text-left text-sm hover:bg-surface-3"
            >
              <span className="flex-1 truncate font-medium">{n.name}</span>
              <span className="text-xs text-muted-foreground">{n.namespace}</span>
              <Badge
                variant={
                  n.health === "ready"
                    ? "success"
                    : n.health === "notReady"
                      ? "destructive"
                      : "secondary"
                }
                className="text-[10px]"
              >
                {n.health}
              </Badge>
            </button>
          ))}
          {group.truncated && (
            <p className="px-4 py-1 text-xs text-muted-foreground" data-testid="group-truncated">
              +{group.memberCount - group.shownCount} more agents (expand limit reached)
            </p>
          )}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// NodeDetailDrawer — click-through agent detail without leaving the map
// ---------------------------------------------------------------------------

function NodeDetailDrawer({
  node,
  onClose,
}: {
  node: TopologyNode | null;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  if (!node) return null;

  const healthBadge =
    node.health === "ready"
      ? "success"
      : node.health === "notReady"
        ? "destructive"
        : "secondary";

  return (
    <DetailDrawer
      open={!!node}
      onClose={onClose}
      title={node.name}
      subtitle={node.namespace}
      status={
        <Badge variant={healthBadge as "success" | "destructive" | "secondary"} className="text-[10px]">
          {node.health}
        </Badge>
      }
      footer={
        <Button
          size="sm"
          onClick={() => navigate(`/agents/${node.namespace}/${node.name}`)}
          data-testid="drawer-open-detail"
        >
          Open detail
        </Button>
      }
    >
      <div className="space-y-4" data-testid="node-drawer-content">
        <dl className="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
          <dt className="text-muted-foreground">Name</dt>
          <dd className="font-medium" data-testid="drawer-agent-name">
            {node.name}
          </dd>
          <dt className="text-muted-foreground">Namespace</dt>
          <dd data-testid="drawer-agent-ns">{node.namespace}</dd>
          <dt className="text-muted-foreground">Health</dt>
          <dd data-testid="drawer-agent-health">{node.health}</dd>
          {node.detail && (
            <>
              <dt className="text-muted-foreground">Image</dt>
              <dd className="font-mono text-xs break-all" data-testid="drawer-agent-detail">
                {node.detail}
              </dd>
            </>
          )}
        </dl>
      </div>
    </DetailDrawer>
  );
}

// ---------------------------------------------------------------------------
// TopologyPage
// ---------------------------------------------------------------------------

export function TopologyPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  // The header namespace scope filters the graph (m24.3) — "" = cluster-wide.
  const { namespace } = useNamespace();
  const [state, setState] = React.useState<LoadState>({ kind: "loading" });
  const [view, setView] = React.useState<ViewMode>("graph");
  const [searchInput, setSearchInput] = React.useState("");
  const [expandedGroups, setExpandedGroups] = React.useState<Set<string>>(new Set());
  const [drawerNode, setDrawerNode] = React.useState<TopologyNode | null>(null);
  const searchTimerRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  // q from URL on initial load (so a link with ?q= works)
  const [q, setQ] = React.useState<string>(() => searchParams.get("q") ?? "");

  // Keep searchInput in sync with q (also on external URL changes)
  React.useEffect(() => {
    setSearchInput(q);
  }, [q]);

  // Debounce the search input → q change
  function handleSearchChange(value: string) {
    setSearchInput(value);
    if (searchTimerRef.current) clearTimeout(searchTimerRef.current);
    searchTimerRef.current = setTimeout(() => {
      setQ(value.trim());
    }, 300);
  }

  // Fetch topology on q or expandedGroups change
  const load = React.useCallback(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .topology(
        {
          group: "registry",
          q: q || undefined,
          expand: expandedGroups.size > 0 ? Array.from(expandedGroups) : undefined,
          namespace: namespace || undefined,
        },
        controller.signal,
      )
      .then((data) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", data });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load topology",
          forbidden: apiErr?.isForbidden ?? false,
        });
      });
    return () => controller.abort();
  }, [q, expandedGroups, namespace]);

  React.useEffect(() => load(), [load]);

  // Sync q to URL search params for shareable links
  React.useEffect(() => {
    setSearchParams(
      (p) => {
        if (q) {
          p.set("q", q);
        } else {
          p.delete("q");
        }
        return p;
      },
      { replace: true },
    );
  }, [q, setSearchParams]);

  function toggleGroup(id: string) {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }

  // nodesByGroup indexes the loaded nodes (expanded members) by their group id.
  // The group id for a registry member is "registry/<ns>/<name>"; for namespace
  // groups it is "namespace/<ns>". The node ids are
  // "<kind>/<ns>/<name>" — we derive the group key from the kind/ns of the node.
  function nodesForGroup(group: TopologyGroup, nodes: TopologyNode[]): TopologyNode[] {
    // Partition members by the AUTHORITATIVE group id the BFF stamped on each node
    // (node.group). Namespace matching is wrong when two registries share a namespace
    // — both would then claim every agent in it (the m25 shakedown bug). Fall back to
    // namespace only for older payloads that predate node.group.
    if (nodes.some((n) => n.group)) {
      return nodes.filter((n) => n.kind === "agent" && n.group === group.id);
    }
    return nodes.filter((n) => n.kind === "agent" && n.namespace === group.namespace);
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="topology-forbidden">
          <ForbiddenInline
            title="Not allowed to view topology"
            description="Your account can't list agent registries or deployments."
            detail={state.message}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="topology-error"
        >
          Couldn&apos;t load topology: {state.message}
        </div>
      </div>
    );
  }

  const groups = state.kind === "ready" ? (state.data.groups ?? []) : [];
  const nodes = state.kind === "ready" ? state.data.nodes : [];

  return (
    <div className="mx-auto max-w-6xl space-y-4" data-testid="topology-page">
      {/* Controls row */}
      <div className="flex flex-wrap items-center gap-3">
        {/* Search */}
        <div className="relative min-w-[18rem] flex-1">
          <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="Search agents, registries, namespaces…"
            className="pl-9"
            value={searchInput}
            onChange={(e) => handleSearchChange(e.target.value)}
            data-testid="topology-search"
          />
        </div>

        {/* View toggle */}
        <div className="flex items-center rounded-md border bg-card">
          <button
            type="button"
            data-testid="toggle-graph"
            onClick={() => setView("graph")}
            className={cn(
              "flex items-center gap-1.5 rounded-l-md px-3 py-1.5 text-sm transition-colors",
              view === "graph" ? "bg-accent text-accent-foreground" : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Network className="h-4 w-4" />
            Graph
          </button>
          <button
            type="button"
            data-testid="toggle-list"
            onClick={() => setView("list")}
            className={cn(
              "flex items-center gap-1.5 rounded-r-md px-3 py-1.5 text-sm transition-colors",
              view === "list" ? "bg-accent text-accent-foreground" : "text-muted-foreground hover:text-foreground",
            )}
          >
            <List className="h-4 w-4" />
            List
          </button>
        </div>

        {/* Agent count badge */}
        {state.kind === "ready" && groups.length > 0 && (
          <Badge variant="secondary" className="text-[10px]" data-testid="topology-count-badge">
            {groups.reduce((sum, g) => sum + g.memberCount, 0)} agents ·{" "}
            {groups.length} {groups[0]?.kind === "namespace" ? "namespaces" : "registries"}
          </Badge>
        )}
      </div>

      {/* Loading */}
      {state.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="topology-loading">
          Loading topology…
        </p>
      )}

      {/* Empty */}
      {state.kind === "ready" && groups.length === 0 && nodes.length === 0 && (
        <EmptyState
          icon={Network}
          title="No registries or agents"
          description={
            q
              ? "No agents match your search. Try a different query."
              : "Create an AgentRegistry and deploy some agents to see the topology."
          }
          data-testid="topology-empty"
        />
      )}

      {/* Graph view */}
      {state.kind === "ready" && view === "graph" && groups.length > 0 && (
        <div
          className="relative min-h-[28rem] overflow-auto rounded-lg border bg-[radial-gradient(hsl(var(--border))_1px,transparent_1px)] [background-size:22px_22px] p-8"
          data-testid="topology-graph-view"
        >
          <div className="flex flex-wrap gap-6">
            {groups.map((g) => {
              const isExpanded = expandedGroups.has(g.id) || q.length > 0;
              const memberNodes = isExpanded ? nodesForGroup(g, nodes) : [];
              return (
                <div key={g.id} className="space-y-3">
                  <GroupCard
                    group={g}
                    expanded={isExpanded}
                    onToggle={() => toggleGroup(g.id)}
                  />
                  {/* Expanded agent nodes beneath their group card */}
                  {isExpanded && memberNodes.length > 0 && (
                    <div className="ml-4 flex flex-col gap-2">
                      {memberNodes.map((n) => (
                        <AgentNodeCard
                          key={n.id}
                          node={n}
                          onClick={() => setDrawerNode(n)}
                        />
                      ))}
                      {g.truncated && (
                        <p
                          className="px-2 text-xs text-muted-foreground"
                          data-testid={`truncated-${g.id}`}
                        >
                          +{g.memberCount - g.shownCount} more
                        </p>
                      )}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
          {/* hint */}
          <div className="absolute left-3 top-3 rounded-md border bg-card/80 px-2.5 py-1 text-xs text-muted-foreground backdrop-blur">
            Grouped by registry — click to expand
          </div>
        </div>
      )}

      {/* List view */}
      {state.kind === "ready" && view === "list" && groups.length > 0 && (
        <div className="space-y-2" data-testid="topology-list-view">
          {groups.map((g) => {
            const isExpanded = expandedGroups.has(g.id) || q.length > 0;
            const memberNodes = isExpanded ? nodesForGroup(g, nodes) : [];
            return (
              <GroupRow
                key={g.id}
                group={g}
                expanded={isExpanded}
                onToggle={() => toggleGroup(g.id)}
                onNodeClick={(n) => setDrawerNode(n)}
                expandedNodes={memberNodes}
              />
            );
          })}
        </div>
      )}

      {/* Node detail drawer — click-through detail without leaving the map */}
      <NodeDetailDrawer node={drawerNode} onClose={() => setDrawerNode(null)} />
    </div>
  );
}
