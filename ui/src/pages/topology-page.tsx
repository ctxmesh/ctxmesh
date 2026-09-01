import * as React from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  ChevronDown,
  ChevronRight,
  List,
  Network,
  Search,
  Shield,
  Sparkles,
} from "lucide-react";

import { api, ApiError } from "@/lib/api";
import type { TopologyGroup, TopologyNode, TopologyResponse } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import {
  ClosingNote,
  DetailDrawer,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuantityValue,
  Skeleton,
  type KeyValueItem,
} from "@/components/kit";
import { buttonVariants } from "@/components/ui/button";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_AGENTS } from "@/lib/nav";
import { cn } from "@/lib/utils";

// TopologyPage — the fleet CANVAS (m15.13 Topology v2; M151 §6.1 archetype A6,
// §6.2 "grouped canvas + list toggle; drawer on node click").
//
// ── THE PAGE'S ONE IDEA: TWO VIEWS OF ONE TRUTH, NEITHER A CONSOLATION ──────
// A canvas answers "how is the fleet arranged?" and a list answers "which of
// these needs me?". Both questions are real, so both views render the SAME
// grouped payload with the SAME node grammar and the SAME status vocabulary —
// the only difference is the layout. That matters most at scale: a graph of 400
// nodes is a picture of a haystack, and the list is the view people will
// actually use. It is therefore built as a proper scannable table (aligned
// tracks, mono names, counts in a tabular column, health stated in words), not
// as a fallback rendered grudgingly.
//
// ── THE CANVAS IS A PAN SURFACE IN A FIXED FRAME (§4.6) ────────────────────
// The frame is a fixed-height bordered `bg-card` box; the map pans INSIDE it.
// The page around it never scrolls sideways, and the frame's silhouette does
// not move between loading, empty, forbidden, error and ready — every one of
// those states renders inside the same box, so the page never jumps.
//
// ── COLOUR (ADR 0128 §2.1 / §2.2 / §2.5) ───────────────────────────────────
// A node's KIND (registry / agent) is identity and takes the neutral register:
// a mono uppercase eyebrow, the word doing the work. Only HEALTH carries hue,
// and only through the two devices §2.2 allows — a status tag on its own tint,
// and `text-{hue}` on a numeric cell that carries the state ("1 failed"). Pine
// appears on this page exactly twice, both times as interactivity: the selected
// view chip and the links. Never on a node.
//
// data-testid contract (the black-box operator journey drives this page by it —
// see agent-brain/harness/ui-e2e/support/pages.ts):
//   topology-page / topology-search / toggle-graph / toggle-list
//   topology-graph-view / topology-list-view / topology-count-badge
//   topology-loading / topology-empty / topology-error / topology-forbidden
//   group-card-{id} / group-label-{id} / group-row-{id} / group-row-label-{id}
//   agent-node-{id} / list-agent-node-{id} / truncated-{id} / group-truncated
//   health-dots / node-drawer-content / drawer-agent-{name,ns,health,detail}
//   drawer-open-detail

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type ViewMode = "graph" | "list";
type LoadState =
  | { kind: "loading" }
  | { kind: "error"; message: string; forbidden: boolean }
  | { kind: "ready"; data: TopologyResponse };

type Health = TopologyNode["health"];

// ---------------------------------------------------------------------------
// The status vocabulary — health, and ONLY health, carries hue
// ---------------------------------------------------------------------------

/**
 * Topology health → the §5.1 tag vocabulary.
 *
 * Deliberately NOT routed through `resolveStatus`/`StatusBadge`: that helper
 * reads free-form phase/reason text, and the literal string "unknown" falls
 * through its ladder to the `failed` tone. A cluster that reported no status
 * for an agent has not told us the agent is broken — it has told us nothing,
 * and rendering silence as a failure is the same lie as rendering it as zero
 * (§7.1). "Unknown" therefore takes the dashed `open` tag, which is the
 * console's word for "declared, but nothing has been observed".
 */
