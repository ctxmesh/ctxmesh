import * as React from "react";
import {
  BookOpen,
  CheckCircle,
  ChevronRight,
  Clock,
  Code2,
  Loader2,
  RefreshCw,
  Server,
  Shield,
  User,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  Wizard,
  useToast,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { groupToolsBySource } from "@/lib/tool-groups";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type AgentSummary,
  type CatalogTool,
  type MCPToolBindingDetail,
} from "@/lib/api";

// ToolCatalogPage — the merged tool catalog surface (m17.10). Lists ALL tools
// from GET /api/tools in three distinct states:
//   • curated   — from the built-in ToolRegistry (source unset or source=
//                 "curated"); always bindable
//   • user-added — submitted via the BYO-MCP flow + approved; bindable
//   • pending-approval — queued in the approval gate (m17.4); NOT bindable
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

function toolState(t: CatalogTool): ToolState {
  if (t.approvalStatus === "pending") return "pending-approval";
  // Heuristic: if a source is present and doesn't look like a registry name
  // (no "/" prefix), treat it as user-added. The BFF marks user-added tools
  // with approvalStatus="approved" + a source referencing the MCP server.
  // Curated tools either have no source or approvalStatus absent/approved with
  // source absent. We use the presence of a source as the user-added signal.
  if (t.source) return "user-added";
  return "curated";
}

