import * as React from "react";
import {
  BookOpen,
  CheckCircle,
  ChevronRight,
  Clock,
  Code2,
  Loader2,
  RefreshCw,
  Shield,
  User,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ClosingNote,
  EmptyState,
  ErrorState,
  FilterChipRow,
  ForbiddenInline,
  PageHeader,
  SkeletonCard,
  StatusBadge,
  Wizard,
  useToast,
  type FilterChip,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type AgentSummary,
  type CatalogTool,
  type MCPToolBindingDetail,
} from "@/lib/api";

// ToolCatalogPage — archetype A9, the gallery (M151 spec §6.1/§6.2: "curated /
// user-added / pending as Tag-differentiated cards; pending cards' CTA disabled
// with the reason line"). Lists ALL tools from GET /api/tools in three distinct
// states:
//   • curated    — from the built-in ToolRegistry; always bindable
//   • user-added — brought in through the BYO-MCP flow + approved; bindable
//   • pending-approval — queued in the approval gate (m17.4); NOT bindable
//
// ── THE THREE STATES ARE THREE DIFFERENT CLAIMS ─────────────────────────────
// They are not three severities of the same thing: they say who vouched for the
// tool. Curated means this install shipped it. User-added means a person here
// brought it in and an operator let it through. Pending means NOBODY has
// decided yet — which is the hold state (§2.2/§2.4: work paused because a
// person must decide), not a warning and not a failure. So the three never
// render alike: `muted` chip + shield, dashed `open` chip + person, hold chip +
// clock — and the pending card's CTA is dead with the reason written next to
// it. A card you cannot use must say why; a disabled button that explains
// nothing just looks broken.
//
// The reason line used to be there and could not be READ: it was
// `text-warning-foreground`, which is the ink for text ON a filled warning
// surface — pure white in light theme (`--warning-foreground: 0 0% 100%`), on a
// white card. It now renders in the hold hue, which is both the correct
// semantic and legible in both themes.
//
// ── WHY A GRID OF CARDS AND NOT THE COLLAPSIBLE SERVER TREE ─────────────────
// The catalog was a tree of MCP-server groups (m25 S11). §6.2 assigns this page
// A9, and the tree fought the archetype: the server was a header row, so the
// only way to learn where a tool came from was to remember which group you had
// scrolled into. Each card now carries its own provenance line, and the cards
// sort by what you can DO with them (bindable first, pending last) rather than
// alphabetically by server.
//
// ── SORT: USABLE FIRST, DELIBERATELY NOT ATTENTION-FIRST ────────────────────
// A1's doctrine puts whatever is blocking at the top. This page inverts it on
// purpose: the reader is shopping, and a pending tool's next step belongs to an
// OPERATOR, not to them — there is no action the browsing user can take on it.
// Leading a discovery surface with things nobody browsing can use would be
// attention-first applied where it does the reader no good.
//
// Bind wizard: opens from any curated/user-added tool. It binds the tool to a
// selected agent via POST /api/mcptoolbindings, then polls the binding detail
// for the controller's Ready condition. The propagation status is surfaced
// HONESTLY from that condition — "propagated" only when Ready=True, otherwise
// the reason. A pending-approval tool cannot open the wizard (button disabled).
//
// RBAC: listing tools is accessible to any authenticated caller (403 renders
// ForbiddenInline). Binding is gated on mcptoolbindings/create; the real gate
// is the API 403, surfaced honestly via the wizard (ADR 0011).

// ---- catalog state types ----------------------------------------------------

type ToolState = "curated" | "user-added" | "pending-approval";

/**
 * The origin class of one tool.
 *
 * `source` is the ORIGIN CLASS ("user-added" | "curated"), not the server name —
 * see the `CatalogTool` contract in lib/api.ts. The old heuristic read any
 * non-empty `source` as user-added, which quietly labelled every curated tool
 * the BFF marks `source: "curated"` as "user-added": the catalog rendered two
 * provenance states where the backend sent three. Older BFFs send the MCP
 * server's name in this field, so anything that is neither empty nor the
 * literal "curated" is still read as user-added.
 */
function toolState(t: CatalogTool): ToolState {
  if (t.approvalStatus === "pending") return "pending-approval";
  if (t.source && t.source.trim() && t.source.trim() !== "curated") return "user-added";
  return "curated";
}