const HEALTH_TAG: Record<Health, "ok" | "crit" | "progressing" | "open"> = {
  ready: "ok",
  notReady: "crit",
  pending: "progressing",
  unknown: "open",
};

const HEALTH_LABEL: Record<Health, string> = {
  ready: "Ready",
  notReady: "Not ready",
  pending: "Pending",
  unknown: "Unknown",
};

const UNKNOWN_HEALTH_TITLE =
  "No status was reported for this agent — unknown, not unhealthy.";

function HealthTag({ health }: { health: Health }) {
  return (
    <Badge
      variant={HEALTH_TAG[health]}
      title={health === "unknown" ? UNKNOWN_HEALTH_TITLE : undefined}
    >
      {HEALTH_LABEL[health]}
    </Badge>
  );
}

// ---------------------------------------------------------------------------
// HealthLine — a group's rollup, worst first, in words
// ---------------------------------------------------------------------------

/**
 * The fleet rollup for one group. §2.2's second sanctioned hue device: the
 * colour sits on a numeric cell that CARRIES the state, never on a surface.
 *
 * Order is the §6.1 attention order — what is blocking comes first, "serving"
 * comes last — so a single failure is never hidden behind a healthy majority.
 * `pending` and `unknown` are neutral on purpose: converging needs no person
 * (§2.5), and an unreported status is not a claim at all.
 */
function HealthLine({ health }: { health: TopologyGroup["health"] }) {
  const total = health.ready + health.notReady + health.pending + health.unknown;
  if (total === 0) {
    return (
      <span
        data-testid="health-dots"
        className="font-mono text-2xs uppercase tracking-wide text-faint"
      >
        no agents
      </span>
    );
  }
  return (
    <div
      data-testid="health-dots"
      className="flex min-w-0 flex-wrap items-baseline justify-end gap-x-2 gap-y-0.5 font-mono text-2xs tabular-nums"
    >
      {health.notReady > 0 && (
        <span className="text-destructive">{health.notReady} failed</span>
      )}
      {health.pending > 0 && <span className="text-faint">{health.pending} pending</span>}
      {health.unknown > 0 && (
        <span className="text-faint" title={UNKNOWN_HEALTH_TITLE}>
          {health.unknown} unknown
        </span>
      )}
      {health.ready > 0 && <span className="text-success">{health.ready} ready</span>}
    </div>
  );
}

/** Worst-first: the ONE thing about a group that asks for attention. */
function groupAccent(health: TopologyGroup["health"]): string {
  // Only a failure earns the accent rule. "Everything is fine" and "the machine
  // is still converging" are not attention states, and an accent that is always
  // on is an accent that says nothing (§2.2 — annotation, not alarm).
  return health.notReady > 0 ? "border-l-destructive" : "border-l-border-strong";
}

// ---------------------------------------------------------------------------
// The canvas frame (§6.1 A6, §4.6)
// ---------------------------------------------------------------------------

/**
 * The bordered `bg-card` box the map pans inside. Fixed height so the page
 * silhouette is identical in every state, `overflow-auto` so the map — not the
 * document — is what scrolls.
 *
 * NOTE (recorded deviation): this grammar is shared verbatim with
 * `workflow-detail-page.tsx`, the other A6 surface. It belongs in `kit/` as a
 * `Canvas` primitive; the M151 page-conversion fence puts `components/kit/`
 * off-limits to this task, so the two pages carry the same class strings and a
 * pointer to each other instead. Lifting it is carded for the backlog.
 */
const CANVAS_FRAME =
  "relative min-h-[35rem] max-h-[42rem] min-w-0 overflow-auto rounded-lg border border-border bg-card p-6";

/** The pan surface's dot grid — a token consumed, never a colour invented. */
const CANVAS_GRID =
  "bg-[radial-gradient(hsl(var(--border))_1px,transparent_1px)] [background-size:22px_22px]";

function CanvasFrame({
  children,
  grid = true,
  className,
  ...rest
}: {
  children: React.ReactNode;
  grid?: boolean;
  className?: string;
} & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={cn(CANVAS_FRAME, grid && CANVAS_GRID, className)} {...rest}>
      {children}
    </div>
  );
}

