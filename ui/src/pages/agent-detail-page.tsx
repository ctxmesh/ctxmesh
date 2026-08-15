import * as React from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Boxes,
  ChevronRight,
  ExternalLink,
  GitFork,
  Pencil,
  Play,
  Plus,
  RotateCcw,
  Server,
  Share2,
  SlidersHorizontal,
  Terminal,
  Trash2,
  Wrench,
  X,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  ConfirmDialog,
  DataTable,
  type Column,
  DetailDrawer,
  EmptyState,
  ForbiddenInline,
  Wizard,
  type WizardStep,
  useFocusTrap,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { ChatPanel } from "@/components/agent-chat";
import { RunInspector } from "@/components/dashboard/run-inspector";
import { UseAgentPanel } from "@/components/dashboard/use-agent-panel";
import {
  api,
  ApiError,
  openLogStream,
  type AgentBinding,
  type AgentCondition,
  type AgentDetailResponse,
  type AgentReference,
  type AgentRuntimeDetail,
  type AgentRunSummary,
  type AgentMemoryEntry,
  type LongTermMemoryConfig,
  type AgentScalingPolicySummary,
  type AgentSimplifiedSpec,
  type LogEventType,
  type MemoryBindingSummary,
  type OnlineScoreResponse,
  type OnlineScoreWindow,
  type PublishTemplateResponse,
  type RunSummary,
} from "@/lib/api";

// FORK_NEEDS_REBINDING_LABEL is the Kubernetes label the operator sets on a
// forked agent that has unresolved references (model route, tools). Its presence
// in the agent's labels drives the "Needs attention" banner (m74.6).
const FORK_NEEDS_REBINDING_LABEL = "agents.ctxmesh.ai/fork-needs-rebinding";
// Fork provenance labels (ADR 0068 §6) — forwarded from the AgentDeployment CR.
// Used for lineage display (U12, m76.3) and banner repair links (U5).
const LABEL_FORK_ORIGIN_NS = "agents.ctxmesh.ai/fork-origin-namespace";
const LABEL_FORK_ORIGIN_NAME = "agents.ctxmesh.ai/fork-origin-name";
const LABEL_FORK_ORIGIN_VERSION = "agents.ctxmesh.ai/fork-origin-version";
import { useCapabilities } from "@/lib/capabilities";
import { navRoute, RES_AGENTS, RES_MEMORY, RES_SCALING } from "@/lib/nav";

// AgentDetailPage — the agent LANDING page (first-agent-flow.md §5, m14.11,
// extended m15.11). It closes the aha loop: watch the agent come alive (status
// timeline + live log tail) → run it (the Run panel) → see the trace with the
// tool span (the native run inspector).
//
// m15.11 additions:
//   • drift / managedOutsideUI badges in the header.
//   • Edit Wizard — full round-trip for console-managed; safe-field patch for
//     managedOutsideUI. On drift, warns before submit (overwrite).
//   • Typed-name Delete via ConfirmDialog: loads + shows agentReferences
//     (GC vs orphan impact), then calls deleteAgent on confirm.
//   • Per-agent Runs list (agentRuns): bounded, 501 → calm empty state.
//   • All write affordances RBAC-aware (display-only, ADR 0011).

const TABS = ["Overview", "Logs", "Runs", "Bindings", "Memory", "Scaling", "Redaction"] as const;
type Tab = (typeof TABS)[number];

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: AgentDetailResponse }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function AgentDetailPage() {
  const { ns = "", name = "" } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [state, setState] = React.useState<Load>({ kind: "loading" });
  // The initial tab is deep-linkable via ?tab=<Name> (m49.3) — a trace→memory
  // back-link lands directly on the Memory tab. Unknown/absent ⇒ Overview.
  const initialTab = ((): Tab => {
    const t = searchParams.get("tab");
    return (TABS as readonly string[]).includes(t ?? "") ? (t as Tab) : "Overview";
  })();
  const [tab, setTab] = React.useState<Tab>(initialTab);
  // The trace to inspect — set when a run returns a traceId; opens the inspector
  // drawer over the page (list context preserved).
  const [inspectTrace, setInspectTrace] = React.useState<string | null>(null);

  // Edit + delete dialogs are opened by ?edit=1 / ?delete=1 search params so
  // they survive a hard reload and can be triggered from the list's row actions.
  const editOpen = searchParams.get("edit") === "1";
  const deleteOpen = searchParams.get("delete") === "1";
  // Publish-as-template dialog — local state (not deep-linked, no reload needed).
  const [publishOpen, setPublishOpen] = React.useState(false);
  // U7: track in-session published state. When the user publishes, we store the
  // response (version + visibility) so the header badge can show it without a
  // reload. No persistent read — the agent DTO doesn't yet carry published state.
  const [publishedState, setPublishedState] = React.useState<{
    version: string;
    visibility: string;
  } | null>(null);

  function openEdit() {
    setSearchParams((p) => { p.set("edit", "1"); return p; });
  }
  function closeEdit() {
    setSearchParams((p) => { p.delete("edit"); return p; });
  }
  function openDelete() {
    setSearchParams((p) => { p.set("delete", "1"); return p; });
  }
  function closeDelete() {
    setSearchParams((p) => { p.delete("delete"); return p; });
  }

  const load = React.useCallback(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .agentDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the agent",
          status: apiErr?.status,
          forbidden: apiErr?.isForbidden ?? false,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  React.useEffect(() => load(), [load]);

  if (state.kind === "loading") {
    return (
      <div className="mx-auto max-w-5xl">
        <p className="text-sm text-muted-foreground" data-testid="agent-detail-loading">
          Loading {name}…
        </p>
      </div>
    );
  }

  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="mx-auto max-w-5xl">
          <ForbiddenInline
            title={`Not allowed to view ${name}`}
            description="Your account can't read this agent in this namespace."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="agent-not-found">
          <EmptyState
            icon={Boxes}
            title="Agent not found"
            description={`No agent "${name}" in ${ns || "this namespace"}. It may have been deleted, or the name is wrong.`}
            action={{ label: "Back to agents", onClick: () => history.back() }}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="agent-detail-error"
        >
          Couldn't load the agent: {state.message}
        </div>
      </div>
    );
  }

  const detail = state.detail;

  // m74.6: fork-needs-rebinding banner — visible when the label is set on the
  // AgentDeployment CR (the operator stamps it at fork time when refs are dangling).
  const needsRebinding =
    detail.labels?.[FORK_NEEDS_REBINDING_LABEL] === "true";

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="agent-detail-page">
      <AgentHeader
        detail={detail}
        onEdit={openEdit}
        onDelete={openDelete}
        onPublish={() => setPublishOpen(true)}
        publishedState={publishedState}
        onUnpublish={() => {
          void (async () => {
            try {
              await api.unpublishTemplate("agent", detail.namespace, detail.name);
              setPublishedState(null);
            } catch {
              // If unpublish fails, the badge stays — the user can try again.
            }
          })();
        }}
      />

      {/* m74.6 / U5: needs-rebinding banner — shown when the agent was forked and has
          dangling references. Renders actionable repair line-items with links. The label
          "true" means at least one ref is unresolvable; we use the agent's own data
          (modelRoute absence, bindings list) to surface specific CTAs. The banner clears
          when the operator removes the label (i.e. the user has connected all resources
          and the operator re-evaluates — or they edit the agent). */}
      {needsRebinding && (
        <div
          className="rounded-lg border border-amber-300 bg-amber-50 p-4 dark:border-amber-700 dark:bg-amber-950/30"
          role="alert"
          data-testid="needs-rebinding-banner"
        >
          <div className="flex items-start gap-3">
            <Wrench className="mt-0.5 h-5 w-5 shrink-0 text-amber-600 dark:text-amber-400" />
            <div className="space-y-2 flex-1">
              <p className="font-medium text-amber-900 dark:text-amber-200">
                Needs attention — connect resources before running
              </p>
              <p className="text-sm text-amber-700 dark:text-amber-300">
                This agent was forked from a template but has unresolved references.
                Complete the steps below so it can run successfully.
              </p>
              {/* U5: actionable line items with repair links */}
              <ul className="space-y-1 text-sm">
                {!detail.modelRoute && (
                  <li className="flex items-center gap-2 text-amber-800 dark:text-amber-300">
                    <ChevronRight className="h-3.5 w-3.5 shrink-0" />
                    <Link
                      to="/routes"
                      className="underline hover:text-amber-600 dark:hover:text-amber-200"
                      data-testid="rebind-model-route-link"
                    >
                      Connect a model route
                    </Link>
                    <span className="text-amber-600 dark:text-amber-400 text-xs">— required to run</span>
                  </li>
                )}
                <li className="flex items-center gap-2 text-amber-800 dark:text-amber-300">
                  <ChevronRight className="h-3.5 w-3.5 shrink-0" />
                  <button
                    onClick={() => setTab("Bindings")}
                    className="underline hover:text-amber-600 dark:hover:text-amber-200"
                    data-testid="rebind-bindings-tab-link"
                  >
                    Review and bind tools in the Bindings tab
                  </button>
                </li>
              </ul>
              <p className="text-xs text-amber-600 dark:text-amber-400">
                This banner clears once the operator confirms all references are resolved.
              </p>
            </div>
          </div>
        </div>
      )}

      <div className="flex flex-wrap gap-1 border-b" role="tablist" aria-label="Agent detail">
        {TABS.map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            data-testid={`tab-${t.toLowerCase()}`}
            className={`-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
              tab === t
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground"
            }`}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === "Overview" && (
        <OverviewTab detail={detail} onTraced={(id) => setInspectTrace(id)} />
      )}
      {tab === "Logs" && <LogsTab ns={detail.namespace} name={detail.name} ready={detail.ready} />}
      {tab === "Runs" && (
        <AgentRunsTab ns={detail.namespace} name={detail.name} onInspect={(id) => setInspectTrace(id)} />
      )}
      {tab === "Bindings" && <BindingsTab bindings={detail.bindings} />}
      {tab === "Memory" && (
        <MemoryPanel ns={detail.namespace} agentName={detail.name} />
      )}
      {tab === "Scaling" && (
        <ScalingPanel ns={detail.namespace} agentName={detail.name} />
      )}
      {tab === "Redaction" && (
        <RedactionPanel ns={detail.namespace} agentName={detail.name} />
      )}

      {/* The run inspector opens over the page (drawer) so list/tab context is
          kept. It closes back to exactly where you were. */}
      <DetailDrawer
        open={inspectTrace !== null}
        onClose={() => setInspectTrace(null)}
        title="Run inspector"
        subtitle={inspectTrace ?? undefined}
        size="lg"
      >
        {inspectTrace && <RunInspector traceId={inspectTrace} />}
      </DetailDrawer>

      {/* Edit Wizard — opened by ?edit=1 search param. */}
      <DetailDrawer
        open={editOpen}
        onClose={closeEdit}
        title={`Edit ${detail.name}`}
        subtitle={detail.managedOutsideUI ? "Managed outside the UI — safe fields only" : undefined}
        size="lg"
      >
        {editOpen && (
          <EditWizard
            detail={detail}
            onClose={closeEdit}
            onSaved={() => {
              closeEdit();
              load();
            }}
          />
        )}
      </DetailDrawer>

      {/* Delete dialog — opened by ?delete=1 search param. */}
      {deleteOpen && (
        <DeleteDialog
          detail={detail}
          onClose={closeDelete}
          onDeleted={() => navigate("/agents")}
        />
      )}

      {/* Publish-as-template dialog (m74.6) — opened by the Publish button in
          the header (RBAC-gated: update agentdeployments). */}
      {publishOpen && (
        <PublishTemplateDialog
          agentNamespace={detail.namespace}
          agentName={detail.name}
          alreadyPublished={publishedState !== null}
          onClose={() => setPublishOpen(false)}
          onDone={(res, visibility) => {
            setPublishOpen(false);
            // U7: store session-level published state so header shows the badge.
            setPublishedState({ version: res.version ?? "1", visibility });
          }}
        />
      )}
    </div>
  );
}