/** Usable first (§ the sort note above); within a state, by server then name. */
const STATE_ORDER: Record<ToolState, number> = {
  curated: 0,
  "user-added": 1,
  "pending-approval": 2,
};

/** Where the tool came from — the MCP server, or this install's own registry. */
function originOf(t: CatalogTool): string {
  const registry = t.registry?.trim();
  return registry ? `from ${registry}` : "from the built-in registry";
}

/** Frozen empty, so a not-yet-loaded catalog doesn't churn the memos. */
const NO_TOOLS: CatalogTool[] = [];

type FilterState = "all" | ToolState;

const VIEWS: { id: FilterState; label: string }[] = [
  { id: "all", label: "Everything" },
  { id: "curated", label: "Curated" },
  { id: "user-added", label: "User-added" },
  { id: "pending-approval", label: "Awaiting approval" },
];

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; tools: CatalogTool[] }
  | { kind: "empty" }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

// ---- bind wizard state ------------------------------------------------------

type WizardState =
  | { kind: "closed" }
  | { kind: "open"; tool: CatalogTool };

// Step indices for the bind wizard
const STEP_SELECT_AGENT = 0;
const STEP_CONFIRM = 1;
const STEP_RESULT = 2;

type BindState =
  | { kind: "idle" }
  | { kind: "loading-agents" }
  | { kind: "agents-error"; message: string }
  | { kind: "binding" }
  | { kind: "polling" }
  | { kind: "done"; detail: MCPToolBindingDetail }
  | { kind: "error"; message: string; forbidden?: boolean };

/**
 * The §5.18 closing line: the catalog's split across its three provenance
 * claims, counted from the tools in hand. `/api/tools` returns the whole
 * catalog in one response — there is no cursor — so these are facts about the
 * catalog, not about a page of it (which is also why the chips carry numbers).
 */
export function catalogClosingLine(tools: CatalogTool[]): string | null {
  const total = tools.length;
  if (total === 0) return null;
  const pending = tools.filter((t) => toolState(t) === "pending-approval").length;
  const bindable = total - pending;
  const noun = `${total} tool${total === 1 ? "" : "s"}`;
  if (pending === 0) {
    return `Every one of the ${noun} here has been approved — there is nothing waiting on an operator.`;
  }
  if (bindable === 0) {
    return `All ${noun} here are waiting on an operator's decision. None of them can be bound yet.`;
  }
  return `${noun} here: ${bindable} you can bind now, ${pending} still waiting on an operator's decision.`;
}

// ---- main page --------------------------------------------------------------

