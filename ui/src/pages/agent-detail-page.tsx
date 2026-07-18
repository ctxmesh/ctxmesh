import * as React from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  AlertTriangle,
  Boxes,
  ExternalLink,
  MessageSquare,
  Pencil,
  Play,
  Plus,
  Send,
  SlidersHorizontal,
  Terminal,
  Trash2,
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
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { ChatMarkdown } from "@/components/chat-markdown";
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
  type AgentRunSummary,
  type AgentScalingPolicySummary,
  type AgentSimplifiedSpec,
  type LogEventType,
  type MemoryBindingSummary,
  type RunSummary,
} from "@/lib/api";
import { extractAgentOutput } from "@/lib/agent-output";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS, RES_MEMORY, RES_SCALING } from "@/lib/nav";
import { MCP_OAUTH_MESSAGE } from "@/lib/oauth-popup";

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
  const [tab, setTab] = React.useState<Tab>("Overview");
  // The trace to inspect — set when a run returns a traceId; opens the inspector
  // drawer over the page (list context preserved).
  const [inspectTrace, setInspectTrace] = React.useState<string | null>(null);

  // Edit + delete dialogs are opened by ?edit=1 / ?delete=1 search params so
  // they survive a hard reload and can be triggered from the list's row actions.
  const editOpen = searchParams.get("edit") === "1";
  const deleteOpen = searchParams.get("delete") === "1";

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
            description={`No AgentDeployment "${name}" in ${ns || "this namespace"}. It may have been deleted, or the name is wrong.`}
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

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="agent-detail-page">
      <AgentHeader
        detail={detail}
        onEdit={openEdit}
        onDelete={openDelete}
      />

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
    </div>
  );
}