// ── Header ──────────────────────────────────────────────────────────────────
function AgentHeader({
  detail,
  onEdit,
  onDelete,
  onPublish,
  publishedState,
  onUnpublish,
}: {
  detail: AgentDetailResponse;
  onEdit: () => void;
  onDelete: () => void;
  onPublish: () => void;
  // U7: in-session published state — shown as a badge; if null, the agent is not (yet) published.
  publishedState: { version: string; visibility: string } | null;
  onUnpublish: () => void;
}) {
  const { can } = useCapabilities();
  const canEdit = can(RES_AGENTS, "update");
  const canDelete = can(RES_AGENTS, "delete");
  // Publish-as-template is gated on agent update rights (the publisher must own
  // the agent). Display-only — the API is the real RBAC gate (ADR 0011).
  const canPublish = can(RES_AGENTS, "update");

  // U12: fork lineage — show "forked from ns/name @ version" when the provenance labels are set.
  const forkOriginNs = detail.labels?.[LABEL_FORK_ORIGIN_NS];
  const forkOriginName = detail.labels?.[LABEL_FORK_ORIGIN_NAME];
  const forkOriginVersion = detail.labels?.[LABEL_FORK_ORIGIN_VERSION];
  const hasForkLineage = !!(forkOriginNs && forkOriginName);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-2xl font-semibold tracking-tight">{detail.name}</h2>
        <Badge variant={detail.ready ? "success" : "warning"}>
          {detail.phase || (detail.ready ? "Ready" : "Pending")}
        </Badge>
        {/* m15.11: drift + managedOutsideUI badges */}
        {detail.managedOutsideUI && (
          <Badge variant="outline" data-testid="managed-outside-badge">
            managed outside UI
          </Badge>
        )}
        {detail.drift && (
          <Badge variant="warning" data-testid="drift-badge">
            <AlertTriangle className="mr-1 h-3 w-3" />
            drift
          </Badge>
        )}
        {/* U7: published badge — shown when this agent has been published as a template
            this session. Includes an Unpublish action. */}
        {publishedState && (
          <div className="flex items-center gap-1" data-testid="published-badge-group">
            <Badge variant="secondary" data-testid="published-badge">
              <Share2 className="mr-1 h-3 w-3" />
              Published · {publishedState.visibility} · v{publishedState.version}
            </Badge>
            {canPublish && (
              <Button
                variant="ghost"
                size="sm"
                className="h-6 px-1.5 text-xs text-muted-foreground hover:text-destructive"
                onClick={onUnpublish}
                data-testid="unpublish-agent-button"
                title="Remove this template from the gallery"
              >
                <X className="h-3 w-3" />
                Unpublish
              </Button>
            )}
          </div>
        )}
        {/* The namespace links to the tenant that governs it (m49.4 UX-review P1 —
            closes the observe→agent→…→tenant loop). The Tenants filter matches on
            namespace; an unowned namespace lands on an honest empty list. */}
        <Link
          to={`/tenants?q=${encodeURIComponent(detail.namespace)}`}
          data-testid="agent-namespace-link"
          title={`View the tenant governing namespace "${detail.namespace}"`}
          className="text-sm text-muted-foreground hover:text-foreground hover:underline"
        >
          {detail.namespace}
        </Link>
        {/* RBAC-aware write affordances — hidden for viewers */}
        <div className="ml-auto flex items-center gap-2">
          {canPublish && (
            <Button
              variant="outline"
              size="sm"
              onClick={onPublish}
              data-testid="publish-agent-button"
            >
              <Share2 className="h-4 w-4" />
              {publishedState ? "Publish new version" : "Publish"}
            </Button>
          )}
          {canEdit && (
            <Button
              variant="outline"
              size="sm"
              onClick={onEdit}
              data-testid="edit-agent-button"
            >
              <Pencil className="h-4 w-4" />
              Edit
            </Button>
          )}
          {canDelete && (
            <Button
              variant="outline"
              size="sm"
              onClick={onDelete}
              className="text-destructive hover:text-destructive"
              data-testid="delete-agent-button"
            >
              <Trash2 className="h-4 w-4" />
              Delete
            </Button>
          )}
        </div>
      </div>
      {/* U12: fork lineage — "forked from ns/name @ version" */}
      {hasForkLineage && (
        <p
          className="flex items-center gap-1.5 text-xs text-muted-foreground"
          data-testid="fork-lineage"
        >
          <GitFork className="h-3.5 w-3.5" />
          Forked from{" "}
          <span className="font-mono">
            {forkOriginNs}/{forkOriginName}
          </span>
          {forkOriginVersion && (
            <span>@ {forkOriginVersion}</span>
          )}
        </p>
      )}
      <dl className="grid grid-cols-1 gap-x-8 gap-y-1.5 text-sm sm:grid-cols-2 lg:grid-cols-3">
        {detail.url && (
          <HeaderKV
            k="Route"
            v={
              <a
                href={detail.url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 truncate font-mono text-xs text-primary hover:underline"
                data-testid="agent-url"
              >
                {detail.url}
                <ExternalLink className="h-3 w-3 shrink-0" />
              </a>
            }
          />
        )}
        {detail.image && (
          <HeaderKV k="Image" v={<span className="truncate font-mono text-xs">{detail.image}</span>} />
        )}
        {detail.executionModel && <HeaderKV k="Execution" v={detail.executionModel} />}
        {detail.role && <HeaderKV k="Role" v={detail.role} />}
        {detail.modelRoute && (
          <HeaderKV
            k="Model route"
            v={
              <Link
                to={`/routes/${encodeURIComponent(detail.namespace)}/${encodeURIComponent(detail.modelRoute)}`}
                className="truncate text-primary hover:underline"
                data-testid="agent-modelroute-link"
              >
                {detail.modelRoute}
              </Link>
            }
          />
        )}
        {detail.promptRef && (
          <HeaderKV
            k="Prompt"
            v={
              <Link
                to={navRoute("prompts")}
                className="truncate text-primary hover:underline"
                data-testid="agent-promptref-link"
              >
                {detail.promptRef}
              </Link>
            }
          />
        )}
        {detail.guardrailPolicyRef && (
          <HeaderKV
            k="Guardrail policy"
            v={
              <>
                <Link
                  to={navRoute("guardrails")}
                  className="truncate text-primary hover:underline"
                  data-testid="agent-guardrail-policy-link"
                >
                  {detail.guardrailPolicyRef}
                </Link>
                {/* When the agent is not ready due to a guardrail policy problem, surface the reason inline
                    so the operator sees it next to the ref rather than having to scroll to the status timeline. */}
                {(() => {
                  const guardrailReasons = ["GuardrailPolicyNotFound", "GuardrailPolicyInvalid"];
                  const reason = detail.conditions.find(
                    (c) => c.type === "Ready" && c.status !== "True" && guardrailReasons.includes(c.reason),
                  )?.reason;
                  return reason ? (
                    <Badge
                      variant="destructive"
                      className="ml-2 text-[10px]"
                      data-testid="agent-guardrail-notready-reason"
                    >
                      {reason}
                    </Badge>
                  ) : null;
                })()}
              </>
            }
          />
        )}
        <HeaderKV k="Scaling" v={`${detail.scaling.min} – ${detail.scaling.max}`} />
        {detail.latestVersion && (
          <HeaderKV k="Latest version" v={<span className="font-mono text-xs">{detail.latestVersion}</span>} />
        )}
      </dl>
    </div>
  );
}

function HeaderKV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <dt className="shrink-0 text-muted-foreground">{k}</dt>
      <dd className="min-w-0 truncate">{v}</dd>
    </div>
  );
}

// ── Edit Wizard (m15.11) ──────────────────────────────────────────────────────
// Two modes:
//   • Console-managed (managedOutsideUI=false/absent): full round-trip — all
//     simplified spec fields are editable.
//   • Managed outside UI (managedOutsideUI=true): safe fields only (image,
//     scaling, modelRoute, systemPrompt) — the rest are read-only with a note.
//     The BFF applies a degraded safe-field patch (ADR 0017).
// On drift (detail.drift=true): warn the user before submit that the edit will
// overwrite any drift (i.e. the live CRD diverges from the last console spec).

type EditForm = {
  image: string;
  modelRoute: string;
  systemPrompt: string;
  scalingMin: string;
  scalingMax: string;
  executionModel: string;
  role: string;
};

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

function EditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: AgentDetailResponse;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const isManaged = !detail.managedOutsideUI;
  const hasDrift = detail.drift ?? false;

  const [form, setForm] = React.useState<EditForm>({
    image: detail.image ?? "",
    modelRoute: "",
    systemPrompt: "",
    scalingMin: String(detail.scaling.min),
    scalingMax: String(detail.scaling.max),
    executionModel: detail.executionModel ?? "",
    role: detail.role ?? "",
  });
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  // Drift-overwrite confirmation: set when the user tries to submit with drift.
  const [confirmDriftOverwrite, setConfirmDriftOverwrite] = React.useState(false);

  function set<K extends keyof EditForm>(k: K, v: EditForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      const spec: AgentSimplifiedSpec = {
        image: form.image.trim() || undefined,
        modelRoute: form.modelRoute.trim() || undefined,
        systemPrompt: form.systemPrompt.trim() || undefined,
        scaling: {
          min: parseInt(form.scalingMin, 10) || 0,
          max: parseInt(form.scalingMax, 10) || 1,
        },
        // Full round-trip fields only for console-managed agents.
        ...(!detail.managedOutsideUI
          ? {
              executionModel: form.executionModel.trim() || undefined,
              role: form.role.trim() || undefined,
            }
          : {}),
      };
      await api.updateAgent(detail.namespace, detail.name, spec);
      toast({
        variant: "success",
        title: "Agent updated",
        description: `${detail.name} saved successfully.`,
      });
      onSaved();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSaveState({
        kind: "error",
        message: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  function onFinish() {
    // If there's drift, confirm before overwriting.
    if (hasDrift && !confirmDriftOverwrite) {
      setConfirmDriftOverwrite(true);
      return;
    }
    void doSave();
  }

  // Step 1: Safe fields (image, scaling, modelRoute, systemPrompt) — always
  // shown regardless of managedOutsideUI.
  const safeFieldsStep: WizardStep = {
    id: "safe-fields",
    title: "Image & scaling",
    description: "Editable on all agents",
    content: (
      <div className="space-y-4">
        {detail.managedOutsideUI && (
          <div
            className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
            data-testid="managed-outside-note"
          >
            <p className="font-medium">Managed outside the UI</p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              This agent was created or last modified outside the console. Only safe
              fields (image, scaling, model route, system prompt) are editable here.
              Other fields are read-only to avoid overwriting your configuration.
            </p>
          </div>
        )}
        <FormField id="edit-image" label="Image">
          <Input
            id="edit-image"
            value={form.image}
            onChange={(e) => set("image", e.target.value)}
            placeholder="ghcr.io/acme/agent:v1"
            data-testid="edit-image"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="edit-min" label="Min replicas">
            <Input
              id="edit-min"
              inputMode="numeric"
              value={form.scalingMin}
              onChange={(e) => set("scalingMin", e.target.value)}
              data-testid="edit-scaling-min"
            />
          </FormField>
          <FormField id="edit-max" label="Max replicas">
            <Input
              id="edit-max"
              inputMode="numeric"
              value={form.scalingMax}
              onChange={(e) => set("scalingMax", e.target.value)}
              data-testid="edit-scaling-max"
            />
          </FormField>
        </div>
        <FormField id="edit-model-route" label="Model route">
          <Input
            id="edit-model-route"
            value={form.modelRoute}
            onChange={(e) => set("modelRoute", e.target.value)}
            placeholder="default-model"
            data-testid="edit-model-route"
          />
        </FormField>
        <FormField id="edit-system-prompt" label="System prompt">
          <Textarea
            id="edit-system-prompt"
            rows={4}
            value={form.systemPrompt}
            onChange={(e) => set("systemPrompt", e.target.value)}
            placeholder="You are a support agent…"
            data-testid="edit-system-prompt"
          />
        </FormField>
      </div>
    ),
  };

  // Step 2: Full round-trip fields — only for console-managed agents. For
  // managedOutsideUI agents, shown as read-only with a note.
  const fullFieldsStep: WizardStep = {
    id: "full-fields",
    title: "Execution & role",
    description: isManaged ? "Full round-trip" : "Read-only (managed outside UI)",
    content: (
      <div className="space-y-4">
        {!isManaged && (
          <div
            className="rounded-md border border-border bg-surface-2/40 px-3 py-2 text-xs text-muted-foreground"
            data-testid="readonly-fields-note"
          >
            These fields are read-only because this agent is managed outside the
            UI. Edit them via the tool that manages this agent.
          </div>
        )}
        <FormField id="edit-execution-model" label="Execution model">
          <Select
            id="edit-execution-model"
            value={form.executionModel}
            onChange={(e) => set("executionModel", e.target.value)}
            disabled={!isManaged}
            data-testid="edit-execution-model"
          >
            <option value="">— unchanged —</option>
            <option value="serving">serving (request-driven)</option>
            <option value="eventing">eventing (broker-triggered)</option>
            <option value="job">job (one-shot)</option>
          </Select>
        </FormField>
        <FormField id="edit-role" label="Role">
          <Input
            id="edit-role"
            value={form.role}
            onChange={(e) => set("role", e.target.value)}
            disabled={!isManaged}
            data-testid="edit-role"
          />
        </FormField>
      </div>
    ),
  };

  // Review step.
  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {hasDrift && (
          <div
            className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
            data-testid="drift-overwrite-warning"
          >
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
            <p>
              This agent has <strong>drift</strong> — the live CRD diverges from the
              last console-applied spec. Saving here will overwrite that drift with the
              values you've entered.
            </p>
          </div>
        )}
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this agent"
            description="Your account can't update agents in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="edit-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <ReviewRow k="Image" v={form.image || "—"} />
          <ReviewRow k="Scaling" v={`${form.scalingMin} – ${form.scalingMax}`} />
          {form.modelRoute && <ReviewRow k="Model route" v={form.modelRoute} />}
          {form.systemPrompt && (
            <ReviewRow k="System prompt" v={truncate(form.systemPrompt, 80)} />
          )}
          {isManaged && form.executionModel && (
            <ReviewRow k="Execution" v={form.executionModel} />
          )}
          {isManaged && form.role && <ReviewRow k="Role" v={form.role} />}
        </dl>
      </div>
    ),
  };

  const steps = isManaged
    ? [safeFieldsStep, fullFieldsStep, reviewStep]
    : [safeFieldsStep, reviewStep];

  return (
    <>
      <Wizard
        steps={steps}
        current={current}
        onStepChange={setCurrent}
        busy={saveState.kind === "saving"}
        onFinish={onFinish}
        finishLabel="Save changes"
        onCancel={onClose}
        dirty={form.image !== (detail.image ?? "") ||
          form.scalingMin !== String(detail.scaling.min) ||
          form.scalingMax !== String(detail.scaling.max)}
      />
      {/* Drift-overwrite confirmation dialog */}
      <ConfirmDialog
        open={confirmDriftOverwrite}
        onCancel={() => setConfirmDriftOverwrite(false)}
        onConfirm={() => {
          setConfirmDriftOverwrite(false);
          void doSave();
        }}
        title="Overwrite drift?"
        description="The live CRD has drifted from the last console-applied spec. Saving will overwrite it with your edits. Changes made outside the console will be lost."
        confirmLabel="Save and overwrite drift"
        destructive={false}
      />
    </>
  );
}