export function ToolCatalogPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [filter, setFilter] = React.useState<FilterState>("all");
  const [q, setQ] = React.useState("");
  const [wizard, setWizard] = React.useState<WizardState>({ kind: "closed" });

  const { can } = useCapabilities();
  // Binding is a write op — gated on mcptoolbindings/create. Display-only;
  // real gate is the API 403 if the caller somehow submits without the right.
  const canBind = can("mcptoolbindings", "create");

  const load = React.useCallback((signal?: AbortSignal) => {
    setPage({ kind: "loading" });
    api
      .listTools(signal)
      .then((res) => {
        if (signal?.aborted) return;
        if (res.tools.length === 0) {
          setPage({ kind: "empty" });
        } else {
          setPage({ kind: "ready", tools: res.tools });
        }
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError) {
          if (err.isForbidden) {
            setPage({ kind: "forbidden", message: err.message });
            return;
          }
          setPage({ kind: "error", message: err.message });
          return;
        }
        setPage({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // Stable across renders while `page` is unchanged — the sort/filter memos
  // below key off it.
  const tools = React.useMemo(
    () => (page.kind === "ready" ? page.tools : NO_TOOLS),
    [page],
  );

  // Sorted once: usable before pending, then by the server that supplies it.
  const sorted = React.useMemo(
    () =>
      [...tools].sort(
        (a, b) =>
          STATE_ORDER[toolState(a)] - STATE_ORDER[toolState(b)] ||
          (a.registry ?? "").localeCompare(b.registry ?? "") ||
          a.name.localeCompare(b.name),
      ),
    [tools],
  );

  // Derived: filter the tool list by state + name query
  const displayedTools = React.useMemo<CatalogTool[]>(() => {
    let list = sorted;
    if (filter !== "all") {
      list = list.filter((t) => toolState(t) === filter);
    }
    if (q.trim()) {
      const lower = q.trim().toLowerCase();
      list = list.filter(
        (t) =>
          t.name.toLowerCase().includes(lower) ||
          (t.description ?? "").toLowerCase().includes(lower),
      );
    }
    return list;
  }, [sorted, filter, q]);

  // Counts are facts here: the catalog arrives whole, so a count of the tools
  // in hand IS the catalog's count (the FilterChipRow contract's one condition).
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: v.id === "all" ? tools.length : tools.filter((t) => toolState(t) === v.id).length,
  }));

  return (
    <div className="min-w-0 space-y-6" data-testid="tool-catalog">
      <PageHeader
        title="Tool catalog"
        lede="Every tool this namespace can reach, and who vouched for it. Bind an approved one to an agent to put it in that agent's managed loop."
      />

      {page.kind === "loading" && (
        <div className="grid auto-rows-fr gap-5 sm:grid-cols-2 xl:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <SkeletonCard key={i} />
          ))}
        </div>
      )}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to view the tool catalog"
          description="Reading the tool catalog requires list permission on tools."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load the tool catalog"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "empty" && (
        <EmptyState
          icon={BookOpen}
          title="No tools in the catalog"
          description="Add a BYO-MCP server to discover tools, or contact your operator to set up curated tools."
        />
      )}

      {page.kind === "ready" && (
        <>
          {/* Views, not filters (§5.28) — one question, one answer at a time. */}
          <div className="flex flex-wrap items-center gap-3">
            <FilterChipRow
              chips={chips}
              value={filter}
              onChange={(id) => setFilter(id as FilterState)}
              label="Filter tools"
              className="min-w-0 flex-1"
            />
            <Button
              variant="ghost"
              size="icon"
              onClick={() => load()}
              aria-label="Refresh catalog"
              data-testid="catalog-refresh"
            >
              <RefreshCw className="h-4 w-4" />
            </Button>
          </div>

          <div className="relative max-w-sm">
            <Input
              placeholder="Filter tools…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              data-testid="catalog-filter-input"
            />
          </div>

          {displayedTools.length === 0 ? (
            <EmptyState
              icon={BookOpen}
              intent="filtered"
              title="Nothing matches"
              description={
                q.trim()
                  ? `No tool in the catalog matches “${q.trim()}”. Clear the search to see the rest.`
                  : filter === "pending-approval"
                    ? "Nothing is waiting on an operator. Show everything to see the tools you can bind."
                    : "No tool in the catalog came from there. Show everything to see the rest."
              }
              totalCount={tools.length}
              countNoun="tools"
              action={{
                label: q.trim() ? "Clear the search" : "Show everything",
                variant: "outline",
                onClick: () => {
                  setQ("");
                  setFilter("all");
                },
              }}
            />
          ) : (
            <ul
              className="grid auto-rows-fr gap-5 sm:grid-cols-2 xl:grid-cols-3"
              data-testid="catalog-tool-list"
            >
              {displayedTools.map((tool) => (
                <ToolCard
                  key={`${tool.registry ?? ""}/${tool.name}`}
                  tool={tool}
                  canBind={canBind}
                  onBind={() => setWizard({ kind: "open", tool })}
                />
              ))}
            </ul>
          )}

          <ClosingNote>{catalogClosingLine(tools)}</ClosingNote>
        </>
      )}

      {/* Bind wizard — mounted when open */}
      {wizard.kind === "open" && (
        <BindWizard
          tool={wizard.tool}
          onClose={() => setWizard({ kind: "closed" })}
        />
      )}
    </div>
  );
}

// ---- ToolCard ---------------------------------------------------------------
// The A9 card anatomy: identity → provenance → 2-line description → Tag row →
// (the reason, when there is one) → right-aligned CTA footer. Equal height by
// `auto-rows-fr` on the grid + `mt-auto` on the footer, so a card never grows
// to fit its longest field and every CTA sits on the same baseline.
//
// A card has no column to bound it, so every line that can carry a machine name
// truncates on one line with the full value in `title` (§4.5) — never
// `break-all`, and never a name allowed to set the card's width.

interface ToolCardProps {
  tool: CatalogTool;
  canBind: boolean;
  onBind: () => void;
}