// ── Header ──────────────────────────────────────────────────────────────────
function AgentHeader({
  detail,
  onEdit,
  onDelete,
}: {
  detail: AgentDetailResponse;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { can } = useCapabilities();
  const canEdit = can(RES_AGENTS, "update");
  const canDelete = can(RES_AGENTS, "delete");

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
        <span className="text-sm text-muted-foreground">{detail.namespace}</span>
        {/* RBAC-aware write affordances — hidden for viewers */}
        <div className="ml-auto flex items-center gap-2">
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
                to="/prompts"
                className="truncate text-primary hover:underline"
                data-testid="agent-promptref-link"
              >
                {detail.promptRef}
              </Link>
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
            description="Your account can't update AgentDeployments in this cluster."
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
      </div>

      <UseAgentPanel
        name={detail.name}
        executionModel={detail.executionModel}
        url={detail.url}
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

// ── Chat panel (conversation-style interaction, m29.7) ───────────────────────
// A turn-by-turn chat with a deployed agent, threaded on the framework's OWN
// conversationId → the memory plane: one stable id per chat session lets a
// memory-bound agent hold context across turns (state-layer.md). Under the hood
// each message is still one traced /invoke (ADR 0011) — the chat just threads
// them with a shared X-Conversation-Id (m29.5) so the stock managed loop replays
// prior turns (m29.6). Each agent turn keeps the trace-id link (opens the run
// inspector ON DEMAND, never automatically) and the inline per-user Connect
// banner (ADR 0031) — connecting re-sends the same message in place.
type ChatTurn = {
  id: number;
  role: "user" | "agent";
  text: string;
  // agent turns carry the user input that produced them, so a post-connect resume
  // re-invokes the SAME message without appending a duplicate user turn.
  sourceText?: string;
  traceId?: string;
  consentRequired?: string[];
  pending?: boolean;
  error?: string;
  // a forbidden invoke (viewer without invoke rights) gets the ForbiddenInline
  // treatment — the API is the real gate, not the SPA's create capability.
  forbidden?: boolean;
};

// newConversationId mints a key-safe thread id (no ':' '/' ' ' — the launcher's
// validateConversationID rules). randomUUID is hex+hyphen only; the fallback keeps
// jsdom (no crypto.randomUUID) working in tests.
function newConversationId(): string {
  const rand =
    typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
  return `chat-${rand}`;
}

function ChatPanel({
  ns,
  name,
  ready,
  memoryBound,
  onTraced,
}: {
  ns: string;
  name: string;
  ready: boolean;
  memoryBound: boolean;
  onTraced: (traceId: string) => void;
}) {
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
  const [conversationId, setConversationId] = React.useState(newConversationId);
  const [turns, setTurns] = React.useState<ChatTurn[]>([]);
  const [draft, setDraft] = React.useState("");
  // Inline consent (ADR 0031, m26.2) — a message whose tool call needs the user's own
  // credential surfaces a "Connect <server>" CTA in that agent turn.
  const [connecting, setConnecting] = React.useState<string | null>(null);
  const [connectError, setConnectError] = React.useState<string | null>(null);
  const idRef = React.useRef(0);
  const scrollRef = React.useRef<HTMLDivElement>(null);

  const busy = connecting !== null || turns.some((t) => t.pending);

  React.useEffect(() => {
    // Keep the newest turn in view as the thread grows.
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [turns]);

  function nextId(): number {
    idRef.current += 1;
    return idRef.current;
  }

  // runInvoke drives ONE traced /invoke for the given agent turn, threading the
  // conversationId. Reused by the initial send and the post-connect resume so a
  // resumed turn updates IN PLACE rather than appending a duplicate.
  async function runInvoke(text: string, agentTurnId: number) {
    setTurns((ts) =>
      ts.map((t) => (t.id === agentTurnId ? { ...t, pending: true, error: undefined } : t)),
    );
    try {
      const res = await api.invoke({
        agent: name,
        namespace: ns,
        input: { input: text },
        conversationId,
      });
      setTurns((ts) =>
        ts.map((t) =>
          t.id === agentTurnId
            ? {
                ...t,
                pending: false,
                text: res.response,
                traceId: res.traceId,
                consentRequired: res.consentRequired,
              }
            : t,
        ),
      );
    } catch (err) {
      const forbidden = err instanceof ApiError && err.isForbidden;
      if (forbidden) reprobe();
      setTurns((ts) =>
        ts.map((t) =>
          t.id === agentTurnId
            ? {
                ...t,
                pending: false,
                error: err instanceof Error ? err.message : "run failed",
                forbidden,
              }
            : t,
        ),
      );
    }
  }

  async function send() {
    const text = draft.trim();
    if (!text || busy) return;
    const userId = nextId();
    const agentId = nextId();
    setTurns((ts) => [
      ...ts,
      { id: userId, role: "user", text },
      { id: agentId, role: "agent", text: "", pending: true, sourceText: text },
    ]);
    setDraft("");
    await runInvoke(text, agentId);
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    // Enter sends; Shift+Enter inserts a newline (the chat convention).
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  // onConnect runs the INLINE per-user consent (ADR 0031): begin the OAuth grant for the
  // named server, open the provider consent in a POPUP so the chat stays on screen, then
  // re-send the SAME message when it completes so the fresh credential is injected. The
  // token never touches the SPA (server-side exchange). Popup blocked → redirect fallback.
  async function onConnect(server: string, text: string, agentTurnId: number) {
    setConnectError(null);
    setConnecting(server);
    let authorizationURL: string;
    try {
      const res = await api.beginMcpGrant({
        server,
        namespace: ns,
        // Scope the grant to THIS agent's trust boundary (its registry, or itself) — the
        // consent empowers this agent's team, not every agent the user owns (ADR 0033).
        agent: name,
        redirectUri: `${window.location.origin}/api/mcp/oauth/callback`,
      });
      authorizationURL = res.authorizationURL;
    } catch (err) {
      setConnecting(null);
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setConnectError(err instanceof Error ? err.message : "Couldn't start the connect flow.");
      return;
    }

    const popup = window.open(
      authorizationURL,
      "ctxmesh-oauth-connect",
      "width=520,height=680,menubar=no,toolbar=no",
    );
    if (!popup) {
      window.location.href = authorizationURL;
      return;
    }

    let done = false;
    let poll = 0;
    function finish() {
      if (done) return;
      done = true;
      window.removeEventListener("message", onMessage);
      window.clearInterval(poll);
      setConnecting(null);
      void runInvoke(text, agentTurnId); // resume the same turn with the fresh credential
    }
    function onMessage(e: MessageEvent) {
      if (e.origin !== window.location.origin) return;
      const data = e.data as { type?: string } | null;
      if (data?.type === MCP_OAUTH_MESSAGE) finish();
    }
    window.addEventListener("message", onMessage);
    poll = window.setInterval(() => {
      if (popup.closed) finish();
    }, 700);
  }

  function newChat() {
    setTurns([]);
    setConversationId(newConversationId());
    setConnectError(null);
    setDraft("");
  }

  function renderTurn(t: ChatTurn) {
    if (t.role === "user") {
      return (
        <div key={t.id} className="flex flex-col items-end gap-1" data-testid="chat-turn-user">
          <span className="px-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            You
          </span>
          <div className="max-w-[80%] whitespace-pre-wrap break-words rounded-2xl rounded-br-sm bg-primary px-3.5 py-2 text-xs text-primary-foreground shadow-sm">
            {t.text}
          </div>
        </div>
      );
    }
    const traceId = t.traceId;
    return (
      <div key={t.id} className="flex flex-col items-start gap-1" data-testid="chat-turn-agent">
        <span className="flex items-center gap-1 px-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
          <MessageSquare className="h-3 w-3" />
          {name}
        </span>
        <div className="w-full max-w-[92%] space-y-2 rounded-2xl rounded-bl-sm border border-border bg-card px-3.5 py-2.5">
          {t.pending ? (
            <span
              className="inline-flex items-center gap-1 py-1"
              data-testid="chat-pending"
              aria-label="Agent is thinking"
            >
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40 [animation-delay:-0.25s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40 [animation-delay:-0.12s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40" />
            </span>
          ) : t.error ? (
            t.forbidden ? (
              <ForbiddenInline
                title="Not allowed to run this agent"
                description="Your account can't invoke agents in this cluster."
                detail={t.error}
              />
            ) : (
              <span className="text-xs text-destructive" role="alert" data-testid="chat-turn-error">
                {t.error}
              </span>
            )
          ) : (
            <>
              {t.consentRequired && t.consentRequired.length > 0 && (
                <div
                  className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-xs"
                  data-testid="chat-consent"
                >
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
                  <div className="space-y-2">
                    <p className="font-medium">Connect your account to continue</p>
                    <p className="text-muted-foreground">
                      This message needs your own credentials. Connect, and it re-sends
                      automatically.
                    </p>
                    <div className="flex flex-wrap gap-2">
                      {t.consentRequired.map((server) => (
                        <Button
                          key={server}
                          size="sm"
                          variant="outline"
                          disabled={connecting !== null}
                          onClick={() => void onConnect(server, t.sourceText ?? "", t.id)}
                          data-testid={`connect-${server}`}
                        >
                          {connecting === server ? "Connecting…" : `Connect ${server}`}
                        </Button>
                      ))}
                    </div>
                    {connectError && (
                      <p className="text-destructive" data-testid="connect-error">
                        {connectError}
                      </p>
                    )}
                  </div>
                </div>
              )}
              {t.text && <ChatMarkdown>{extractAgentOutput(t.text)}</ChatMarkdown>}
              {traceId && (
                <button
                  type="button"
                  onClick={() => onTraced(traceId)}
                  className="pt-0.5 font-mono text-[11px] text-muted-foreground hover:text-primary hover:underline"
                  data-testid="open-trace"
                >
                  trace {traceId} →
                </button>
              )}
            </>
          )}
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col rounded-lg border bg-card shadow-card" data-testid="chat-panel">
      <div className="flex items-center justify-between gap-2 border-b px-4 py-3">
        <div className="flex items-center gap-2">
          <span className="flex h-7 w-7 items-center justify-center rounded-full bg-primary/10 text-primary">
            <MessageSquare className="h-4 w-4" />
          </span>
          <div className="leading-tight">
            <p className="text-sm font-medium">Chat</p>
            <p className="text-[11px] text-muted-foreground" data-testid="chat-memory-hint">
              {!canRun
                ? "read-only access"
                : memoryBound
                  ? "keeps context across turns"
                  : "no memory — won't remember earlier turns"}
            </p>
          </div>
        </div>
        {turns.length > 0 && (
          <Button size="sm" variant="ghost" onClick={newChat} data-testid="chat-new">
            New chat
          </Button>
        )}
      </div>

      {!canRun ? (
        <p
          className="m-4 rounded-md border border-dashed bg-card/40 px-3 py-2 text-xs text-muted-foreground"
          data-testid="chat-readonly-note"
        >
          You have read-only access — chatting with an agent requires create permission on
          AgentDeployments.
        </p>
      ) : (
        <>
          {!ready && (
            <p
              className="mx-4 mt-3 rounded-md border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning-foreground"
              data-testid="chat-not-ready-note"
            >
              The agent isn't Ready yet — a message may fail until it comes up.
            </p>
          )}

          <div
            ref={scrollRef}
            className="flex min-h-[18rem] flex-col gap-4 overflow-y-auto px-4 py-4"
            style={{ maxHeight: "32rem" }}
            data-testid="chat-thread"
          >
            {turns.length === 0 ? (
              <div
                className="m-auto max-w-xs text-center text-xs text-muted-foreground"
                data-testid="chat-empty"
              >
                <MessageSquare className="mx-auto mb-2 h-6 w-6 opacity-40" />
                <p>Start a conversation with {name}.</p>
                <p className="mt-1 opacity-80">
                  {memoryBound
                    ? "It remembers earlier turns in this chat."
                    : "Each message is independent (no memory bound)."}
                </p>
              </div>
            ) : (
              turns.map(renderTurn)
            )}
          </div>

          <div className="border-t p-3">
            <div className="flex items-end gap-2">
              <Textarea
                aria-label="Chat message"
                rows={2}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={onKeyDown}
                placeholder="Message the agent…"
                className="resize-none text-xs"
                data-testid="chat-input"
              />
              <Button
                size="icon"
                onClick={() => void send()}
                disabled={busy || draft.trim() === ""}
                data-testid="chat-send"
                aria-label="Send message"
                className="h-9 w-9 shrink-0"
              >
                <Send className="h-4 w-4" />
              </Button>
            </div>
            <p className="mt-1.5 px-0.5 text-[10px] text-muted-foreground">
              Enter to send · Shift+Enter for a new line
            </p>
          </div>
        </>
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

function BindingsList({ bindings }: { bindings: AgentBinding[] }) {
  if (bindings.length === 0) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="bindings-empty">
        No bindings reference this agent yet.
      </p>
    );
  }
  return (
    <div className="space-y-2">
      {bindings.map((b) => (
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
        <p className="text-sm font-medium">Memory bindings</p>
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
          description="Attach a memory binding to give this agent long-term memory."
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
    </div>
  );
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