function ReviewRow({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex items-start justify-between gap-4 px-3 py-2">
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="text-right">{v}</dd>
    </div>
  );
}

// ── Delete Dialog (m15.11) ───────────────────────────────────────────────────
// Loads agentReferences (delete-impact preview) before showing the typed-name
// ConfirmDialog. The impact section shows what GC'd vs what's orphaned. On
// confirm → deleteAgent → navigate back to the agents list.

type RefsLoad =
  | { kind: "loading" }
  | { kind: "ready"; references: AgentReference[] }
  | { kind: "error"; message: string };

function DeleteDialog({
  detail,
  onClose,
  onDeleted,
}: {
  detail: AgentDetailResponse;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const [refs, setRefs] = React.useState<RefsLoad>({ kind: "loading" });
  const [deleting, setDeleting] = React.useState(false);

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .agentReferences(detail.namespace, detail.name, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRefs({ kind: "ready", references: res.references });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setRefs({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load delete impact",
        });
      });
    return () => controller.abort();
  }, [detail.namespace, detail.name]);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.deleteAgent(detail.namespace, detail.name);
      toast({
        variant: "success",
        title: "Agent deleted",
        description: `${detail.name} has been removed.`,
      });
      onDeleted();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      toast({
        variant: "error",
        title: "Delete failed",
        description: err instanceof Error ? err.message : "delete failed",
      });
      setDeleting(false);
      onClose();
    }
  }

  // Delete-impact preview rendered inside the ConfirmDialog's `impact` slot.
  const impact =
    refs.kind === "loading" ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-loading">
        Loading delete impact…
      </p>
    ) : refs.kind === "error" ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-error">
        Couldn't load delete impact ({refs.message}) — proceeding will still
        delete the agent.
      </p>
    ) : refs.references.length === 0 ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-empty">
        No referencing objects found.
      </p>
    ) : (
      <div data-testid="refs-list">
        <p className="mb-2 text-xs font-medium text-muted-foreground">
          Objects affected by this delete:
        </p>
        <ul className="space-y-1.5">
          {refs.references.map((r) => (
            <li
              key={`${r.kind}/${r.namespace}/${r.name}`}
              className="flex items-center justify-between gap-2 text-sm"
              data-testid={`ref-${r.name}`}
            >
              <span>
                <span className="font-mono text-xs">{r.kind}/{r.name}</span>
                {r.namespace && r.namespace !== detail.namespace && (
                  <span className="ml-1 text-xs text-muted-foreground">({r.namespace})</span>
                )}
              </span>
              <Badge
                variant={r.disposition === "gc" ? "warning" : "secondary"}
                className="text-[10px]"
              >
                {r.disposition === "gc" ? "will be deleted" : "will be orphaned"}
              </Badge>
            </li>
          ))}
        </ul>
      </div>
    );

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${detail.name}?`}
      description={`This will permanently delete the agent "${detail.name}" and may affect related objects (see below).`}
      confirmText={detail.name}
      confirmLabel="Delete agent"
      busy={deleting}
      impact={impact}
    />
  );
}

// ── Overview tab (spec summary + status timeline + Run panel) ────────────────
function OverviewTab({
  detail,
  onTraced,
}: {
  detail: AgentDetailResponse;
  onTraced: (traceId: string) => void;
}) {
  return (
    <div className="grid gap-6 lg:grid-cols-[1fr_20rem]">
      <div className="space-y-6">
        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Spec</p>
          <dl className="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
            <SpecKV k="Execution" v={detail.executionModel || "—"} />
            <SpecKV k="Image" v={<span className="font-mono text-xs">{detail.image || "—"}</span>} />
            <SpecKV k="Role" v={detail.role || "—"} />
            <SpecKV k="Scaling" v={`${detail.scaling.min} – ${detail.scaling.max}`} />
          </dl>
        </div>

        {/* Runtime sits adjacent to Spec — both are spec-level authoring concerns
            (J6 m76.6: regrouped from below Bindings). */}
        {detail.runtime && <RuntimeSection runtime={detail.runtime} />}

        <StatusTimeline conditions={detail.conditions} ready={detail.ready} phase={detail.phase} />

        {detail.versions.length > 0 && (
          <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="versions-list">
            <p className="mb-3 text-sm font-medium">Versions</p>
            <ul className="space-y-1.5">
              {detail.versions.map((v) => (
                <li key={v} className="flex items-center gap-2 text-sm">
                  <span className="font-mono text-xs">{v}</span>
                  {v === detail.latestVersion && (
                    <Badge variant="secondary" className="text-[10px]">
                      latest
                    </Badge>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}

        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Bindings</p>
          <BindingsList bindings={detail.bindings} />
        </div>

        <ImprovementLoopSection
          ns={detail.namespace}
          name={detail.name}
          conditions={detail.conditions}
          gatePhase={detail.gate?.phase}
          versions={detail.versions}
        />
      </div>

      <UseAgentPanel
        name={detail.name}
        executionModel={detail.executionModel}
        url={detail.url}
        ns={detail.namespace}
      />

      <ChatPanel
        ns={detail.namespace}
        name={detail.name}
        ready={detail.ready}
        memoryBound={detail.bindings.some((b) => b.kind === "memory")}
        onTraced={onTraced}
      />
    </div>
  );
}

function SpecKV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{k}</dt>
      <dd>{v}</dd>
    </>
  );
}

// ── Runtime section (m65.9, ADR 0058) ────────────────────────────────────────
// Read-only card rendered in the Overview tab when spec.runtime is present AND
// at least one sub-section has content (outputSchemaSet, toolPolicy, resilience).
// When runtime is absent or every sub-section is empty, nothing is rendered —
// no clutter for agents that don't use it.
function RuntimeSection({ runtime }: { runtime: AgentRuntimeDetail }) {
  const [schemaOpen, setSchemaOpen] = React.useState(false);

  const hasContent =
    runtime.outputSchemaSet || runtime.toolPolicy != null || runtime.resilience != null;
  if (!hasContent) return null;

  return (
    <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="runtime-section">
      <div className="mb-3 flex items-center gap-2">
        <SlidersHorizontal className="h-4 w-4 text-muted-foreground" />
        <p className="text-sm font-medium">Runtime</p>
      </div>

      <div className="space-y-4">
        {/* --- Structured output --- */}
        {runtime.outputSchemaSet && (
          <div data-testid="runtime-output-schema">
            <div className="flex items-center gap-2">
              <span className="text-sm text-muted-foreground">Structured output</span>
              <Badge variant="success" className="text-[10px]" data-testid="runtime-output-schema-badge">
                ✓ set
              </Badge>
              {/* J6(a) m76.6: outputSchemaSet=true but no body returned — make it
                  clear the schema is configured but not echoed (avoids the false
                  impression that "✓ set" with no expand = no schema). */}
              {!runtime.outputSchema && (
                <span
                  className="text-[10px] text-muted-foreground"
                  data-testid="runtime-output-schema-not-returned"
                >
                  (content not returned)
                </span>
              )}
            </div>
            {runtime.outputSchema && (
              <details
                open={schemaOpen}
                onToggle={(e) => setSchemaOpen((e.currentTarget as HTMLDetailsElement).open)}
                className="mt-1"
                data-testid="runtime-schema-details"
              >
                <summary className="cursor-pointer text-xs text-primary hover:underline">
                  {schemaOpen ? "Hide schema" : "Show schema"}
                </summary>
                <pre className="mt-1 max-h-40 overflow-y-auto rounded bg-surface-3 p-2 text-xs leading-relaxed">
                  {(() => {
                    try {
                      return JSON.stringify(JSON.parse(runtime.outputSchema), null, 2);
                    } catch {
                      return runtime.outputSchema;
                    }
                  })()}
                </pre>
              </details>
            )}
          </div>
        )}

        {/* --- Tool policy --- */}
        {runtime.toolPolicy && (
          <div data-testid="runtime-tool-policy">
            <div className="mb-1.5 flex items-baseline gap-2">
              <p className="text-sm text-muted-foreground">Tool policy</p>
              {/* J6(c) m76.6: honesty qualifier — tool-policy is an SDK-layer authoring
                  convention (the SDK enforces it inside the agent loop), not a hard
                  platform enforcement boundary at the network/proxy layer. */}
              <span
                className="text-[10px] text-muted-foreground"
                title="Tool policy is enforced by the agent SDK at runtime, not by a platform proxy. It is an authoring convention, not a hard network-level boundary."
                data-testid="runtime-tool-policy-note"
              >
                SDK-layer convention
              </span>
            </div>
            <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
              <dt className="text-muted-foreground">Default rule</dt>
              <dd>
                <Badge variant="secondary" className="text-[10px]">
                  {runtime.toolPolicy.default || "allow"}
                </Badge>
              </dd>
              {runtime.toolPolicy.parallelLimit !== undefined && runtime.toolPolicy.parallelLimit > 0 && (
                <>
                  <dt className="text-muted-foreground">Parallel limit</dt>
                  <dd className="text-sm">{runtime.toolPolicy.parallelLimit} concurrent calls</dd>
                </>
              )}
              {runtime.toolPolicy.forcedChoice && (
                <>
                  <dt className="text-muted-foreground">Forced choice</dt>
                  <dd className="font-mono text-xs">
                    {runtime.toolPolicy.forcedChoice}
                    <span className="ml-1 font-sans text-[10px] text-muted-foreground">
                      {runtime.toolPolicy.forcedChoice === "auto"
                        ? "(model chooses)"
                        : runtime.toolPolicy.forcedChoice === "required"
                          ? "(must call a tool)"
                          : `(must call ${runtime.toolPolicy.forcedChoice})`}
                    </span>
                  </dd>
                </>
              )}
            </dl>
            {runtime.toolPolicy.overrides.length > 0 && (
              <div className="mt-2" data-testid="runtime-tool-overrides">
                <p className="mb-1 text-xs text-muted-foreground">
                  Per-tool overrides ({runtime.toolPolicy.overrides.length})
                </p>
                <ul className="space-y-1">
                  {runtime.toolPolicy.overrides.map((o) => (
                    <li
                      key={o.name}
                      className="flex items-center gap-2 text-sm"
                      data-testid={`tool-override-${o.name}`}
                    >
                      <span className="font-mono text-xs">{o.name}</span>
                      <Badge variant="secondary" className="text-[10px]">
                        {o.rule}
                      </Badge>
                      {o.retryable && (
                        <Badge variant="outline" className="text-[10px]">
                          retryable
                        </Badge>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        )}

        {/* --- Resilience --- */}
        {runtime.resilience && (
          <div data-testid="runtime-resilience">
            <p className="mb-1.5 text-sm text-muted-foreground">Resilience</p>
            <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
              {runtime.resilience.modelCall && (
                <>
                  <dt className="text-muted-foreground">Model call</dt>
                  <dd className="text-xs">
                    {[
                      runtime.resilience.modelCall.timeoutSeconds
                        ? `${runtime.resilience.modelCall.timeoutSeconds}s timeout`
                        : null,
                      runtime.resilience.modelCall.maxRetries
                        ? `${runtime.resilience.modelCall.maxRetries} ${runtime.resilience.modelCall.maxRetries === 1 ? "retry" : "retries"}`
                        : null,
                    ]
                      .filter(Boolean)
                      .join(", ") || "—"}
                  </dd>
                </>
              )}
              {runtime.resilience.toolCall && (
                <>
                  <dt className="text-muted-foreground">Tool call</dt>
                  <dd className="text-xs">
                    {[
                      runtime.resilience.toolCall.timeoutSeconds
                        ? `${runtime.resilience.toolCall.timeoutSeconds}s timeout`
                        : null,
                      runtime.resilience.toolCall.maxRetries
                        ? `${runtime.resilience.toolCall.maxRetries} ${runtime.resilience.toolCall.maxRetries === 1 ? "retry" : "retries"}`
                        : null,
                    ]
                      .filter(Boolean)
                      .join(", ") || "—"}
                  </dd>
                  {runtime.resilience.toolCall.circuitBreaker && (
                    <>
                      <dt className="text-muted-foreground">Circuit breaker</dt>
                      <dd className="text-xs" data-testid="runtime-circuit-breaker">
                        opens at {runtime.resilience.toolCall.circuitBreaker.failureThreshold} failures
                        {runtime.resilience.toolCall.circuitBreaker.cooldownSeconds
                          ? `, ${runtime.resilience.toolCall.circuitBreaker.cooldownSeconds}s cooldown`
                          : ""}
                      </dd>
                    </>
                  )}
                </>
              )}
            </dl>
          </div>
        )}
      </div>
    </div>
  );
}

// StatusTimeline renders the readiness progression from the status conditions —
// the "watch it come alive" surface. Each condition is a dot (green True / red
// False / muted Unknown) + its type/reason/message.
function StatusTimeline({
  conditions,
  ready,
  phase,
}: {
  conditions: AgentCondition[];
  ready: boolean;
  phase: string;
}) {
  return (
    <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="status-timeline">
      <div className="mb-3 flex items-center gap-2">
        <p className="text-sm font-medium">Status timeline</p>
        <Badge variant={ready ? "success" : "warning"} className="text-[10px]">
          {phase || (ready ? "Ready" : "Pending")}
        </Badge>
      </div>
      {conditions.length === 0 ? (
        <p className="text-sm text-muted-foreground">No status conditions reported yet.</p>
      ) : (
        <ol className="space-y-3">
          {conditions.map((c, i) => {
            const tone =
              c.status === "True"
                ? "bg-success"
                : c.status === "False"
                  ? "bg-destructive"
                  : "bg-border-strong";
            return (
              <li key={`${c.type}-${i}`} className="flex gap-3" data-testid={`condition-${c.type}`}>
                <div className="flex flex-col items-center">
                  <span className={`mt-1 h-2.5 w-2.5 rounded-full ${tone}`} />
                  {i < conditions.length - 1 && <span className="mt-1 h-full w-px bg-border" />}
                </div>
                <div className="pb-1">
                  <p className="text-sm font-medium">
                    {c.type}
                    {c.reason && (
                      <span className="ml-2 font-normal text-muted-foreground">{c.reason}</span>
                    )}
                  </p>
                  {c.message && <p className="text-xs text-muted-foreground">{c.message}</p>}
                  {c.lastTransitionTime && (
                    <p className="text-[10px] text-muted-foreground">{c.lastTransitionTime}</p>
                  )}
                </div>
              </li>
            );
          })}
        </ol>
      )}
    </div>
  );
}


// ── Logs tab (live SSE tail, bearer-attached fetch-stream) ───────────────────
type LogLine = { seq: number; text: string };
type LogPhase = "connecting" | "waiting" | "streaming" | "ended" | "error" | "forbidden";

function LogsTab({ ns, name, ready }: { ns: string; name: string; ready: boolean }) {
  const [lines, setLines] = React.useState<LogLine[]>([]);
  const [phase, setPhase] = React.useState<LogPhase>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
  const seqRef = React.useRef(0);

  React.useEffect(() => {
    setLines([]);
    setPhase("connecting");
    setErrorMsg("");
    seqRef.current = 0;

    // The SSE tail over fetch-stream: the Bearer rides the request (EventSource
    // can't set headers). We follow the stream and render every frame honestly.
    const cancel = openLogStream(
      ns,
      name,
      {
        onEvent: (type: LogEventType, data: string) => {
          if (type === "log") {
            setPhase("streaming");
            setLines((prev) => [...prev, { seq: seqRef.current++, text: data }]);
          } else if (type === "waiting") {
            setPhase((p) => (p === "streaming" ? p : "waiting"));
          } else if (type === "error") {
            // An IN-STREAM error frame (mid-stream break / pods-log denied after
            // the stream opened) — surfaced honestly, distinct from a pre-stream
            // 403 (handled by onForbidden below).
            setPhase("error");
            setErrorMsg(data);
          } else if (type === "end") {
            setPhase((p) => (p === "error" || p === "forbidden" ? p : "ended"));
          }
        },
        // A PRE-STREAM 403 (RBAC denied pods list) — an HTTP status before any
        // frame. Rendered as a forbidden state, NOT an in-stream error.
        onForbidden: (message: string) => {
          setPhase("forbidden");
          setErrorMsg(message);
        },
        onError: (message: string) => {
          setPhase("error");
          setErrorMsg(message);
        },
      },
      { follow: true, tailLines: 200 },
    );

    // Cancel the stream on unmount / tab-switch — no leak.
    return cancel;
  }, [ns, name]);

  if (phase === "forbidden") {
    return (
      <ForbiddenInline
        title="Not allowed to read logs"
        description="Your account can't read pod logs in this namespace."
        detail={errorMsg}
      />
    );
  }

  return (
    <div className="overflow-hidden rounded-lg border bg-surface-3" data-testid="logs-tab">
      <div className="flex items-center justify-between border-b bg-card/60 px-4 py-2">
        <div className="flex items-center gap-2 text-sm">
          <Terminal className="h-4 w-4" /> Live tail
          {phase === "streaming" && (
            <span className="h-2 w-2 animate-pulse rounded-full bg-success" aria-label="streaming" />
          )}
        </div>
        <span className="text-xs text-muted-foreground" data-testid="logs-status">
          {phase === "connecting" && "connecting…"}
          {phase === "waiting" && "waiting for the agent to start"}
          {phase === "streaming" && `${lines.length} lines`}
          {phase === "ended" && "stream ended"}
          {phase === "error" && "stream error"}
        </span>
      </div>

      {phase === "waiting" && lines.length === 0 ? (
        <div
          className="flex h-40 items-center justify-center text-sm text-muted-foreground"
          data-testid="logs-waiting"
        >
          {ready
            ? "Waiting for the agent to start — no running pod yet."
            : "The agent is still coming up — waiting for its first pod."}
        </div>
      ) : (
        <pre className="max-h-80 overflow-y-auto p-4 font-mono text-xs leading-relaxed">
          {lines.map((l) => (
            <div key={l.seq} data-testid="log-line">
              {l.text}
            </div>
          ))}
          {phase === "error" && (
            <div className="mt-2 text-destructive" role="alert" data-testid="logs-error">
              — log stream error: {errorMsg}
            </div>
          )}
          {phase === "ended" && lines.length === 0 && (
            <div className="text-muted-foreground">No log output.</div>
          )}
        </pre>
      )}
    </div>
  );
}

// ── Per-agent Runs tab (m15.11) ───────────────────────────────────────────────
// Uses the bounded GET /api/agents/{ns}/{name}/runs endpoint. On 501 (Langfuse
// not configured) renders a calm "unavailable" empty state, never an error toast.
function AgentRunsTab({
  ns,
  name,
  onInspect,
}: {
  ns: string;
  name: string;
  onInspect: (traceId: string) => void;
}) {
  const [state, setState] = React.useState<
    | { kind: "loading" }
    | { kind: "ready"; runs: AgentRunSummary[] }
    | { kind: "unavailable" } // 501 — Langfuse not configured
    | { kind: "error"; message: string; forbidden: boolean }
  >({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .agentRuns(ns, name, 50, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (Langfuse not wired) — degrade calmly.
        if (res === null) {
          setState({ kind: "unavailable" });
          return;
        }
        setState({ kind: "ready", runs: res.runs });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load runs",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  const cols: Column<AgentRunSummary>[] = [
    {
      id: "traceId",
      header: "Run",
      cell: (r) => <span className="font-mono text-xs">{r.traceId}</span>,
    },
    { id: "name", header: "Name", hideOnMobile: true, cell: (r) => r.name },
    { id: "timestamp", header: "When", hideOnMobile: true, cell: (r) => r.timestamp },
    {
      id: "tokens",
      header: "Tokens",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">{r.tokens.toLocaleString()}</span>,
    },
    {
      id: "cost",
      header: "Cost",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">${r.costUSD.toFixed(3)}</span>,
    },
    {
      id: "latency",
      header: "Latency",
      className: "text-right",
      hideOnMobile: true,
      cell: (r) => <span className="tabular-nums">{Math.round(r.latencyMs)}ms</span>,
    },
  ];

  // 501-degrade: calm empty state, NOT an error toast or error state.
  if (state.kind === "unavailable") {
    return (
      <div
        className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
        data-testid="runs-unavailable"
      >
        Runs unavailable — tracing not configured (Langfuse not wired).
      </div>
    );
  }

  if (state.kind === "error" && state.forbidden) {
    return (
      <ForbiddenInline
        title="Not allowed to read runs"
        description="Your account can't read run history in this cluster."
        detail={state.message}
      />
    );
  }

  return (
    <div data-testid="runs-tab">
      <DataTable<AgentRunSummary>
        columns={cols}
        rows={state.kind === "ready" ? state.runs : []}
        rowKey={(r) => r.traceId}
        loading={state.kind === "loading"}
        error={
          state.kind === "error"
            ? { message: state.message, forbidden: false, onRetry: undefined }
            : null
        }
        onRowClick={(r) => onInspect(r.traceId)}
        ariaLabel="Agent runs"
        empty={{
          icon: Play,
          title: "No runs yet",
          description: "Run this agent from the Overview tab to see its traced runs here.",
        }}
      />
    </div>
  );
}

// ── Bindings tab / list ──────────────────────────────────────────────────────
function BindingsTab({ bindings }: { bindings: AgentBinding[] }) {
  return (
    <div data-testid="bindings-tab">
      <BindingsList bindings={bindings} />
    </div>
  );
}

const OTHER_TOOLS_GROUP = "Other tools";

function BindingsList({ bindings }: { bindings: AgentBinding[] }) {
  if (bindings.length === 0) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="bindings-empty">
        No bindings reference this agent yet.
      </p>
    );
  }

  // Tool bindings are grouped by MCP server and COLLAPSED by default (an agent can bind
  // dozens of tools — a flat list is an unscrollable wall). Non-tool bindings (memory, …)
  // are few, so they render flat below.
  const tools = bindings.filter((b) => b.kind === "tool");
  const others = bindings.filter((b) => b.kind !== "tool");

  const groups = new Map<string, AgentBinding[]>();
  for (const b of tools) {
    const key = b.server?.trim() || OTHER_TOOLS_GROUP;
    const arr = groups.get(key);
    if (arr) arr.push(b);
    else groups.set(key, [b]);
  }
  const serverGroups = [...groups.entries()].sort((a, b) => {
    if (a[0] === OTHER_TOOLS_GROUP) return 1;
    if (b[0] === OTHER_TOOLS_GROUP) return -1;
    return a[0].localeCompare(b[0]);
  });

  return (
    <div className="space-y-2" data-testid="bindings-list">
      {serverGroups.map(([server, group]) => {
        const readyCount = group.filter((b) => b.ready).length;
        const allReady = readyCount === group.length;
        return (
          <details
            key={server}
            className="group rounded-md border bg-surface-2/40"
            data-testid={`binding-group-${server}`}
          >
            <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-sm [&::-webkit-details-marker]:hidden">
              <div className="flex min-w-0 items-center gap-2">
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-90" />
                <Server className="h-4 w-4 shrink-0 text-primary" />
                <span className="truncate font-medium">{server}</span>
                <Badge variant="secondary" className="text-[10px]">
                  {group.length} tool{group.length === 1 ? "" : "s"}
                </Badge>
              </div>
              <Badge
                variant={allReady ? "success" : "warning"}
                className="shrink-0 text-[10px]"
              >
                {allReady ? "all ready" : `${readyCount}/${group.length} ready`}
              </Badge>
            </summary>
            <div className="space-y-1 border-t px-3 py-2">
              {group.map((b) => (
                <div
                  key={`${b.kind}/${b.name}`}
                  className="flex items-center justify-between gap-3 rounded px-2 py-1.5 text-sm hover:bg-accent/40"
                  data-testid={`binding-${b.name}`}
                >
                  <span className="truncate font-mono text-xs">
                    {b.detail || b.name}
                  </span>
                  <Badge
                    variant={b.ready ? "success" : "warning"}
                    className="shrink-0 text-[10px]"
                  >
                    {b.ready ? "ready" : "pending"}
                  </Badge>
                </div>
              ))}
            </div>
          </details>
        );
      })}

      {others.map((b) => (
        <div
          key={`${b.kind}/${b.name}`}
          className="flex items-center justify-between gap-3 rounded-md border bg-surface-2/40 px-4 py-3 text-sm"
          data-testid={`binding-${b.name}`}
        >
          <div className="flex min-w-0 items-center gap-2">
            <Badge variant="secondary" className="text-[10px]">
              {b.kind}
            </Badge>
            <span className="truncate">{b.detail || b.name}</span>
          </div>
          <Badge variant={b.ready ? "success" : "warning"} className="text-[10px]">
            {b.ready ? "ready" : "pending"}
          </Badge>
        </div>
      ))}
    </div>
  );
}

// ── Memory panel (m17.11) ────────────────────────────────────────────────────
// Shows the MemoryBinding(s) that reference this agent (filtered by agentRef).
// Supports attach (create), edit, and detach (typed-name delete). RBAC-aware:
//   • attach/edit/detach are gated on can("memorybindings", verb)
//   • a forced 403 surfaces honestly in the form; viewers see read-only.

type MemoryForm = {
  scope: string;
  backend: string;
};

type MemoryActionState =
  | { kind: "idle" }
  | { kind: "attach-open"; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "edit-open"; binding: MemoryBindingSummary; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "detach-open"; binding: MemoryBindingSummary; busy: boolean };

type MemoryPanelLoad =
  | { kind: "loading" }
  | { kind: "ready"; bindings: MemoryBindingSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

function MemoryPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can, reprobe } = useCapabilities();
  const { toast } = useToast();
  const canCreate = can(RES_MEMORY, "create");
  const canUpdate = can(RES_MEMORY, "update");
  const canDelete = can(RES_MEMORY, "delete");

  const [load, setLoad] = React.useState<MemoryPanelLoad>({ kind: "loading" });
  const [action, setAction] = React.useState<MemoryActionState>({ kind: "idle" });
  const [form, setForm] = React.useState<MemoryForm>({ scope: "", backend: "" });

  const fetchBindings = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .listMemoryBindings({ namespace: ns }, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.items.filter((b) => b.agentRef === agentName);
        setLoad({ kind: "ready", bindings: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load memory bindings",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => {
    const cancel = fetchBindings();
    return cancel;
  }, [fetchBindings]);

  function openAttach() {
    setForm({ scope: "", backend: "" });
    setAction({ kind: "attach-open", busy: false, error: null, forbidden: false });
  }

  function openEdit(binding: MemoryBindingSummary) {
    setForm({ scope: binding.scope, backend: binding.backend ?? "" });
    setAction({ kind: "edit-open", binding, busy: false, error: null, forbidden: false });
  }

  function openDetach(binding: MemoryBindingSummary) {
    setAction({ kind: "detach-open", binding, busy: false });
  }

  async function doAttach() {
    if (action.kind !== "attach-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.createMemoryBinding({
        namespace: ns,
        agentRef: agentName,
        scope: form.scope.trim(),
        backend: form.backend.trim() || undefined,
      });
      toast({ variant: "success", title: "Memory binding attached", description: `Scope "${form.scope}" attached to ${agentName}.` });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "attach failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doEdit() {
    if (action.kind !== "edit-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.updateMemoryBinding(ns, action.binding.name, {
        scope: form.scope.trim(),
        backend: form.backend.trim() || undefined,
      });
      toast({ variant: "success", title: "Memory binding updated" });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doDetach() {
    if (action.kind !== "detach-open") return;
    setAction({ ...action, busy: true });
    try {
      await api.removeMemoryBinding(ns, action.binding.name);
      toast({ variant: "success", title: "Memory binding detached", description: `Binding "${action.binding.name}" removed.` });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      toast({ variant: "error", title: "Detach failed", description: err instanceof Error ? err.message : "detach failed" });
      setAction({ kind: "idle" });
    }
  }

  const isAttachOpen = action.kind === "attach-open";
  const isEditOpen = action.kind === "edit-open";
  const isDetachOpen = action.kind === "detach-open";
  const formError = (action.kind === "attach-open" || action.kind === "edit-open") ? action.error : null;
  const formForbidden = (action.kind === "attach-open" || action.kind === "edit-open") ? action.forbidden : false;
  const formBusy = (action.kind === "attach-open" || action.kind === "edit-open" || action.kind === "detach-open") ? action.busy : false;

  return (
    <div data-testid="memory-panel">
      <div className="mb-4 flex items-center justify-between">
        <div>
          <p className="text-sm font-medium">Memory bindings</p>
          <p className="text-xs text-muted-foreground">
            The session &amp; shared memory backends wired to this agent (configuration).
          </p>
        </div>
        {canCreate && (
          <Button
            variant="outline"
            size="sm"
            onClick={openAttach}
            data-testid="memory-attach"
          >
            <Plus className="h-4 w-4" />
            Attach
          </Button>
        )}
      </div>

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="memory-loading">Loading…</p>
      )}
      {load.kind === "error" && load.forbidden && (
        <ForbiddenInline
          title="Not allowed to list memory bindings"
          description="Your account can't read MemoryBindings in this namespace."
          detail={load.message}
        />
      )}
      {load.kind === "error" && !load.forbidden && (
        <p className="text-sm text-destructive" role="alert" data-testid="memory-error">
          {load.message}
        </p>
      )}
      {load.kind === "ready" && load.bindings.length === 0 && (
        <EmptyState
          icon={Boxes}
          title="No memory bindings"
          description="Attach a memory binding to configure this agent's session and shared memory backend. (Long-term, semantically-retrievable memory is shown separately below.)"
        />
      )}
      {load.kind === "ready" && load.bindings.length > 0 && (
        <ul className="space-y-2">
          {load.bindings.map((b) => (
            <li
              key={b.name}
              className="flex items-center justify-between gap-3 rounded-md border bg-surface-2/40 px-4 py-3 text-sm"
              data-testid={`memory-binding-${b.name}`}
            >
              <div className="flex min-w-0 items-center gap-3">
                <Badge variant="secondary" className="text-[10px]">scope</Badge>
                <span className="font-medium">{b.scope}</span>
                {b.backend && (
                  <span className="text-xs text-muted-foreground">via {b.backend}</span>
                )}
              </div>
              <div className="flex items-center gap-2">
                <Badge variant={b.ready ? "success" : "warning"} className="text-[10px]">
                  {b.ready ? "ready" : "pending"}
                </Badge>
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => openEdit(b)} data-testid={`memory-edit-${b.name}`}>
                    <Pencil className="h-3.5 w-3.5" />
                  </Button>
                )}
                {canDelete && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => openDetach(b)}
                    className="text-destructive hover:text-destructive"
                    data-testid={`memory-detach-${b.name}`}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* Attach / edit form inline */}
      {(isAttachOpen || isEditOpen) && (
        <div className="mt-4 rounded-lg border bg-card p-4 shadow-card">
          <p className="mb-3 text-sm font-medium">{isAttachOpen ? "Attach memory binding" : "Edit memory binding"}</p>
          <div className="space-y-3">
            <FormField id="memory-scope" label="Scope">
              <Input
                id="memory-scope"
                value={form.scope}
                onChange={(e) => setForm((f) => ({ ...f, scope: e.target.value }))}
                placeholder="global"
                data-testid="memory-scope-input"
              />
            </FormField>
            <FormField id="memory-backend" label="Backend (optional)">
              <Input
                id="memory-backend"
                value={form.backend}
                onChange={(e) => setForm((f) => ({ ...f, backend: e.target.value }))}
                placeholder="redis"
                data-testid="memory-backend-input"
              />
            </FormField>
            {formForbidden && (
              <ForbiddenInline
                title="Not allowed to manage memory bindings"
                description="Your account can't create or update MemoryBindings."
                detail={formError ?? undefined}
              />
            )}
            {formError && !formForbidden && (
              <p className="text-sm text-destructive" role="alert" data-testid="memory-form-error">
                {formError}
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "idle" })} disabled={formBusy}>
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={isAttachOpen ? doAttach : doEdit}
                disabled={!form.scope.trim() || formBusy}
                data-testid="memory-form-submit"
              >
                {formBusy ? "Saving…" : isAttachOpen ? "Attach" : "Save"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Typed-name detach confirmation */}
      {isDetachOpen && action.kind === "detach-open" && (
        <ConfirmDialog
          open={true}
          onCancel={() => setAction({ kind: "idle" })}
          onConfirm={doDetach}
          title={`Detach memory binding?`}
          description={`This will remove the binding "${action.binding.name}" from ${agentName}. The agent will lose access to the "${action.binding.scope}" memory scope.`}
          confirmText={action.binding.name}
          confirmLabel="Detach"
          busy={action.busy}
        />
      )}

      <LongTermMemoryConfigPanel ns={ns} agentName={agentName} />
      <LongTermMemoryPanel ns={ns} agentName={agentName} />
    </div>
  );
}

// ── Long-term memory viewer (m46.6, ADR 0045) ────────────────────────────────
// Read-only list of the agent's AGENT-WIDE long-term memories (persistent,
// semantically-retrievable knowledge). Per-user memories are never shown
// (privacy). Degrades to "unavailable" on 501 (no control-plane store), and to a
// friendly empty state when the agent has remembered nothing yet.

type LongTermLoad =
  | { kind: "loading" }
  | { kind: "ready"; items: AgentMemoryEntry[] }
  | { kind: "unavailable" }
  | { kind: "error"; message: string; forbidden: boolean };

// LongTermMemoryConfigPanel (m49.3) — the ENABLE surface for M46's folded long-term-memory capability. The
// console could VIEW an agent's long-term memories (LongTermMemoryPanel) but had no way to TURN THE
// CAPABILITY ON (the m49.1 capability pocket). Patches spec.longTermMemory via the BFF (the tracepolicy
// pattern). Read-only for viewers (the form is gated on the agent-update capability); hides if unreadable.
function LongTermMemoryConfigPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can } = useCapabilities();
  const canUpdate = can(RES_AGENTS, "update");
  const [config, setConfig] = React.useState<LongTermMemoryConfig | null>(null);
  const [route, setRoute] = React.useState("");
  const [perUser, setPerUser] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState<string | null>(null);

  const apply = React.useCallback((c: LongTermMemoryConfig) => {
    setConfig(c);
    setRoute(c.embeddingRoute ?? "");
    setPerUser(c.perUser);
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .longTermMemoryConfig(ns, agentName, controller.signal)
      .then((c) => !controller.signal.aborted && apply(c))
      .catch(() => !controller.signal.aborted && setConfig(null));
    return () => controller.abort();
  }, [ns, agentName, apply]);

  function save(enabled: boolean) {
    setBusy(true);
    setErr(null);
    api
      .setLongTermMemory(ns, agentName, {
        enabled,
        perUser,
        embeddingRoute: route.trim() || undefined,
      })
      .then(apply)
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : "update failed"))
      .finally(() => setBusy(false));
  }

  if (config === null) return null; // unreadable (403/404) — hide, no noise

  return (
    <div className="mt-8 border-t pt-6" data-testid="longterm-config">
      <h3 className="mb-1 text-sm font-medium">Long-term memory</h3>
      <p className="mb-3 text-xs text-muted-foreground">
        Let this agent remember facts across conversations and recall them by meaning (ADR 0045).
      </p>
      <div className="flex items-center gap-2 text-sm">
        <Badge variant={config.enabled ? "success" : "secondary"} data-testid="longterm-state">
          {config.enabled ? "Enabled" : "Disabled"}
        </Badge>
        {config.enabled && (
          <span className="text-xs text-muted-foreground">
            {config.perUser ? "per-user" : "agent-wide"}
            {config.embeddingRoute ? ` · route ${config.embeddingRoute}` : ""}
          </span>
        )}
      </div>
      {canUpdate && (
        <div className="mt-3 space-y-2" data-testid="longterm-config-form">
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={perUser}
              onChange={(e) => setPerUser(e.target.checked)}
              data-testid="longterm-peruser"
            />
            Per-user memory (each end-user's own facts, isolated)
          </label>
          <Input
            placeholder="Embedding route (optional — a ModelRoute name)"
            value={route}
            onChange={(e) => setRoute(e.target.value)}
            data-testid="longterm-route"
          />
          <div className="flex gap-2">
            {config.enabled ? (
              <>
                <Button size="sm" variant="outline" disabled={busy} onClick={() => save(true)} data-testid="longterm-save">
                  Save
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive hover:text-destructive"
                  disabled={busy}
                  onClick={() => save(false)}
                  data-testid="longterm-disable"
                >
                  Disable
                </Button>
              </>
            ) : (
              <Button size="sm" disabled={busy} onClick={() => save(true)} data-testid="longterm-enable">
                Enable
              </Button>
            )}
          </div>
          {err && (
            <p className="text-sm text-destructive" data-testid="longterm-config-error">
              {err}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function LongTermMemoryPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const [load, setLoad] = React.useState<LongTermLoad>({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .agentMemory(ns, agentName, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoad(res === null ? { kind: "unavailable" } : { kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load long-term memory",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  // Unavailable (no store wired) — hide the section entirely, like other degraded surfaces.
  if (load.kind === "unavailable") return null;

  return (
    <div className="mt-8 border-t pt-6" data-testid="longterm-memory-panel">
      <h3 className="mb-1 text-sm font-medium">Long-term memory</h3>
      <p className="mb-3 text-xs text-muted-foreground">
        Facts this agent has remembered and can recall by meaning across conversations. Only
        agent-wide memories appear here; per-user memories are scoped to each end-user's own
        conversations and are never exposed in the console, for privacy.
      </p>

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="longterm-loading">Loading…</p>
      )}
      {load.kind === "error" && (
        <p className="text-sm text-destructive" data-testid="longterm-error">
          {load.forbidden ? "Not allowed to read this agent's memory." : load.message}
        </p>
      )}
      {load.kind === "ready" && load.items.length === 0 && (
        <div data-testid="longterm-empty">
          <EmptyState
            icon={Boxes}
            title="Nothing remembered yet"
            description="When this agent stores a long-term memory (via memory.remember), its agent-wide facts will appear here."
          />
        </div>
      )}
      {load.kind === "ready" && load.items.length > 0 && (
        <ul className="space-y-2" aria-label="Long-term memories" data-testid="longterm-list">
          {load.items.map((m, i) => (
            <li
              key={`${m.createdAt}-${i}`}
              className="rounded-md border bg-card p-3 text-sm shadow-card"
              data-testid="longterm-item"
            >
              <p className="whitespace-pre-wrap">{m.content}</p>
              {m.tags && Object.keys(m.tags).length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1" data-testid="longterm-tags">
                  {Object.entries(m.tags).map(([k, v]) => (
                    <Badge key={k} variant="secondary" className="text-[10px]">
                      {k}: {v}
                    </Badge>
                  ))}
                </div>
              )}
              <div className="mt-1 flex flex-wrap items-center gap-x-3 text-xs text-muted-foreground">
                <span>{formatTimestamp(m.createdAt)}</span>
                {/* memory→trace back-link (m54.3, M49 UX review A2): jump from a
                    remembered fact to the run/trace that produced it. Absent when
                    the memory was written outside a traced run. */}
                {m.traceId && (
                  <Link
                    to={`/traces/${encodeURIComponent(m.traceId)}`}
                    data-testid={`longterm-trace-link-${m.traceId}`}
                    aria-label={`View the trace that produced this memory (${m.traceId})`}
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <Activity className="h-3 w-3" />
                    trace
                  </Link>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// formatTimestamp renders an ISO timestamp in the viewer's locale, falling back to the raw
// string if it is missing or unparseable (never the literal "Invalid Date").
function formatTimestamp(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

// ── Scaling panel (m17.11) ────────────────────────────────────────────────────
// Shows the AgentScalingPolicy for this agent (filtered by agentRef). Supports
// attach (create), edit, and detach (typed-name delete). RBAC-aware. A 422
// from the CRD XValidations (max < min, or schedule-without-scheduled-mode)
// surfaces in the form with the server's message — never faked as success.

type ScalingForm = {
  minReplicas: string;
  maxReplicas: string;
  mode: string;
  schedule: string;
};

type ScalingActionState =
  | { kind: "idle" }
  | { kind: "attach-open"; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "edit-open"; policy: AgentScalingPolicySummary; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "detach-open"; policy: AgentScalingPolicySummary; busy: boolean };

type ScalingPanelLoad =
  | { kind: "loading" }
  | { kind: "ready"; policies: AgentScalingPolicySummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

function ScalingPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can, reprobe } = useCapabilities();
  const { toast } = useToast();
  const canCreate = can(RES_SCALING, "create");
  const canUpdate = can(RES_SCALING, "update");
  const canDelete = can(RES_SCALING, "delete");

  const [load, setLoad] = React.useState<ScalingPanelLoad>({ kind: "loading" });
  const [action, setAction] = React.useState<ScalingActionState>({ kind: "idle" });
  const [form, setForm] = React.useState<ScalingForm>({ minReplicas: "0", maxReplicas: "3", mode: "static", schedule: "" });
  // Keep-warm (m32.5): a one-switch view over the agent's min-replicas floor — "warm" iff any
  // attached policy holds ≥1 replica, so a latency-sensitive agent avoids Knative cold-starts.
  const [keepWarmBusy, setKeepWarmBusy] = React.useState(false);

  const fetchPolicies = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .listAgentScalingPolicies({ namespace: ns }, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.items.filter((p) => p.agentRef === agentName);
        setLoad({ kind: "ready", policies: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load scaling policies",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => {
    const cancel = fetchPolicies();
    return cancel;
  }, [fetchPolicies]);

  function openAttach() {
    setForm({ minReplicas: "0", maxReplicas: "3", mode: "static", schedule: "" });
    setAction({ kind: "attach-open", busy: false, error: null, forbidden: false });
  }

  function openEdit(policy: AgentScalingPolicySummary) {
    setForm({
      minReplicas: String(policy.minReplicas),
      maxReplicas: String(policy.maxReplicas),
      mode: policy.mode ?? "static",
      schedule: policy.schedule ?? "",
    });
    setAction({ kind: "edit-open", policy, busy: false, error: null, forbidden: false });
  }

  function openDetach(policy: AgentScalingPolicySummary) {
    setAction({ kind: "detach-open", policy, busy: false });
  }

  async function doAttach() {
    if (action.kind !== "attach-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.createAgentScalingPolicy({
        namespace: ns,
        agentRef: agentName,
        minReplicas: parseInt(form.minReplicas, 10) || 0,
        maxReplicas: parseInt(form.maxReplicas, 10) || 1,
        mode: form.mode || undefined,
        schedule: form.mode === "scheduled" && form.schedule.trim() ? form.schedule.trim() : undefined,
      });
      toast({ variant: "success", title: "Scaling policy attached" });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      // 422 = XValidation rejection (max<min, schedule-without-scheduled-mode) —
      // surfaced in the form with the server's message. NOT faked as a success.
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "attach failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doEdit() {
    if (action.kind !== "edit-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.updateAgentScalingPolicy(ns, action.policy.name, {
        minReplicas: parseInt(form.minReplicas, 10) || 0,
        maxReplicas: parseInt(form.maxReplicas, 10) || 1,
        mode: form.mode || undefined,
        schedule: form.mode === "scheduled" && form.schedule.trim() ? form.schedule.trim() : undefined,
      });
      toast({ variant: "success", title: "Scaling policy updated" });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  // doKeepWarm flips the agent's min-replicas floor (m32.5): ON holds ≥1 replica warm (creating a
  // default policy if none is attached), OFF returns every attached policy to scale-to-zero (min 0).
  async function doKeepWarm(enable: boolean) {
    if (load.kind !== "ready") return;
    setKeepWarmBusy(true);
    try {
      if (load.policies.length === 0) {
        if (enable) {
          await api.createAgentScalingPolicy({
            namespace: ns,
            agentRef: agentName,
            minReplicas: 1,
            maxReplicas: 3,
          });
        }
      } else {
        for (const p of load.policies) {
          const target = enable ? Math.max(1, p.minReplicas) : 0;
          if (target !== p.minReplicas) {
            await api.updateAgentScalingPolicy(ns, p.name, { minReplicas: target });
          }
        }
      }
      toast({
        variant: "success",
        title: enable ? "Keeping the agent warm" : "Scale-to-zero enabled",
      });
      fetchPolicies();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      toast({
        variant: "error",
        title: "Couldn't change keep-warm",
        description: err instanceof Error ? err.message : "failed",
      });
    } finally {
      setKeepWarmBusy(false);
    }
  }

  async function doDetach() {
    if (action.kind !== "detach-open") return;
    setAction({ ...action, busy: true });
    try {
      await api.removeAgentScalingPolicy(ns, action.policy.name);
      toast({ variant: "success", title: "Scaling policy detached", description: `Policy "${action.policy.name}" removed.` });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      toast({ variant: "error", title: "Detach failed", description: err instanceof Error ? err.message : "detach failed" });
      setAction({ kind: "idle" });
    }
  }

  const isAttachOpen = action.kind === "attach-open";
  const isEditOpen = action.kind === "edit-open";
  const isDetachOpen = action.kind === "detach-open";
  const formError = (action.kind === "attach-open" || action.kind === "edit-open") ? action.error : null;
  const formForbidden = (action.kind === "attach-open" || action.kind === "edit-open") ? action.forbidden : false;
  const formBusy = (action.kind === "attach-open" || action.kind === "edit-open" || action.kind === "detach-open") ? action.busy : false;

  // warm iff any attached policy holds ≥1 replica (the min-replicas floor is up, no cold-starts).
  const warm =
    load.kind === "ready" && load.policies.some((p) => p.minReplicas >= 1);

  return (
    <div data-testid="scaling-panel">
      <div className="mb-4 flex items-center justify-between">
        <p className="text-sm font-medium">Scaling policies</p>
        {canCreate && (
          <Button
            variant="outline"
            size="sm"
            onClick={openAttach}
            data-testid="scaling-attach"
          >
            <Plus className="h-4 w-4" />
            Attach
          </Button>
        )}
      </div>

      {load.kind === "ready" && (canCreate || canUpdate) && (
        <div
          className="mb-4 flex items-center justify-between rounded-md border p-3"
          data-testid="keep-warm"
        >
          <div className="space-y-0.5">
            <p className="text-sm font-medium">Keep warm</p>
            <p className="text-xs text-muted-foreground">
              Hold at least one replica ready so latency-sensitive invokes skip the
              Knative cold-start.
            </p>
          </div>
          <Button
            variant={warm ? "default" : "outline"}
            size="sm"
            disabled={keepWarmBusy}
            aria-pressed={warm}
            onClick={() => void doKeepWarm(!warm)}
            data-testid="keep-warm-toggle"
          >
            {keepWarmBusy ? "Saving…" : warm ? "On" : "Off"}
          </Button>
        </div>
      )}

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="scaling-loading">Loading…</p>
      )}
      {load.kind === "error" && load.forbidden && (
        <ForbiddenInline
          title="Not allowed to list scaling policies"
          description="Your account can't read AgentScalingPolicies in this namespace."
          detail={load.message}
        />
      )}
      {load.kind === "error" && !load.forbidden && (
        <p className="text-sm text-destructive" role="alert" data-testid="scaling-error">
          {load.message}
        </p>
      )}
      {load.kind === "ready" && load.policies.length === 0 && (
        <EmptyState
          icon={SlidersHorizontal}
          title="No scaling policies"
          description="Attach a scaling policy to control how this agent scales."
        />
      )}
      {load.kind === "ready" && load.policies.length > 0 && (
        <ul className="space-y-2">
          {load.policies.map((p) => (
            <li
              key={p.name}
              className="flex items-center justify-between gap-3 rounded-md border bg-surface-2/40 px-4 py-3 text-sm"
              data-testid={`scaling-policy-${p.name}`}
            >
              <div className="flex min-w-0 items-center gap-3">
                <Badge variant="secondary" className="text-[10px]">{p.mode ?? "static"}</Badge>
                <span className="font-medium">{p.minReplicas}–{p.maxReplicas} replicas</span>
                {p.schedule && (
                  <span className="font-mono text-xs text-muted-foreground">{p.schedule}</span>
                )}
              </div>
              <div className="flex items-center gap-2">
                <Badge variant={p.ready ? "success" : "warning"} className="text-[10px]">
                  {p.ready ? "ready" : "pending"}
                </Badge>
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => openEdit(p)} data-testid={`scaling-edit-${p.name}`}>
                    <Pencil className="h-3.5 w-3.5" />
                  </Button>
                )}
                {canDelete && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => openDetach(p)}
                    className="text-destructive hover:text-destructive"
                    data-testid={`scaling-detach-${p.name}`}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* Attach / edit form inline */}
      {(isAttachOpen || isEditOpen) && (
        <div className="mt-4 rounded-lg border bg-card p-4 shadow-card">
          <p className="mb-3 text-sm font-medium">{isAttachOpen ? "Attach scaling policy" : "Edit scaling policy"}</p>
          <div className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <FormField id="scaling-min" label="Min replicas">
                <Input
                  id="scaling-min"
                  inputMode="numeric"
                  value={form.minReplicas}
                  onChange={(e) => setForm((f) => ({ ...f, minReplicas: e.target.value }))}
                  data-testid="scaling-min-input"
                />
              </FormField>
              <FormField id="scaling-max" label="Max replicas">
                <Input
                  id="scaling-max"
                  inputMode="numeric"
                  value={form.maxReplicas}
                  onChange={(e) => setForm((f) => ({ ...f, maxReplicas: e.target.value }))}
                  data-testid="scaling-max-input"
                />
              </FormField>
            </div>
            <FormField id="scaling-mode" label="Mode">
              <Select
                id="scaling-mode"
                value={form.mode}
                onChange={(e) => setForm((f) => ({ ...f, mode: e.target.value }))}
                data-testid="scaling-mode-input"
              >
                <option value="static">static</option>
                <option value="scheduled">scheduled</option>
              </Select>
            </FormField>
            {form.mode === "scheduled" && (
              <FormField id="scaling-schedule" label="Schedule (cron)">
                <Input
                  id="scaling-schedule"
                  value={form.schedule}
                  onChange={(e) => setForm((f) => ({ ...f, schedule: e.target.value }))}
                  placeholder="0 8 * * 1-5"
                  data-testid="scaling-schedule-input"
                />
              </FormField>
            )}
            {/* 422 from XValidations (max<min, schedule-without-scheduled) surfaces here
                with the server message — never a fabricated success. */}
            {formForbidden && (
              <ForbiddenInline
                title="Not allowed to manage scaling policies"
                description="Your account can't create or update AgentScalingPolicies."
                detail={formError ?? undefined}
              />
            )}
            {formError && !formForbidden && (
              <p className="text-sm text-destructive" role="alert" data-testid="scaling-form-error">
                {formError}
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "idle" })} disabled={formBusy}>
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={isAttachOpen ? doAttach : doEdit}
                disabled={formBusy}
                data-testid="scaling-form-submit"
              >
                {formBusy ? "Saving…" : isAttachOpen ? "Attach" : "Save"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Typed-name detach confirmation */}
      {isDetachOpen && action.kind === "detach-open" && (
        <ConfirmDialog
          open={true}
          onCancel={() => setAction({ kind: "idle" })}
          onConfirm={doDetach}
          title="Detach scaling policy?"
          description={`This will remove the scaling policy "${action.policy.name}" from ${agentName}.`}
          confirmText={action.policy.name}
          confirmLabel="Detach"
          busy={action.busy}
        />
      )}
    </div>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────
function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

// Keep the old RunsTab export for backward compatibility with existing tests
// that import RunSummary-based runs from the global /api/runs endpoint.
// This is ONLY used by old tests; the new AgentRunsTab uses the per-agent endpoint.
function RunsTab({
  agentName,
  onInspect,
}: {
  agentName: string;
  onInspect: (traceId: string) => void;
}) {
  const [state, setState] = React.useState<
    | { kind: "loading" }
    | { kind: "ready"; runs: RunSummary[] }
    | { kind: "error"; message: string; forbidden: boolean }
  >({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .runs(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.runs.filter((r) => r.name === agentName);
        setState({ kind: "ready", runs: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load runs",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [agentName]);

  const cols: Column<RunSummary>[] = [
    {
      id: "traceId",
      header: "Run",
      cell: (r) => <span className="font-mono text-xs">{r.traceId}</span>,
    },
    { id: "timestamp", header: "When", hideOnMobile: true, cell: (r) => r.timestamp },
    {
      id: "tokens",
      header: "Tokens",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">{r.tokens.toLocaleString()}</span>,
    },
    {
      id: "cost",
      header: "Cost",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">${r.costUSD.toFixed(3)}</span>,
    },
    {
      id: "latency",
      header: "Latency",
      className: "text-right",
      hideOnMobile: true,
      cell: (r) => <span className="tabular-nums">{Math.round(r.latencyMs)}ms</span>,
    },
  ];

  if (state.kind === "error" && state.forbidden) {
    return (
      <ForbiddenInline
        title="Not allowed to read runs"
        description="Your account can't read run history in this cluster."
        detail={state.message}
      />
    );
  }

  return (
    <DataTable<RunSummary>
      columns={cols}
      rows={state.kind === "ready" ? state.runs : []}
      rowKey={(r) => r.traceId}
      loading={state.kind === "loading"}
      error={
        state.kind === "error"
          ? { message: state.message, forbidden: false, onRetry: undefined }
          : null
      }
      onRowClick={(r) => onInspect(r.traceId)}
      ariaLabel="Recent runs"
      empty={{
        icon: Play,
        title: "No runs yet",
        description: "Run this agent from the Overview tab to see its traced runs here.",
      }}
    />
  );
}

// Keep exported for any backward-compat consumers that import it directly.
export { RunsTab };

// ── Redaction policy editor (m18.14, ADR 0019) ────────────────────────────────
// The per-agent custom redaction detectors (name + RE2 pattern), on top of the
// always-on built-in redaction. Editing gates on agentdeployments update; a bad
// name/regex (rejected by the CRD validation) surfaces inline as a 422. No secret
// material is ever rendered.
type RedactionLoad =
  | { kind: "loading" }
  | { kind: "ready" }
  | { kind: "error"; message: string; forbidden: boolean };

function RedactionPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can } = useCapabilities();
  const canEdit = can(RES_AGENTS, "update");
  const { toast } = useToast();
  const [load, setLoad] = React.useState<RedactionLoad>({ kind: "loading" });
  const [rows, setRows] = React.useState<{ name: string; pattern: string }[]>([]);
  const [saving, setSaving] = React.useState(false);
  const [saveError, setSaveError] = React.useState<string | null>(null);

  const reload = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .getTracePolicy(ns, agentName, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRows(res.customDetectors ?? []);
        setLoad({ kind: "ready" });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const forbidden = err instanceof ApiError && err.isForbidden;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the redaction policy",
          forbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => reload(), [reload]);

  async function save() {
    setSaving(true);
    setSaveError(null);
    try {
      const cleaned = rows.filter((r) => r.name.trim() || r.pattern.trim());
      const res = await api.updateTracePolicy(ns, agentName, { customDetectors: cleaned });
      setRows(res.customDetectors ?? []);
      toast({
        variant: "success",
        title: "Redaction policy saved",
        description: `${cleaned.length} custom detector${cleaned.length === 1 ? "" : "s"}.`,
      });
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "save failed");
    } finally {
      setSaving(false);
    }
  }

  if (load.kind === "loading") {
    return (
      <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground shadow-card">
        Loading the redaction policy…
      </div>
    );
  }
  if (load.kind === "error") {
    return (
      <div className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card" role="alert">
        {load.forbidden
          ? "Not allowed to read this agent's redaction policy."
          : load.message}
      </div>
    );
  }

  return (
    <div className="space-y-4 rounded-lg border bg-card p-6 shadow-card" data-testid="redaction-panel">
      <div>
        <p className="text-sm font-medium">Custom redaction detectors</p>
        <p className="mt-1 text-xs text-muted-foreground">
          Extra named regex rules applied to trace payloads before they are stored —
          on top of the always-on built-in detectors (emails, keys, SSNs). Each match
          becomes <span className="font-mono">[REDACTED:name]</span>.
        </p>
      </div>

      {rows.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No custom detectors — only the built-in redaction is active.
        </p>
      )}

      <div className="space-y-2">
        {rows.map((r, i) => (
          <div key={i} className="flex items-center gap-2" data-testid={`detector-${i}`}>
            <Input
              aria-label={`Detector ${i} name`}
              placeholder="name (e.g. badge)"
              value={r.name}
              disabled={!canEdit}
              className="w-40"
              onChange={(e) =>
                setRows((rs) => rs.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))
              }
            />
            <Input
              aria-label={`Detector ${i} pattern`}
              placeholder="RE2 pattern (e.g. BADGE-[0-9]+)"
              value={r.pattern}
              disabled={!canEdit}
              className="flex-1 font-mono"
              onChange={(e) =>
                setRows((rs) => rs.map((x, j) => (j === i ? { ...x, pattern: e.target.value } : x)))
              }
            />
            {canEdit && (
              <Button
                variant="ghost"
                size="sm"
                data-testid={`remove-detector-${i}`}
                onClick={() => setRows((rs) => rs.filter((_, j) => j !== i))}
              >
                Remove
              </Button>
            )}
          </div>
        ))}
      </div>

      {saveError && (
        <p className="text-sm text-destructive" role="alert" data-testid="redaction-error">
          {saveError}
        </p>
      )}

      {canEdit && (
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            data-testid="add-detector"
            onClick={() => setRows((rs) => [...rs, { name: "", pattern: "" }])}
          >
            Add detector
          </Button>
          <Button
            size="sm"
            disabled={saving}
            data-testid="save-redaction"
            onClick={() => void save()}
          >
            {saving ? "Saving…" : "Save policy"}
          </Button>
        </div>
      )}
    </div>
  );
}

// ── Improvement-Loop Section (m69.11, ADR 0062) ───────────────────────────────
// Surfaces on the Overview tab. Shows:
//   1. The serving version's 3-component online score (operational/feedback/judge)
//      + a RegressionDetected badge (from status.conditions).
//   2. When gate.phase == "canary": two arms side-by-side (old vs candidate) each
//      with their per-version online-score components.
//   3. A rollback button (confirm-guarded): picks a version from history, POSTs
//      the rollback annotation via the caller's token. Degrades calmly when the
//      online-score store is unconfigured (501 = calm "not available").

type OnlineScoreLoad =
  | { kind: "loading" }
  | { kind: "ready"; data: OnlineScoreResponse }
  | { kind: "unavailable" }  // 501 — store not configured
  | { kind: "error"; message: string };

function ImprovementLoopSection({
  ns,
  name,
  conditions,
  gatePhase,
  versions,
}: {
  ns: string;
  name: string;
  conditions: AgentCondition[];
  gatePhase?: string;
  versions: string[];
}) {
  const { toast } = useToast();
  const [scoreLoad, setScoreLoad] = React.useState<OnlineScoreLoad>({ kind: "loading" });
  const [rollbackTarget, setRollbackTarget] = React.useState<string>("");
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const [rolling, setRolling] = React.useState(false);

  React.useEffect(() => {
    const controller = new AbortController();
    setScoreLoad({ kind: "loading" });
    api
      .agentOnlineScore(ns, name, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        if (res === null) {
          setScoreLoad({ kind: "unavailable" });
          return;
        }
        setScoreLoad({ kind: "ready", data: res });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setScoreLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load online score",
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  // RegressionDetected condition from status.conditions.
  const regressionCond = conditions.find((c) => c.type === "RegressionDetected");
  const regressionDetected = regressionCond?.status === "True";

  const isCanary = gatePhase === "canary";

  // Per-version windows for canary arms (group the most recent window per version).
  const latestByVersion = React.useMemo((): Map<string, OnlineScoreWindow> => {
    if (scoreLoad.kind !== "ready") return new Map();
    const map = new Map<string, OnlineScoreWindow>();
    for (const w of scoreLoad.data.windows) {
      if (!map.has(w.agentVersion)) map.set(w.agentVersion, w);
    }
    return map;
  }, [scoreLoad]);

  async function doRollback() {
    if (!rollbackTarget) return;
    setRolling(true);
    try {
      await api.agentRollback(ns, name, rollbackTarget);
      toast({
        variant: "success",
        title: "Rollback requested",
        description: `Annotation set: rollback to "${rollbackTarget}" requested. The controller will actuate it.`,
      });
      setConfirmOpen(false);
      setRollbackTarget("");
    } catch (err) {
      toast({
        variant: "error",
        title: "Rollback failed",
        description: err instanceof Error ? err.message : "rollback failed",
      });
    } finally {
      setRolling(false);
    }
  }

  // If the store is unavailable AND no RegressionDetected AND no canary AND no versions,
  // render nothing (no noise for simple agents with no eval suite).
  if (
    scoreLoad.kind === "unavailable" &&
    !regressionDetected &&
    !isCanary &&
    versions.length === 0
  ) {
    return null;
  }

  return (
    <div
      className="rounded-lg border bg-card p-5 shadow-card space-y-4"
      data-testid="improvement-loop-section"
    >
      <div className="flex items-center gap-2">
        <Activity className="h-4 w-4 text-muted-foreground" />
        <p className="text-sm font-medium">Online score</p>
        {regressionDetected && (
          <Badge variant="destructive" className="text-[10px]" data-testid="regression-detected-badge">
            <AlertTriangle className="mr-1 h-3 w-3" />
            Regression detected
          </Badge>
        )}
        {regressionCond && !regressionDetected && regressionCond.status === "False" && (
          <Badge variant="success" className="text-[10px]" data-testid="regression-ok-badge">
            Healthy
          </Badge>
        )}
      </div>

      {/* Online score content */}
      {scoreLoad.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="online-score-loading">
          Loading online score…
        </p>
      )}
      {scoreLoad.kind === "unavailable" && (
        <p className="text-sm text-muted-foreground" data-testid="online-score-unavailable">
          Online score not available — control-plane store not configured.
        </p>
      )}
      {scoreLoad.kind === "error" && (
        <p className="text-sm text-destructive" data-testid="online-score-error">
          {scoreLoad.message}
        </p>
      )}

      {scoreLoad.kind === "ready" && (
        <>
          {scoreLoad.data.windows.length === 0 ? (
            <p className="text-sm text-muted-foreground" data-testid="online-score-empty">
              No score data yet — no production runs recorded for this agent.
            </p>
          ) : isCanary ? (
            /* Canary arms: two versions side-by-side */
            <CanaryArms latestByVersion={latestByVersion} />
          ) : (
            /* Serving version: most-recent window */
            <OnlineScoreCard window={scoreLoad.data.windows[0]} />
          )}
        </>
      )}

      {/* Rollback button — shown when versions available (RBAC-permissive: server enforces) */}
      {versions.length > 1 && (
        <div className="flex items-center gap-3 pt-2 border-t" data-testid="rollback-section">
          <RotateCcw className="h-4 w-4 text-muted-foreground shrink-0" />
          <p className="text-sm text-muted-foreground shrink-0">Rollback to</p>
          <select
            value={rollbackTarget}
            onChange={(e) => setRollbackTarget(e.target.value)}
            className="flex-1 rounded-md border bg-background px-3 py-1.5 text-sm"
            data-testid="rollback-version-select"
          >
            <option value="">— choose a version —</option>
            {versions.map((v) => (
              <option key={v} value={v}>
                {v}
              </option>
            ))}
          </select>
          <Button
            variant="outline"
            size="sm"
            disabled={!rollbackTarget}
            onClick={() => setConfirmOpen(true)}
            data-testid="rollback-button"
          >
            Rollback
          </Button>
        </div>
      )}

      {/* Confirm dialog — guards the destructive annotation write */}
      <ConfirmDialog
        open={confirmOpen}
        onCancel={() => setConfirmOpen(false)}
        onConfirm={() => void doRollback()}
        title={`Rollback ${name} to ${rollbackTarget}?`}
        description={`This will set the rollback annotation on ${name}. The controller will revert the serving spec to version "${rollbackTarget}", subject to cooldown and flap guards. The annotation is one-shot (cleared after evaluation).`}
        confirmLabel="Rollback"
        busy={rolling}
        destructive
      />
    </div>
  );
}

// OnlineScoreCard renders the most-recent window for the serving version —
// all 3 components with clear labels so the operator sees the full picture.
function OnlineScoreCard({ window: w }: { window: OnlineScoreWindow }) {
  const errorRate = w.operational.total > 0
    ? ((w.operational.errorCount / w.operational.total) * 100).toFixed(1)
    : "—";
  const toolFailRate = w.operational.total > 0
    ? ((w.operational.toolFailCount / w.operational.total) * 100).toFixed(1)
    : "—";
  const feedbackAvg = w.feedback.count > 0
    ? (w.feedback.sumVal / w.feedback.count).toFixed(2)
    : "—";
  const judgeAvg = w.judge.count > 0
    ? (w.judge.sumVal / w.judge.count).toFixed(2)
    : "—";

  return (
    <div
      className="grid grid-cols-3 gap-3 text-sm"
      data-testid="online-score-card"
    >
      {/* Operational */}
      <div className="rounded-md border bg-surface-2/40 p-3" data-testid="operational-component">
        <p className="text-xs font-medium text-muted-foreground mb-2">Operational</p>
        <dl className="space-y-1">
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Requests</dt>
            <dd className="tabular-nums">{w.operational.total.toLocaleString()}</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Error rate</dt>
            <dd className="tabular-nums">{errorRate}%</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Tool fail</dt>
            <dd className="tabular-nums">{toolFailRate}%</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">p95 latency</dt>
            <dd className="tabular-nums">{w.operational.latencyP95Ms.toFixed(0)}ms</dd>
          </div>
        </dl>
      </div>

      {/* Feedback */}
      <div className="rounded-md border bg-surface-2/40 p-3" data-testid="feedback-component">
        <p className="text-xs font-medium text-muted-foreground mb-2">Feedback</p>
        <dl className="space-y-1">
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Count</dt>
            <dd className="tabular-nums">{w.feedback.count}</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Avg score</dt>
            <dd className="tabular-nums">{feedbackAvg}</dd>
          </div>
        </dl>
      </div>

      {/* Judge */}
      <div className="rounded-md border bg-surface-2/40 p-3" data-testid="judge-component">
        <p className="text-xs font-medium text-muted-foreground mb-2">Judge</p>
        <dl className="space-y-1">
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Count</dt>
            <dd className="tabular-nums">{w.judge.count}</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-xs text-muted-foreground">Avg score</dt>
            <dd className="tabular-nums">{judgeAvg}</dd>
          </div>
        </dl>
      </div>
    </div>
  );
}

// CanaryArms renders the two canary arms side-by-side when gate.phase == "canary":
// OLD (baseline serving revision) vs CANDIDATE (new revision being canary-tested).
// Each arm shows its own online-score components so regressions are visible before
// the promotion decision is made.
function CanaryArms({
  latestByVersion,
}: {
  latestByVersion: Map<string, OnlineScoreWindow>;
}) {
  // Derive the two arms: we have ≤N versions; we show all distinct versions.
  // When exactly 2 versions: label the newest as Candidate, the other as Baseline.
  const versions = [...latestByVersion.keys()];
  if (versions.length === 0) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="canary-arms-empty">
        No per-version score data yet — still accumulating.
      </p>
    );
  }

  // Sort by the window start of the version's latest window, newest last = candidate.
  const sorted = versions.sort((a, b) => {
    const wa = latestByVersion.get(a)?.windowStart ?? "";
    const wb = latestByVersion.get(b)?.windowStart ?? "";
    return wa.localeCompare(wb);
  });

  return (
    <div className="space-y-3" data-testid="canary-arms">
      <p className="text-xs text-muted-foreground">
        Canary in progress — comparing serving arms:
      </p>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        {sorted.map((v: string, i: number) => {
          const w = latestByVersion.get(v)!;
          const label = sorted.length === 2
            ? i === 0 ? "Baseline (old)" : "Candidate (new)"
            : v;
          return (
            <div
              key={v}
              className="rounded-md border bg-surface-2/40 p-3 space-y-2"
              data-testid={`canary-arm-${i === 0 ? "old" : "candidate"}`}
            >
              <div className="flex items-center gap-2">
                <Badge
                  variant={sorted.length === 2 && i === 1 ? "secondary" : "outline"}
                  className="text-[10px]"
                >
                  {label}
                </Badge>
                <span className="font-mono text-[10px] text-muted-foreground truncate">{v}</span>
              </div>
              <OnlineScoreCard window={w} />
            </div>
          );
        })}
      </div>
    </div>
  );
}



// ── Publish-as-template dialog (m74.6) ───────────────────────────────────────
// Mirrors the m73.7 PublishDialog in mcp-servers-page.tsx. Pick visibility
// (team/org/public) → POST /api/templates → on success close; on 403 surface
// the tier requirement honestly. Public requires an explicit confirm checkbox
// (blast-radius acknowledgement).
type PublishVisibility = "team" | "org" | "public";

function PublishTemplateDialog({
  agentNamespace,
  agentName,
  alreadyPublished,
  onClose,
  onDone,
}: {
  agentNamespace: string;
  agentName: string;
  // U7: whether the agent is already published this session — warn against silent re-publish.
  alreadyPublished?: boolean;
  onClose: () => void;
  // U8: onDone now passes back the publish response + chosen visibility.
  onDone: (res: PublishTemplateResponse, visibility: string) => void;
}) {
  const { toast } = useToast();
  const [selected, setSelected] = React.useState<PublishVisibility>("team");
  const [publicConfirmed, setPublicConfirmed] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // U8 / U12: inline error state — keep dialog open on failure with an error message.
  const [inlineError, setInlineError] = React.useState<string | null>(null);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  function handleSelect(v: PublishVisibility) {
    setSelected(v);
    if (v !== "public") setPublicConfirmed(false);
    setInlineError(null);
  }

  // U7: block re-publish at the same visibility (prevent silent overwrite without acknowledgement).
  const isPublishDisabled =
    busy || (selected === "public" && !publicConfirmed);

  async function onPublish() {
    if (isPublishDisabled) return;
    setInlineError(null);
    setBusy(true);
    try {
      const res = await api.publishTemplate("agent", agentNamespace, agentName, selected);
      toast({
        variant: "success",
        title: "Shared as template",
        description: `${agentName} v${res.version ?? "1"} is now available as a ${selected}-visible template.`,
      });
      onDone(res, selected);
    } catch (err) {
      const isForbidden = err instanceof ApiError && err.isForbidden;
      // U8 / U12: keep dialog open, show error inline instead of closing.
      const errMsg = isForbidden
        ? `You need ${
            selected === "public"
              ? "Platform-admin"
              : selected === "org"
              ? "Tenant-admin"
              : "team-admin"
          } rights to share ${selected}-wide.`
        : err instanceof Error
        ? err.message
        : "publish failed";
      setInlineError(errMsg);
      setBusy(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${agentName} as template`}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">
          Share {agentName} as a template
        </h2>
        {/* U8: immutable snapshot copy note */}
        <p className="mt-1 text-sm text-muted-foreground">
          Publishing shares an <strong>immutable snapshot</strong> of the current definition —
          your secrets and credentials are never shared. Publish again to share a new version.
        </p>
        {/* U7: warn if already published */}
        {alreadyPublished && (
          <p
            className="mt-2 text-sm text-amber-600 dark:text-amber-400"
            data-testid="publish-template-already-published-warning"
          >
            This agent is already published. Publishing again creates a new version at the
            selected visibility.
          </p>
        )}
        <div className="mt-4 space-y-2">
          {(["team", "org", "public"] as PublishVisibility[]).map((v) => (
            <label
              key={v}
              className="flex cursor-pointer items-center gap-3 rounded-md border p-3 hover:bg-accent/40"
              data-testid={`publish-template-option-${v}`}
            >
              <input
                type="radio"
                name="template-visibility"
                value={v}
                checked={selected === v}
                onChange={() => handleSelect(v)}
                className="accent-primary"
              />
              <div>
                <p className="font-medium capitalize">{v}</p>
                <p className="text-xs text-muted-foreground">
                  {v === "team"
                    ? "Visible to your team's namespace"
                    : v === "org"
                    ? "Visible org-wide (Tenant-admin required)"
                    : "Visible to everyone (Platform-admin required)"}
                </p>
              </div>
            </label>
          ))}
        </div>
        {selected === "public" && (
          <div className="mt-3 space-y-2">
            <p className="text-sm text-amber-600 dark:text-amber-400" data-testid="publish-template-public-warning">
              Public means every tenant on this cluster can discover and fork this template.
            </p>
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={publicConfirmed}
                onChange={(e) => setPublicConfirmed(e.target.checked)}
                className="accent-primary"
                data-testid="publish-template-public-confirm"
              />
              I understand this template is discoverable by all tenants
            </label>
          </div>
        )}
        {/* U8 / U12: inline error — keep dialog open, no HTTP copy leak */}
        {inlineError && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="publish-template-error"
          >
            {inlineError}
          </p>
        )}
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onPublish()}
            disabled={isPublishDisabled}
            data-testid="publish-template-submit"
          >
            {/* U8: rename from "Publish as X" to "Share as template" */}
            {busy ? "Sharing…" : "Share as template"}
          </Button>
        </div>
      </div>
    </div>
  );
}