function ToolCard({ tool, canBind, onBind }: ToolCardProps) {
  const state = toolState(tool);
  const isPending = state === "pending-approval";
  const hasSchema = tool.inputSchema !== undefined && tool.inputSchema !== null;
  const [showSchema, setShowSchema] = React.useState(false);
  const origin = originOf(tool);

  return (
    <li
      className="flex min-w-0 flex-col rounded-lg border border-border bg-card p-5"
      data-testid={`catalog-tool-${tool.name}`}
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <ToolStateIcon state={state} />
        <div className="min-w-0 flex-1">
          {/* A tool name is a machine identifier — the literal string the model
              emits — so it stays mono where a gallery title would be serif. */}
          <h3 className="truncate font-mono text-md font-medium" title={tool.name}>
            {tool.name}
          </h3>
          <p className="mt-1 truncate font-mono text-xs text-faint" title={origin}>
            {origin}
          </p>
        </div>
      </div>

      {tool.description && (
        <p
          className="mt-3 line-clamp-2 text-sm text-secondary-foreground"
          title={tool.description}
        >
          {tool.description}
        </p>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-1.5">
        <ToolStateBadge state={state} toolName={tool.name} />
      </div>

      {isPending && (
        // §6.2: a pending card's CTA is disabled WITH the reason line. The hue
        // is hold, not warn — nothing is degraded, a person simply has not
        // decided yet (§2.2), and the person is not the reader.
        <p
          className="mt-3 text-xs text-hold"
          data-testid={`catalog-pending-reason-${tool.name}`}
        >
          An operator has to approve this tool before anyone can bind it. Nothing
          you do here changes that — the decision is theirs.
        </p>
      )}

      {showSchema && hasSchema && (
        // A code well, own-container scrolling (§4.6/§4.5): structured JSON
        // keeps its indentation and scrolls sideways inside its own frame.
        <pre
          className="mt-3 max-h-64 overflow-auto rounded-md border border-border bg-surface-3 p-3 font-mono text-xs text-secondary-foreground"
          data-testid={`catalog-schema-${tool.name}`}
        >
          {JSON.stringify(tool.inputSchema, null, 2)}
        </pre>
      )}

      <div className="mt-auto flex flex-wrap items-center justify-end gap-2 pt-4">
        {hasSchema && (
          // A disclosure, not a destination: a quiet ghost control, so the pine
          // CTA stays the only pine affordance on the card (§6.1 A9).
          <Button
            variant="ghost"
            size="sm"
            className="mr-auto text-faint"
            onClick={() => setShowSchema((s) => !s)}
            data-testid={`catalog-schema-toggle-${tool.name}`}
            aria-expanded={showSchema}
          >
            <Code2 className="mr-1.5 h-3.5 w-3.5" />
            {showSchema ? "hide schema" : "schema"}
          </Button>
        )}
        <Button
          size="sm"
          // Disabled renders as the outline shape, never a greyed pine slab:
          // a dead primary button reads as available at a glance (§2.3).
          variant={isPending || !canBind ? "outline" : "default"}
          disabled={isPending || !canBind}
          onClick={onBind}
          title={
            isPending
              ? "Tool is pending operator approval and cannot be bound"
              : !canBind
              ? "You don't have permission to bind tools"
              : `Bind ${tool.name} to an agent`
          }
          data-testid={`catalog-bind-${tool.name}`}
        >
          Bind to agent
          <ChevronRight className="ml-1 h-3.5 w-3.5" />
        </Button>
      </div>
    </li>
  );
}

// ---- state icon + badge helpers ---------------------------------------------
// Three claims, three treatments — they may never look alike (§6.2). The two
// vouched-for states carry NO hue: provenance is a declared fact, not a health
// reading, so they use the two non-hue Tag variants (§5.6) and are separated by
// form — a solid sunk chip for "this install ships it", the dashed `open` chip
// for "somebody here brought it in". Only the third state is a real state: work
// paused because a person must decide, which is hold (§2.2/§2.4).

function ToolStateIcon({ state }: { state: ToolState }) {
  if (state === "curated") {
    return <Shield className="mt-0.5 h-4 w-4 shrink-0 text-faint" />;
  }
  if (state === "user-added") {
    return <User className="mt-0.5 h-4 w-4 shrink-0 text-faint" />;
  }
  return <Clock className="mt-0.5 h-4 w-4 shrink-0 text-hold" />;
}

function ToolStateBadge({
  state,
  toolName,
}: {
  state: ToolState;
  toolName: string;
}) {
  if (state === "curated") {
    return (
      <Badge variant="muted" data-testid={`catalog-tool-state-${toolName}`}>
        curated
      </Badge>
    );
  }
  if (state === "user-added") {
    return (
      <Badge variant="open" data-testid={`catalog-tool-state-${toolName}`}>
        user-added
      </Badge>
    );
  }
  return (
    <Badge variant="hold" data-testid={`catalog-tool-state-${toolName}`}>
      pending-approval
    </Badge>
  );
}

// ---- BindWizard -------------------------------------------------------------
// A 3-step wizard:
//   0. Select agent (namespace + name from the agents list)
//   1. Confirm (shows tool + agent, warns about pending if somehow opened)
//   2. Result (binding created → poll propagation status from binding detail)
//
// Propagation status is surfaced HONESTLY from the controller's Ready condition:
//   Ready=True  → propagationStatus="propagated" → "Propagated (hot-updated live)"
//   Ready=False → propagationStatus=reason       → "Not propagated: <reason>"
//   Still polling → "Checking propagation status…"
//
// The poll runs up to MAX_POLL_ATTEMPTS times with POLL_INTERVAL_MS delay.
// After exhausting polls it shows the last status faithfully — NEVER upgrades
// "not-ready" to "propagated".

const POLL_INTERVAL_MS = 2000;
const MAX_POLL_ATTEMPTS = 15; // 30 s max

interface BindWizardProps {
  tool: CatalogTool;
  onClose: () => void;
}

function BindWizard({ tool, onClose }: BindWizardProps) {
  const [step, setStep] = React.useState(STEP_SELECT_AGENT);
  const [agents, setAgents] = React.useState<AgentSummary[]>([]);
  const [selectedNs, setSelectedNs] = React.useState("");
  const [selectedName, setSelectedName] = React.useState("");
  const [bindName, setBindName] = React.useState("");
  const [bindNs, setBindNs] = React.useState("");
  const [bindState, setBindState] = React.useState<BindState>({ kind: "idle" });
  const { namespace: shellNs } = useNamespace();
  const { can, reprobe } = useCapabilities();
  const { toast } = useToast();

  // Load agents for the select step
  React.useEffect(() => {
    if (step !== STEP_SELECT_AGENT) return;
    setBindState({ kind: "loading-agents" });
    const controller = new AbortController();
    api
      .listAgents({ namespace: shellNs || undefined }, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setAgents(res.items);
        setBindState({ kind: "idle" });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const msg =
          err instanceof ApiError
            ? err.message
            : err instanceof Error
            ? err.message
            : "failed to load agents";
        setBindState({ kind: "agents-error", message: msg });
      });
    return () => controller.abort();
  }, [step, shellNs]);

  // Auto-generate a binding name from agent + tool
  React.useEffect(() => {
    if (selectedName && tool.name) {
      const generated = `${selectedName}-${tool.name}`
        .toLowerCase()
        .replace(/[^a-z0-9-]/g, "-")
        .replace(/-+/g, "-")
        .slice(0, 63);
      setBindName(generated);
    }
  }, [selectedName, tool.name]);

  React.useEffect(() => {
    if (selectedNs) setBindNs(selectedNs);
  }, [selectedNs]);

  // When we reach the result step, create the binding and start polling
  React.useEffect(() => {
    if (step !== STEP_RESULT) return;
    if (!selectedNs || !selectedName) return;

    setBindState({ kind: "binding" });
    const controller = new AbortController();

    void (async () => {
      let detail: MCPToolBindingDetail;
      try {
        detail = await api.createMcpToolBinding(
          {
            name: bindName || undefined,
            namespace: bindNs || undefined,
            agentRef: { namespace: selectedNs, name: selectedName },
            toolName: tool.name,
          },
          controller.signal,
        );
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError) {
          if (err.isForbidden) {
            reprobe();
            setBindState({
              kind: "error",
              message: `Not allowed to bind tools: ${err.message}`,
              forbidden: true,
            });
            return;
          }
          setBindState({ kind: "error", message: err.message });
          return;
        }
        setBindState({
          kind: "error",
          message:
            err instanceof Error ? err.message : "binding creation failed",
        });
        return;
      }

      // Binding created — if already propagated, we're done immediately.
      if (detail.ready) {
        setBindState({ kind: "done", detail });
        toast({
          variant: "success",
          title: `Bound ${tool.name} to ${selectedName}`,
          description: "The tool is propagated and live on the agent.",
        });
        return;
      }

      // Poll for propagation status using the binding detail endpoint.
      setBindState({ kind: "polling" });
      const { namespace: bNs, name: bName } = detail;
      let attempts = 0;
      let lastDetail = detail;

      while (attempts < MAX_POLL_ATTEMPTS && !controller.signal.aborted) {
        await new Promise<void>((resolve) =>
          setTimeout(resolve, POLL_INTERVAL_MS),
        );
        if (controller.signal.aborted) break;
        try {
          lastDetail = await api.mcpToolBinding(bNs, bName, controller.signal);
        } catch {
          // Network hiccup during poll — don't error out, just keep polling.
          attempts++;
          continue;
        }
        if (lastDetail.ready) {
          // ONLY mark propagated when the controller says Ready=True.
          setBindState({ kind: "done", detail: lastDetail });
          toast({
            variant: "success",
            title: `Bound ${tool.name} to ${selectedName}`,
            description: "The tool is propagated and live on the agent.",
          });
          return;
        }
        attempts++;
      }

      // Exhausted polls — surface what the controller last reported, HONESTLY.
      setBindState({ kind: "done", detail: lastDetail });
    })();

    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step]);

  // ---- step content ----

  const canProceed =
    step === STEP_SELECT_AGENT
      ? !!selectedNs && !!selectedName
      : step === STEP_CONFIRM
      ? true
      : false; // STEP_RESULT has no "next"

  const busy =
    bindState.kind === "binding" || bindState.kind === "polling";

  const steps: WizardStep[] = [
    {
      id: "select-agent",
      title: "Select agent",
      description: "Choose the agent to bind the tool to",
      content: (
        <SelectAgentStep
          agents={agents}
          loadState={bindState}
          selectedNs={selectedNs}
          selectedName={selectedName}
          onSelect={(ns, name) => {
            setSelectedNs(ns);
            setSelectedName(name);
          }}
        />
      ),
    },
    {
      id: "confirm",
      title: "Confirm",
      description: "Review the binding before creating",
      content: (
        <ConfirmStep
          tool={tool}
          agentNs={selectedNs}
          agentName={selectedName}
          bindName={bindName}
          bindNs={bindNs}
          onBindNameChange={setBindName}
          onBindNsChange={setBindNs}
        />
      ),
    },
    {
      id: "result",
      title: "Propagation status",
      description: "Live status from the controller",
      review: true,
      content: (
        <ResultStep
          tool={tool}
          agentName={selectedName}
          bindState={bindState}
          canBind={can("mcptoolbindings", "create")}
        />
      ),
    },
  ];

  function handleStepChange(next: number) {
    // The Wizard's Next button calls onStepChange(current + 1); rail clicks
    // can go backwards. Both are valid — let the step advance freely (the
    // Wizard's canProceed already gates the Next button).
    setStep(next);
  }

  function handleFinish() {
    // Called ONLY when on the last step and the Finish button is clicked.
    onClose();
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      data-testid="bind-tool-wizard"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div className="relative w-full max-w-2xl rounded-xl border border-border bg-card shadow-overlay">
        <div className="p-6">
          <h3 className="font-serif text-xl font-medium">
            Bind <span className="font-mono text-lg">{tool.name}</span> to an agent
          </h3>
          <p className="mt-1 text-sm text-secondary-foreground">
            A new MCPToolBinding will be created and the controller will
            hot-update the agent's tool list live.
          </p>
        </div>
        <div className="px-6 pb-6">
          <Wizard
            steps={steps}
            current={step}
            onStepChange={handleStepChange}
            canProceed={canProceed}
            busy={busy}
            onFinish={handleFinish}
            finishLabel={step === STEP_RESULT ? "Done" : "Next"}
            nextLabel="Next"
            onCancel={onClose}
          />
        </div>
      </div>
    </div>
  );
}