type FilterState = "all" | ToolState;

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

  // Derived: filter the tool list by state + name query
  const displayedTools = React.useMemo<CatalogTool[]>(() => {
    if (page.kind !== "ready") return [];
    let tools = page.tools;
    if (filter !== "all") {
      tools = tools.filter((t) => toolState(t) === filter);
    }
    if (q.trim()) {
      const lower = q.trim().toLowerCase();
      tools = tools.filter(
        (t) =>
          t.name.toLowerCase().includes(lower) ||
          (t.description ?? "").toLowerCase().includes(lower),
      );
    }
    return tools;
  }, [page, filter, q]);

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="tool-catalog">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Tool catalog</h2>
          <p className="text-sm text-muted-foreground">
            Browse curated, user-added, and pending-approval tools. Bind an
            approved tool to an agent to enable it in that agent's managed loop.
          </p>
        </div>
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

      {page.kind === "loading" && <SkeletonTable rows={5} />}

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
          {/* Filters */}
          <div className="flex items-center gap-3">
            <div className="relative flex-1 max-w-xs">
              <Input
                placeholder="Filter tools…"
                value={q}
                onChange={(e) => setQ(e.target.value)}
                className="pl-3"
                data-testid="catalog-filter-input"
              />
            </div>
            <div className="flex items-center gap-2">
              <Button
                variant={filter === "all" ? "secondary" : "ghost"}
                size="sm"
                onClick={() => setFilter("all")}
                data-testid="catalog-filter-all"
              >
                All ({page.tools.length})
              </Button>
              <Button
                variant={filter === "curated" ? "secondary" : "ghost"}
                size="sm"
                onClick={() => setFilter("curated")}
                data-testid="catalog-filter-curated"
              >
                Curated
              </Button>
              <Button
                variant={filter === "user-added" ? "secondary" : "ghost"}
                size="sm"
                onClick={() => setFilter("user-added")}
                data-testid="catalog-filter-user-added"
              >
                User-added
              </Button>
              <Button
                variant={filter === "pending-approval" ? "secondary" : "ghost"}
                size="sm"
                onClick={() => setFilter("pending-approval")}
                data-testid="catalog-filter-pending"
              >
                Pending
              </Button>
            </div>
          </div>

          {displayedTools.length === 0 ? (
            <EmptyState
              title="No tools match"
              description="Try a different filter or clear the search."
              intent="filtered"
            />
          ) : (
            // Group the catalog by MCP server (m25 S11): each server's tools sit under
            // a header naming the server, so it's obvious which server a tool comes
            // from. Curated tools (no source) group last under "Curated tools".
            <div className="space-y-4" data-testid="catalog-tool-list">
              {groupToolsBySource(displayedTools).map(([source, tools]) => (
                <div
                  key={source}
                  className="overflow-hidden rounded-lg border bg-card shadow-card"
                  data-testid={`catalog-group-${source}`}
                >
                  <div className="flex items-center gap-2 border-b bg-muted/40 px-4 py-2">
                    <Server className="h-4 w-4 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 truncate text-sm font-medium">{source}</span>
                    <Badge variant="secondary">{tools.length}</Badge>
                  </div>
                  <div className="divide-y">
                    {tools.map((tool) => (
                      <ToolRow
                        key={`${source}/${tool.name}`}
                        tool={tool}
                        canBind={canBind}
                        onBind={() => setWizard({ kind: "open", tool })}
                      />
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}
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

// ---- ToolRow ----------------------------------------------------------------

interface ToolRowProps {
  tool: CatalogTool;
  canBind: boolean;
  onBind: () => void;
}

function ToolRow({ tool, canBind, onBind }: ToolRowProps) {
  const state = toolState(tool);
  const isPending = state === "pending-approval";

  return (
    <div
      className="flex items-start gap-4 px-4 py-3"
      data-testid={`catalog-tool-${tool.name}`}
    >
      <ToolStateIcon state={state} />
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-sm font-medium">{tool.name}</span>
          <ToolStateBadge state={state} toolName={tool.name} />
          {tool.inputSchema !== undefined && (
            <Badge variant="outline" className="text-[10px]">
              <Code2 className="mr-1 h-2.5 w-2.5" />
              schema
            </Badge>
          )}
        </div>
        {tool.description && (
          <p className="text-sm text-muted-foreground">{tool.description}</p>
        )}
        {tool.source && (
          <p className="font-mono text-xs text-muted-foreground">
            source: {tool.source}
          </p>
        )}
        {isPending && (
          <p className="text-xs text-warning-foreground">
            Awaiting operator approval — cannot bind until approved.
          </p>
        )}
      </div>
      <div className="shrink-0">
        <Button
          size="sm"
          variant="outline"
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
    </div>
  );
}

// ---- state icon + badge helpers ---------------------------------------------

function ToolStateIcon({ state }: { state: ToolState }) {
  if (state === "curated") {
    return <Shield className="mt-0.5 h-4 w-4 shrink-0 text-success" />;
  }
  if (state === "user-added") {
    return <User className="mt-0.5 h-4 w-4 shrink-0 text-accent-foreground" />;
  }
  return <Clock className="mt-0.5 h-4 w-4 shrink-0 text-warning" />;
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
      <Badge
        variant="secondary"
        className="text-[10px]"
        data-testid={`catalog-tool-state-${toolName}`}
      >
        curated
      </Badge>
    );
  }
  if (state === "user-added") {
    return (
      <Badge
        variant="outline"
        className="text-[10px]"
        data-testid={`catalog-tool-state-${toolName}`}
      >
        user-added
      </Badge>
    );
  }
  return (
    <Badge
      variant="warning"
      className="text-[10px]"
      data-testid={`catalog-tool-state-${toolName}`}
    >
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
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      data-testid="bind-tool-wizard"
    >
      <div className="w-full max-w-2xl rounded-xl bg-background shadow-xl">
        <div className="p-6">
          <h3 className="text-lg font-semibold">
            Bind <span className="font-mono">{tool.name}</span> to an agent
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
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
      <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading agents…
      </div>
    );
  }

  if (loadState.kind === "agents-error") {
    return (
      <p
        className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
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
      <p className="text-sm text-muted-foreground">
        Select the agent you want to bind this tool to.
      </p>
      <div className="max-h-64 overflow-y-auto rounded-lg border divide-y">
        {agents.map((a) => {
          const selected = selectedNs === a.namespace && selectedName === a.name;
          return (
            <button
              key={`${a.namespace}/${a.name}`}
              type="button"
              className={`flex w-full items-center gap-3 px-4 py-3 text-left transition-colors hover:bg-accent/50 ${
                selected ? "bg-accent/30" : ""
              }`}
              onClick={() => onSelect(a.namespace, a.name)}
              data-testid={`bind-agent-${a.namespace}-${a.name}`}
            >
              {selected ? (
                <CheckCircle className="h-4 w-4 shrink-0 text-success" />
              ) : (
                <div className="h-4 w-4 shrink-0 rounded-full border border-border" />
              )}
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="font-mono text-sm font-medium">{a.name}</span>
                  <Badge variant="secondary" className="text-[10px]">
                    {a.namespace}
                  </Badge>
                  {!a.ready && (
                    <Badge variant="warning" className="text-[10px]">
                      not ready
                    </Badge>
                  )}
                </div>
                <p className="text-xs text-muted-foreground">{a.image}</p>
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
      <div className="rounded-lg border bg-card/50 p-4 space-y-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium text-muted-foreground">Tool</span>
          <span className="font-mono text-sm font-semibold">{tool.name}</span>
          {tool.source && (
            <Badge variant="outline" className="text-[10px]">
              {tool.source}
            </Badge>
          )}
        </div>
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium text-muted-foreground">Agent</span>
          <span className="font-mono text-sm font-semibold">{agentName}</span>
          <Badge variant="secondary" className="text-[10px]">
            {agentNs}
          </Badge>
        </div>
      </div>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor="bind-ns" className="text-sm">
            Binding namespace{" "}
            <span className="text-muted-foreground">(defaults to agent namespace)</span>
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
            Binding name{" "}
            <span className="text-muted-foreground">(auto-generated)</span>
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
      <p className="text-xs text-muted-foreground">
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
        className="flex items-center gap-2 py-4 text-sm text-muted-foreground"
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
        className="flex items-center gap-2 py-4 text-sm text-muted-foreground"
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
        className="rounded-md border border-destructive/40 bg-destructive/5 px-4 py-3 space-y-1"
        data-testid="binding-propagation-status"
        data-status="error"
        role="alert"
      >
        <p className="text-sm font-medium text-destructive">Binding failed</p>
        <p className="text-sm text-destructive/90">{bindState.message}</p>
        {bindState.forbidden && !canBind && (
          <p className="text-xs text-muted-foreground">
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
      <div className="space-y-4" data-testid="binding-propagation-status" data-status={isActuallyPropagated ? "propagated" : "pending"}>
        <div className="rounded-lg border bg-card/50 p-4 space-y-2">
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium text-muted-foreground">Binding</span>
            <span className="font-mono text-sm">{detail.name}</span>
            <Badge variant="secondary" className="text-[10px]">{detail.namespace}</Badge>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium text-muted-foreground">Tool</span>
            <span className="font-mono text-sm">{tool.name}</span>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium text-muted-foreground">Agent</span>
            <span className="font-mono text-sm">{agentName}</span>
          </div>
        </div>

        {isActuallyPropagated ? (
          <div className="flex items-center gap-2 rounded-md border border-success/30 bg-success/5 px-4 py-3">
            <CheckCircle className="h-4 w-4 text-success" />
            <div>
              <p className="text-sm font-medium text-success-foreground">
                Propagated — hot-updated live
              </p>
              <p className="text-xs text-muted-foreground">
                The tool is active in the agent's managed loop.
              </p>
            </div>
          </div>
        ) : (
          <div className="rounded-md border border-warning/30 bg-warning/5 px-4 py-3 space-y-1">
            <div className="flex items-center gap-2">
              <Clock className="h-4 w-4 text-warning" />
              <p className="text-sm font-medium text-warning-foreground">
                Not yet propagated
              </p>
            </div>
            <p className="text-sm text-muted-foreground">
              Controller status: <span className="font-mono text-xs">{status || "pending"}</span>
            </p>
            <p className="text-xs text-muted-foreground">
              The binding was created. The controller will reconcile it — check the
              agent's binding list for the latest status.
            </p>
          </div>
        )}
      </div>
    );
  }

  // idle / loading-agents — shouldn't reach here at STEP_RESULT, but guard.
  return null;
}