/** §7 A6 loading: the frame with node-shaped blocks, and no edges. */
function CanvasSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading topology"
      data-testid="topology-loading"
      className="flex flex-wrap gap-6"
    >
      {[0, 1, 2, 3, 4].map((i) => (
        <div key={i} className="space-y-3">
          <Skeleton decorative className="h-[5.5rem] w-56 rounded-lg" />
          <Skeleton decorative className="ml-4 h-16 w-52 rounded-md" />
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// GroupCard — one collapsible registry/namespace group on the canvas
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
  // A registry is a TRUST BOUNDARY, and the console draws every trust boundary
  // the same way (ADR 0115): a dashed fence with the Shield mark. The dash is
  // the boundary; the left rule is the health inside it.
  return (
    <button
      type="button"
      data-testid={`group-card-${group.id}`}
      onClick={onToggle}
      aria-expanded={expanded}
      className={cn(
        "w-64 rounded-lg border border-dashed border-border-strong border-l-2 bg-card p-3 text-left transition-colors hover:bg-surface-2",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
        groupAccent(group.health),
      )}
    >
      <div className="mb-2 flex items-start gap-2">
        <Shield className="mt-0.5 h-4 w-4 shrink-0 text-faint" aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="font-mono text-2xs uppercase tracking-wide text-faint">
            registry
          </p>
          <p
            className="truncate font-mono text-sm font-medium"
            title={group.label}
            data-testid={`group-label-${group.id}`}
          >
            {group.label}
          </p>
          <p className="truncate font-mono text-2xs text-faint" title={group.namespace}>
            {group.namespace}
          </p>
        </div>
        {expanded ? (
          <ChevronDown className="h-4 w-4 shrink-0 text-faint" aria-hidden />
        ) : (
          <ChevronRight className="h-4 w-4 shrink-0 text-faint" aria-hidden />
        )}
      </div>
      <div className="flex items-baseline justify-between gap-2 border-t border-border-soft pt-2">
        <span className="shrink-0 whitespace-nowrap font-mono text-2xs tabular-nums text-faint">
          {group.memberCount} {group.memberCount === 1 ? "agent" : "agents"}
        </span>
        <HealthLine health={group.health} />
      </div>
    </button>
  );
}

// ---------------------------------------------------------------------------
// AgentNodeCard — an expanded individual agent on the canvas
// ---------------------------------------------------------------------------

function AgentNodeCard({
  node,
  onClick,
}: {
  node: TopologyNode;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      data-testid={`agent-node-${node.id}`}
      onClick={onClick}
      className={cn(
        "w-60 rounded-md border border-border bg-card px-3 py-2 text-left transition-colors hover:bg-surface-2",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
        // §6.1 A6's node grammar: the border says what STATE the node is in —
        // a crit rule when it is failing, a dashed frame when nothing has been
        // reported about it at all. A healthy node needs no rule.
        node.health === "notReady" && "border-l-2 border-l-destructive",
        node.health === "unknown" && "border-dashed",
      )}
    >
      <p className="font-mono text-2xs uppercase tracking-wide text-faint">Agent</p>
      <p className="truncate font-mono text-sm font-medium" title={node.name}>
        {node.name}
      </p>
      <div className="mt-1.5 flex items-center gap-2">
        <HealthTag health={node.health} />
      </div>
      {node.detail && (
        <p className="mt-1 truncate font-mono text-2xs text-faint" title={node.detail}>
          {node.detail}
        </p>
      )}
    </button>
  );
}

// ---------------------------------------------------------------------------
// The list view — the same data, as a table you can actually scan
// ---------------------------------------------------------------------------

/**
 * One track set shared by group rows, agent rows and the column heads, so the
 * three read as one object. The table lives in its own horizontal scroller with
 * a `min-w` floor (§4.6): below ~672px the TABLE scrolls, never the page.
 */
const LIST_GRID =
  "grid grid-cols-[minmax(0,1fr)_minmax(0,9rem)_4.5rem_minmax(0,10rem)] items-center gap-3";

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
    <div className="border-b border-border-soft last:border-b-0">
      <button
        type="button"
        data-testid={`group-row-${group.id}`}
        onClick={onToggle}
        aria-expanded={expanded}
        className={cn(
          LIST_GRID,
          "w-full px-4 py-2.5 text-left transition-colors hover:bg-surface-2",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
        )}
      >
        <span className="flex min-w-0 items-center gap-2">
          {expanded ? (
            <ChevronDown className="h-3.5 w-3.5 shrink-0 text-faint" aria-hidden />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 shrink-0 text-faint" aria-hidden />
          )}
          <Shield className="h-3.5 w-3.5 shrink-0 text-faint" aria-hidden />
          <span
            className="truncate font-mono text-sm font-medium"
            title={group.label}
            data-testid={`group-row-label-${group.id}`}
          >
            {group.label}
          </span>
        </span>
        <span className="truncate font-mono text-xs text-faint" title={group.namespace}>
          {group.namespace}
        </span>
        <span className="text-right" data-testid={`group-row-count-${group.id}`}>
          <QuantityValue value={group.memberCount} className="text-xs" />
        </span>
        <HealthLine health={group.health} />
      </button>

      {/* Expanded member agents. A sub-row is the sunk band (§4.1) so the
          hierarchy reads without an indent guessing game. */}
      {expanded && expandedNodes.length > 0 && (
        <div className="border-t border-border-soft bg-surface-2">
          {expandedNodes.map((n) => (
            <button
              key={n.id}
              type="button"
              data-testid={`list-agent-node-${n.id}`}
              onClick={() => onNodeClick(n)}
              className={cn(
                LIST_GRID,
                "w-full border-b border-border-soft px-4 py-2 text-left transition-colors last:border-b-0 hover:bg-surface-3",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
              )}
            >
              <span className="flex min-w-0 items-center gap-2 pl-6">
                <span className="truncate font-mono text-sm" title={n.name}>
                  {n.name}
                </span>
              </span>
              <span className="truncate font-mono text-xs text-faint" title={n.namespace}>
                {n.namespace}
              </span>
              {/* A member row has no member count. Inapplicable is not zero and
                  not unknown — it is simply nothing, and prints as nothing. */}
              <span aria-hidden="true" />
              <span>
                <HealthTag health={n.health} />
              </span>
            </button>
          ))}
          {group.truncated && (
            <p
              className="px-4 py-2 pl-14 font-mono text-2xs text-faint"
              data-testid="group-truncated"
            >
              +{group.memberCount - group.shownCount} more agents — the expand
              limit was reached. Search to narrow the group.
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
  if (!node) return null;

  const facts: KeyValueItem[] = [
    {
      key: "Name",
      value: <span data-testid="drawer-agent-name">{node.name}</span>,
      title: node.name,
    },
    {
      key: "Namespace",
      value: <span data-testid="drawer-agent-ns">{node.namespace}</span>,
      title: node.namespace,
    },
    {
      key: "Health",
      // The human word, not the raw enum: `notReady` is a wire value, and the
      // drawer is a reading surface.
      value: (
        <span data-testid="drawer-agent-health">{HEALTH_LABEL[node.health]}</span>
      ),
      title: node.health === "unknown" ? UNKNOWN_HEALTH_TITLE : node.health,
    },
    {
      key: "Image",
      value: node.detail ? (
        <span data-testid="drawer-agent-detail" className="break-all">
          {node.detail}
        </span>
      ) : undefined,
      absent: "not reported",
      title: "The topology payload carries no image for this agent.",
    },
  ];

  return (
    <DetailDrawer
      open={!!node}
      onClose={onClose}
      title={node.name}
      subtitle={node.namespace}
      status={<HealthTag health={node.health} />}
      footer={
        <Link
          to={`/agents/${encodeURIComponent(node.namespace)}/${encodeURIComponent(node.name)}`}
          data-testid="drawer-open-detail"
          className={buttonVariants({ size: "sm" })}
        >
          Open detail
        </Link>
      }
    >
      <div className="space-y-4" data-testid="node-drawer-content">
        <KeyValueList items={facts} />
      </div>
    </DetailDrawer>
  );
}

// ---------------------------------------------------------------------------
// TopologyPage
// ---------------------------------------------------------------------------

export function TopologyPage() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The header namespace scope filters the graph (m24.3) — "" = cluster-wide.
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_AGENTS, "create");
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

  function clearSearch() {
    if (searchTimerRef.current) clearTimeout(searchTimerRef.current);
    setSearchInput("");
    setQ("");
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
  // Partition members by the AUTHORITATIVE group id the BFF stamped on each node
  // (node.group). Namespace matching is wrong when two registries share a
  // namespace — both would then claim every agent in it (the m25 shakedown bug).
  // Fall back to namespace only for older payloads that predate node.group.
  function nodesForGroup(group: TopologyGroup, nodes: TopologyNode[]): TopologyNode[] {
    if (nodes.some((n) => n.group)) {
      return nodes.filter((n) => n.kind === "agent" && n.group === group.id);
    }
    return nodes.filter((n) => n.kind === "agent" && n.namespace === group.namespace);
  }

  const groups = state.kind === "ready" ? (state.data.groups ?? []) : [];
  const nodes = state.kind === "ready" ? state.data.nodes : [];
  const isEmpty = state.kind === "ready" && groups.length === 0 && nodes.length === 0;
  const agentTotal = groups.reduce((sum, g) => sum + g.memberCount, 0);
  const unitNoun = groups[0]?.kind === "namespace" ? "namespace" : "registry";
  const unitPlural = unitNoun === "namespace" ? "namespaces" : "registries";

  // ── The view toggle ────────────────────────────────────────────────────────
  //
  // RECORDED DEVIATION: this is the kit's `FilterChipRow` vocabulary, hand-rolled.
  // FilterChipRow is the right primitive and its radiogroup semantics are copied
  // exactly — but it takes no per-chip `data-testid`, and `toggle-graph` /
  // `toggle-list` are a SHIPPED black-box contract (the operator journey in
  // agent-brain/harness/ui-e2e drives the page through them). Rebuilding the
  // chip styling is the smaller sin than breaking a suite that cannot be edited
  // from here; adding a `testId` to `FilterChip` is carded for the backlog.
  const CHIP =
    "inline-flex items-center gap-1.5 rounded-sm border px-3 py-1.5 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background";
  const chipClass = (active: boolean) =>
    cn(
      CHIP,
      active
        ? // Selection is always pine-family, never a status hue (§2.3).
          "border-primary bg-accent font-semibold text-primary"
        : "border-border-strong bg-card text-secondary-foreground hover:bg-surface-2",
    );

  const viewToggle = (
    <div role="radiogroup" aria-label="Topology view" className="flex gap-2">
      <button
        type="button"
        role="radio"
        aria-checked={view === "graph"}
        tabIndex={view === "graph" ? 0 : -1}
        data-testid="toggle-graph"
        onClick={() => setView("graph")}
        className={chipClass(view === "graph")}
      >
        <Network className="h-4 w-4" aria-hidden />
        Graph
      </button>
      <button
        type="button"
        role="radio"
        aria-checked={view === "list"}
        tabIndex={view === "list" ? 0 : -1}
        data-testid="toggle-list"
        onClick={() => setView("list")}
        className={chipClass(view === "list")}
      >
        <List className="h-4 w-4" aria-hidden />
        List
      </button>
    </div>
  );

  // ── What goes inside the frame, in every state (§7 A6) ────────────────────
  let frame: React.ReactNode;
  if (state.kind === "loading") {
    frame = (
      <CanvasFrame>
        <CanvasSkeleton />
      </CanvasFrame>
    );
  } else if (state.kind === "error" && state.forbidden) {
    frame = (
      <CanvasFrame grid={false} className="grid place-items-center">
        <div className="w-full max-w-lg" data-testid="topology-forbidden">
          <ForbiddenInline
            title="You don't have permission to view the topology."
            description="It reads agent registries and deployments across the cluster."
            resource="agent registries"
            detail={state.message}
          />
        </div>
      </CanvasFrame>
    );
  } else if (state.kind === "error") {
    frame = (
      <CanvasFrame grid={false} className="grid place-items-center">
        <div className="w-full max-w-lg" data-testid="topology-error">
          <ErrorState
            title="The topology didn't load."
            description="The fleet itself is unaffected — only the map failed to read."
            detail={state.message}
            onRetry={() => load()}
          />
        </div>
      </CanvasFrame>
    );
  } else if (isEmpty) {
    frame = (
      <CanvasFrame grid={false} className="grid place-items-center">
        <div className="w-full max-w-lg" data-testid="topology-empty">
          {q ? (
            <EmptyState
              intent="filtered"
              icon={Search}
              title="No nodes match"
              description={`Nothing in the map matches “${q}”. Clear the search to see the whole fleet.`}
              action={{
                label: "Clear the search",
                variant: "outline",
                onClick: clearSearch,
              }}
            />
          ) : (
            <EmptyState
              icon={Network}
              title="Nothing to draw yet"
              description="The map shows every registry and the agents inside it. It appears as soon as the first agent is created."
              action={
                canCreate
                  ? {
                      label: "New agent",
                      icon: Sparkles,
                      onClick: () => navigate("/agents/new"),
                    }
                  : undefined
              }
            />
          )}
        </div>
      </CanvasFrame>
    );
  } else if (view === "graph") {
    frame = (
      <CanvasFrame data-testid="topology-graph-view">
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
                        className="px-2 font-mono text-2xs text-faint"
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
      </CanvasFrame>
    );
  } else {
    frame = (
      <div
        className="overflow-x-auto rounded-lg border border-border bg-card"
        data-testid="topology-list-view"
      >
        <div className="min-w-[42rem]">
          {/* Column heads in the §4.8 mono eyebrow register. Hidden from
              assistive tech — each row states its own values in full. */}
          <div
            aria-hidden="true"
            className={cn(
              LIST_GRID,
              "border-b border-border px-4 py-2 font-mono text-2xs uppercase tracking-wide text-faint",
            )}
          >
            <span>Registry / agent</span>
            <span>Namespace</span>
            <span className="text-right">Agents</span>
            <span>Health</span>
          </div>
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
      </div>
    );
  }

  return (
    <div className="min-w-0 space-y-5" data-testid="topology-page">
      <PageHeader
        title="Topology"
        lede="Every registry in the cluster and the agents inside it. Open a registry to see its members; open a member to see what it is."
        actionsSlot={viewToggle}
      />

      {/* The canvas chrome: one filter over both views, and the honest count. */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative min-w-0 flex-1 basis-64">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-faint"
            aria-hidden
          />
          <Input
            placeholder="Filter agents, registries, namespaces…"
            aria-label="Filter the topology"
            className="pl-9"
            value={searchInput}
            onChange={(e) => handleSearchChange(e.target.value)}
            data-testid="topology-search"
          />
        </div>
        {state.kind === "ready" && groups.length > 0 && (
          // Counts come from the BFF's own memberCount, never from the rows in
          // hand — a truncated group still reports its true size.
          <p
            className="font-mono text-xs tabular-nums text-faint"
            data-testid="topology-count-badge"
          >
            {agentTotal} agents · {groups.length}{" "}
            {groups.length === 1 ? unitNoun : unitPlural}
          </p>
        )}
      </div>

      {frame}

      {state.kind === "ready" && groups.length > 0 && (
        <ClosingNote>
          {q
            ? `Filtered to “${q}”. Clear the filter to see the whole fleet.`
            : view === "graph"
              ? "Grouped by registry — open one to see the agents inside it."
              : "The same fleet as the map, in the order you can scan it."}
        </ClosingNote>
      )}

      {/* Node detail drawer — click-through detail without leaving the map */}
      <NodeDetailDrawer node={drawerNode} onClose={() => setDrawerNode(null)} />
    </div>
  );
}