// ---- SelectAgentStep --------------------------------------------------------

interface SelectAgentStepProps {
  agents: AgentSummary[];
  loadState: BindState;
  selectedNs: string;
  selectedName: string;
  onSelect: (ns: string, name: string) => void;
}

function SelectAgentStep({
  agents,
  loadState,
  selectedNs,
  selectedName,
  onSelect,
}: SelectAgentStepProps) {
  if (loadState.kind === "loading-agents") {
    return (
      <div className="flex items-center gap-2 py-4 text-sm text-faint">
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading agents…
      </div>
    );
  }

  if (loadState.kind === "agents-error") {
    return (
      <p
        className="rounded-md border border-destructive-surface bg-destructive-surface px-3 py-2 text-sm text-destructive"
        role="alert"
      >
        {loadState.message}
      </p>
    );
  }

  if (agents.length === 0) {
    return (
      <EmptyState
        title="No agents found"
        description="Create an agent first before binding a tool to it."
      />
    );
  }

  return (
    <div className="space-y-3">
      <p className="text-sm text-secondary-foreground">
        Select the agent you want to bind this tool to.
      </p>
      <div className="max-h-64 divide-y divide-border-soft overflow-y-auto rounded-lg border border-border">
        {agents.map((a) => {
          const selected = selectedNs === a.namespace && selectedName === a.name;
          return (
            <button
              key={`${a.namespace}/${a.name}`}
              type="button"
              // Selection is always pine-family, never a status hue (§2.3).
              className={`flex w-full items-center gap-3 px-4 py-3 text-left transition-colors ${
                selected ? "bg-accent" : "hover:bg-surface-2"
              }`}
              onClick={() => onSelect(a.namespace, a.name)}
              data-testid={`bind-agent-${a.namespace}-${a.name}`}
            >
              {selected ? (
                <CheckCircle className="h-4 w-4 shrink-0 text-primary" />
              ) : (
                <div className="h-4 w-4 shrink-0 rounded-full border border-border-strong" />
              )}
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="truncate font-mono text-sm font-medium" title={a.name}>
                    {a.name}
                  </span>
                  <Badge variant="muted">{a.namespace}</Badge>
                  {/* The kit owns the status vocabulary — a hand-rolled "not
                      ready" chip is how two surfaces start disagreeing. */}
                  {!a.ready && (
                    <StatusBadge ready={a.ready} phase={a.phase} reason={a.reason} />
                  )}
                </div>
                <p className="truncate font-mono text-xs text-faint" title={a.image}>
                  {a.image}
                </p>
              </div>
            </button>
          );
        })}
      </div>
    </div>
  );
}

// ---- ConfirmStep ------------------------------------------------------------

interface ConfirmStepProps {
  tool: CatalogTool;
  agentNs: string;
  agentName: string;
  bindName: string;
  bindNs: string;
  onBindNameChange: (v: string) => void;
  onBindNsChange: (v: string) => void;
}

function ConfirmStep({
  tool,
  agentNs,
  agentName,
  bindName,
  bindNs,
  onBindNameChange,
  onBindNsChange,
}: ConfirmStepProps) {
  return (
    <div className="space-y-4" data-testid="bind-tool-confirm">
      <div className="space-y-2 rounded-lg border border-border bg-surface-2 p-4">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-faint">Tool</span>
          <span className="min-w-0 truncate font-mono text-sm font-semibold" title={tool.name}>
            {tool.name}
          </span>
          {tool.registry && <Badge variant="muted">{tool.registry}</Badge>}
        </div>
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-faint">Agent</span>
          <span className="min-w-0 truncate font-mono text-sm font-semibold" title={agentName}>
            {agentName}
          </span>
          <Badge variant="muted">{agentNs}</Badge>
        </div>
      </div>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor="bind-ns" className="text-sm">
            Binding namespace{" "}
            <span className="text-faint">(defaults to agent namespace)</span>
          </Label>
          <Input
            id="bind-ns"
            value={bindNs}
            onChange={(e) => onBindNsChange(e.target.value)}
            placeholder={agentNs}
            data-testid="bind-ns-input"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="bind-name" className="text-sm">
            Binding name <span className="text-faint">(auto-generated)</span>
          </Label>
          <Input
            id="bind-name"
            value={bindName}
            onChange={(e) => onBindNameChange(e.target.value)}
            placeholder="agent-tool-binding"
            data-testid="bind-name-input"
          />
        </div>
      </div>
      <p className="text-xs text-faint">
        The controller will create an MCPToolBinding and hot-update the agent's
        tool list. You'll see the propagation status on the next step.
      </p>
    </div>
  );
}

// ---- ResultStep -------------------------------------------------------------
// Surfaces the propagation status HONESTLY from the controller's Ready condition.
// "propagated" is shown ONLY when the binding detail says ready=true (Ready=True).
// Any non-ready state shows the reason from the controller — never faked.
//
// The not-yet-propagated block reads `progressing`, not `warn`: the controller
// converging on its own is the machine doing its own work (§2.5), and amber is
// reserved for a bound near or crossed. The hue rides on a Tag, not on a filled
// panel — semantic hues are annotation, not alarm (§2.2).

interface ResultStepProps {
  tool: CatalogTool;
  agentName: string;
  bindState: BindState;
  canBind: boolean;
}

function ResultStep({ tool, agentName, bindState, canBind }: ResultStepProps) {
  if (bindState.kind === "binding") {
    return (
      <div
        className="flex items-center gap-2 py-4 text-sm text-faint"
        data-testid="binding-propagation-status"
        data-status="creating"
      >
        <Loader2 className="h-4 w-4 animate-spin" />
        Creating MCPToolBinding…
      </div>
    );
  }

  if (bindState.kind === "polling") {
    return (
      <div
        className="flex items-center gap-2 py-4 text-sm text-faint"
        data-testid="binding-propagation-status"
        data-status="propagating"
      >
        <Loader2 className="h-4 w-4 animate-spin" />
        Propagating… The controller is hot-updating the agent's tool list.
      </div>
    );
  }

  if (bindState.kind === "error") {
    return (
      <div
        className="space-y-1 rounded-md border border-destructive bg-destructive-surface px-4 py-3"
        data-testid="binding-propagation-status"
        data-status="error"
        role="alert"
      >
        <p className="text-sm font-medium text-destructive">Binding failed</p>
        <p className="text-sm text-destructive">{bindState.message}</p>
        {bindState.forbidden && !canBind && (
          <p className="text-xs text-secondary-foreground">
            Your account lacks permission to create tool bindings on this cluster.
          </p>
        )}
      </div>
    );
  }

  if (bindState.kind === "done") {
    const { detail } = bindState;
    // Honest propagation status: ONLY "propagated" when the controller says
    // ready=true. Otherwise show the controller's own reason — never upgrade.
    const isReady = detail.ready;
    const status = detail.propagationStatus;
    const isActuallyPropagated = isReady && status === "propagated";

    return (
      <div
        className="space-y-4"
        data-testid="binding-propagation-status"
        data-status={isActuallyPropagated ? "propagated" : "pending"}
      >
        <div className="space-y-2 rounded-lg border border-border bg-surface-2 p-4">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-faint">Binding</span>
            <span className="min-w-0 truncate font-mono text-sm" title={detail.name}>
              {detail.name}
            </span>
            <Badge variant="muted">{detail.namespace}</Badge>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-faint">Tool</span>
            <span className="min-w-0 truncate font-mono text-sm" title={tool.name}>
              {tool.name}
            </span>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-faint">Agent</span>
            <span className="min-w-0 truncate font-mono text-sm" title={agentName}>
              {agentName}
            </span>
          </div>
        </div>

        {isActuallyPropagated ? (
          <div className="flex items-start gap-3 rounded-md border border-border bg-surface-2 px-4 py-3">
            <CheckCircle className="mt-0.5 h-4 w-4 shrink-0 text-success" />
            <div>
              <p className="text-sm font-medium text-foreground">
                Propagated — hot-updated live
              </p>
              <p className="text-xs text-secondary-foreground">
                The tool is active in the agent's managed loop.
              </p>
            </div>
          </div>
        ) : (
          <div className="space-y-1 rounded-md border border-border bg-surface-2 px-4 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant="progressing">not yet propagated</Badge>
              <span className="font-mono text-xs text-faint">
                {status || "pending"}
              </span>
            </div>
            <p className="text-sm text-secondary-foreground">
              The binding was created and the controller has not reported it
              Ready yet. Check the agent's binding list for the latest status.
            </p>
          </div>
        )}
      </div>
    );
  }

  // idle / loading-agents — shouldn't reach here at STEP_RESULT, but guard.
  return null;
}
