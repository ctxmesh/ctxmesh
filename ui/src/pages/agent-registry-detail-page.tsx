import * as React from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Pencil, Plus, Trash2, Users, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  ConfirmDialog,
  DetailDrawer,
  EmptyState,
  ForbiddenInline,
  ResourceLink,
  Wizard,
  type WizardStep,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import {
  api,
  ApiError,
  type AgentRegistryDetail,
  type LabelSelectorDTO,
  type RegistryGuardsDTO,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";

// AgentRegistryDetailPage — detail + edit wizard + typed-name delete.
//
// KEY CONSTRAINTS:
//   1. NO egress/allowlist field anywhere — the egress NetworkPolicy is
//      controller-managed; the console cannot alter the egress posture.
//   2. registryId is immutable after creation — shown READ-ONLY on edit.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: AgentRegistryDetail }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function AgentRegistryDetailPage() {
  const { ns = "", name = "" } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [state, setState] = React.useState<Load>({ kind: "loading" });

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
      .agentRegistryDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the registry",
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
        <p className="text-sm text-muted-foreground" data-testid="registry-detail-loading">
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
            description="Your account can't read this agent registry."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="registry-not-found">
          <EmptyState
            icon={Users}
            title="Agent registry not found"
            description={`No AgentRegistry "${name}" in ${ns || "this namespace"}.`}
            action={{ label: "Back to registries", onClick: () => navigate("/registries") }}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="registry-detail-error"
        >
          Couldn't load the registry: {state.message}
        </div>
      </div>
    );
  }

  const detail = state.detail;
  return (
    <RegistryDetailContent
      detail={detail}
      editOpen={editOpen}
      deleteOpen={deleteOpen}
      onOpenEdit={openEdit}
      onCloseEdit={closeEdit}
      onOpenDelete={openDelete}
      onCloseDelete={closeDelete}
      onDeleted={() => navigate("/registries")}
      onSaved={() => { closeEdit(); load(); }}
    />
  );
}

function RegistryDetailContent({
  detail,
  editOpen,
  deleteOpen,
  onOpenEdit,
  onCloseEdit,
  onOpenDelete,
  onCloseDelete,
  onDeleted,
  onSaved,
}: {
  detail: AgentRegistryDetail;
  editOpen: boolean;
  deleteOpen: boolean;
  onOpenEdit: () => void;
  onCloseEdit: () => void;
  onOpenDelete: () => void;
  onCloseDelete: () => void;
  onDeleted: () => void;
  onSaved: () => void;
}) {
  const { can } = useCapabilities();
  const canEdit = can(RES_REGISTRIES, "update");
  const canDelete = can(RES_REGISTRIES, "delete");

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="registry-detail-page">
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold tracking-tight">{detail.name}</h2>
          <Badge variant={detail.status.ready ? "success" : "warning"}>
            {detail.status.phase || (detail.status.ready ? "Ready" : "Pending")}
          </Badge>
          <span className="text-sm text-muted-foreground">{detail.namespace}</span>
          <div className="ml-auto flex items-center gap-2">
            {canEdit && (
              <Button
                variant="outline"
                size="sm"
                onClick={onOpenEdit}
                data-testid="edit-registry-button"
              >
                <Pencil className="h-4 w-4" />
                Edit
              </Button>
            )}
            {canDelete && (
              <Button
                variant="outline"
                size="sm"
                onClick={onOpenDelete}
                className="text-destructive hover:text-destructive"
                data-testid="delete-registry-button"
              >
                <Trash2 className="h-4 w-4" />
                Delete
              </Button>
            )}
          </div>
        </div>
      </div>

      {/* Spec panel — registryId shown read-only; NO egress field */}
      <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="registry-detail-panel">
        <p className="mb-3 text-sm font-medium">Configuration</p>
        <dl className="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
          <dt className="text-muted-foreground">Registry ID</dt>
          {/* registryId is immutable — always shown read-only */}
          <dd className="font-mono text-xs" data-testid="registry-id-display">{detail.registryId}</dd>
          <dt className="text-muted-foreground">Roles</dt>
          <dd>
            {detail.roles.length > 0
              ? detail.roles.map((r) => (
                  <Badge key={r} variant="secondary" className="mr-1 text-[10px]">
                    {r}
                  </Badge>
                ))
              : <span className="text-muted-foreground">—</span>}
          </dd>
          {detail.guards && (
            <>
              <dt className="text-muted-foreground">Max depth</dt>
              <dd>{detail.guards.maxDepth ?? "—"}</dd>
              <dt className="text-muted-foreground">Hop budget</dt>
              <dd>{detail.guards.hopBudget ?? "—"}</dd>
            </>
          )}
        </dl>

        {Object.keys(detail.memberSelector.matchLabels ?? {}).length > 0 && (
          <div className="mt-4">
            <p className="mb-2 text-xs font-medium text-muted-foreground">Member selector</p>
            <div className="flex flex-wrap gap-1">
              {Object.entries(detail.memberSelector.matchLabels ?? {}).map(([k, v]) => (
                <Badge key={`${k}=${v}`} variant="outline" className="font-mono text-[10px]">
                  {k}={v}
                </Badge>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* Status panel — resolved members */}
      <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="registry-members-panel">
        <p className="mb-3 text-sm font-medium">Members</p>
        {detail.status.members.length === 0 ? (
          <p className="text-sm text-muted-foreground">No members resolved yet.</p>
        ) : (
          <ul className="space-y-1.5">
            {detail.status.members.map((m) => (
              <li key={m} className="flex items-center gap-2 text-sm">
                {/* U4/Theme 1: a member is an AgentDeployment in this registry's
                    namespace — link to its detail, never dead-end text. */}
                <ResourceLink
                  kind="agent"
                  namespace={detail.namespace}
                  name={m}
                  className="font-mono text-xs"
                  testId={`member-link-${m}`}
                />
              </li>
            ))}
          </ul>
        )}
      </div>

      {/* Edit wizard */}
      <DetailDrawer
        open={editOpen}
        onClose={onCloseEdit}
        title={`Edit ${detail.name}`}
        subtitle={`Registry ID: ${detail.registryId} (read-only — immutable)`}
        size="lg"
      >
        {editOpen && (
          <RegistryEditWizard
            detail={detail}
            onClose={onCloseEdit}
            onSaved={onSaved}
          />
        )}
      </DetailDrawer>

      {/* Delete dialog */}
      {deleteOpen && (
        <RegistryDeleteDialog
          detail={detail}
          onClose={onCloseDelete}
          onDeleted={onDeleted}
        />
      )}
    </div>
  );
}

// ── Edit Wizard ───────────────────────────────────────────────────────────────
// registryId shown as read-only (immutable). NO egress/allowlist field.

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

// MatchLabels editor: key=value pairs
function MatchLabelsEditor({
  labels,
  onChange,
}: {
  labels: Record<string, string>;
  onChange: (l: Record<string, string>) => void;
}) {
  const entries = Object.entries(labels);

  function addEntry() {
    onChange({ ...labels, "": "" });
  }

  function removeEntry(key: string) {
    const next = { ...labels };
    delete next[key];
    onChange(next);
  }

  function updateKey(oldKey: string, newKey: string) {
    const val = labels[oldKey] ?? "";
    const next: Record<string, string> = {};
    for (const [k, v] of Object.entries(labels)) {
      next[k === oldKey ? newKey : k] = v;
    }
    onChange({ ...next, [newKey]: val });
  }

  function updateVal(key: string, val: string) {
    onChange({ ...labels, [key]: val });
  }

  return (
    <div className="space-y-2">
      {entries.map(([k, v], i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            value={k}
            onChange={(e) => updateKey(k, e.target.value)}
            placeholder="key"
            className="font-mono text-xs"
            data-testid={`label-key-${i}`}
          />
          <span className="text-muted-foreground">=</span>
          <Input
            value={v}
            onChange={(e) => updateVal(k, e.target.value)}
            placeholder="value"
            className="font-mono text-xs"
            data-testid={`label-val-${i}`}
          />
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7 shrink-0"
            aria-label={`Remove label ${k}`}
            data-testid={`remove-label-${i}`}
            onClick={() => removeEntry(k)}
          >
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={addEntry}
        data-testid="add-label-button"
      >
        <Plus className="h-4 w-4" />
        Add label
      </Button>
    </div>
  );
}

function RegistryEditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: AgentRegistryDetail;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  const [matchLabels, setMatchLabels] = React.useState<Record<string, string>>(
    detail.memberSelector.matchLabels ?? {},
  );
  const [rolesText, setRolesText] = React.useState(detail.roles.join(", "));
  const [maxDepth, setMaxDepth] = React.useState(
    detail.guards?.maxDepth !== undefined ? String(detail.guards.maxDepth) : "",
  );
  const [hopBudget, setHopBudget] = React.useState(
    detail.guards?.hopBudget !== undefined ? String(detail.guards.hopBudget) : "",
  );
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      const memberSelector: LabelSelectorDTO = {};
      if (Object.keys(matchLabels).length > 0) {
        memberSelector.matchLabels = matchLabels;
      }
      const roles = rolesText
        .split(",")
        .map((r) => r.trim())
        .filter(Boolean);
      const guards: RegistryGuardsDTO | undefined =
        maxDepth.trim() || hopBudget.trim()
          ? {
              ...(maxDepth.trim() ? { maxDepth: parseInt(maxDepth, 10) } : {}),
              ...(hopBudget.trim() ? { hopBudget: parseInt(hopBudget, 10) } : {}),
            }
          : undefined;

      await api.updateAgentRegistry(detail.namespace, detail.name, {
        name: detail.name,
        memberSelector,
        guards,
        roles: roles.length > 0 ? roles : undefined,
      });
      toast({ variant: "success", title: "Registry updated", description: `${detail.name} saved.` });
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

  const memberStep: WizardStep = {
    id: "members",
    title: "Member selector",
    description: "Labels that select AgentDeployments into this registry",
    content: (
      <div className="space-y-4">
        {/* registryId is read-only on edit — immutable after creation */}
        <div
          className="rounded-md border border-border bg-surface-2/40 px-3 py-2 text-sm"
          data-testid="registry-id-readonly"
        >
          <span className="text-muted-foreground">Registry ID (immutable): </span>
          <span className="font-mono text-xs">{detail.registryId}</span>
        </div>
        <FormField id="member-selector" label="Match labels">
          <MatchLabelsEditor labels={matchLabels} onChange={setMatchLabels} />
        </FormField>
      </div>
    ),
  };

  const rolesStep: WizardStep = {
    id: "roles",
    title: "Roles & guards",
    description: "Custom role names and conversation guards",
    content: (
      <div className="space-y-4">
        <FormField id="roles-input" label="Roles (comma-separated)">
          <Input
            id="roles-input"
            value={rolesText}
            onChange={(e) => setRolesText(e.target.value)}
            placeholder="planner, executor, reviewer"
            data-testid="edit-roles"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="max-depth" label="Max depth">
            <Input
              id="max-depth"
              inputMode="numeric"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
              placeholder="10"
              data-testid="edit-max-depth"
            />
          </FormField>
          <FormField id="hop-budget" label="Hop budget">
            <Input
              id="hop-budget"
              inputMode="numeric"
              value={hopBudget}
              onChange={(e) => setHopBudget(e.target.value)}
              placeholder="50"
              data-testid="edit-hop-budget"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="registry-edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this registry"
            description="Your account can't update AgentRegistries in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="registry-edit-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Registry ID</dt>
            {/* read-only — always shows the live value */}
            <dd className="font-mono text-xs" data-testid="review-registry-id">{detail.registryId}</dd>
          </div>
          {Object.keys(matchLabels).length > 0 && (
            <div className="px-3 py-2">
              <dt className="text-muted-foreground mb-1">Match labels</dt>
              <dd className="flex flex-wrap gap-1">
                {Object.entries(matchLabels).map(([k, v]) => (
                  <Badge key={`${k}=${v}`} variant="outline" className="font-mono text-[10px]">
                    {k}={v}
                  </Badge>
                ))}
              </dd>
            </div>
          )}
          {rolesText.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Roles</dt>
              <dd>{rolesText}</dd>
            </div>
          )}
          {maxDepth.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Max depth</dt>
              <dd>{maxDepth}</dd>
            </div>
          )}
          {hopBudget.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Hop budget</dt>
              <dd>{hopBudget}</dd>
            </div>
          )}
        </dl>
      </div>
    ),
  };

  return (
    <Wizard
      steps={[memberStep, rolesStep, reviewStep]}
      current={current}
      onStepChange={setCurrent}
      busy={saveState.kind === "saving"}
      onFinish={() => { void doSave(); }}
      finishLabel="Save changes"
      onCancel={onClose}
    />
  );
}

// ── Delete Dialog ─────────────────────────────────────────────────────────────

function RegistryDeleteDialog({
  detail,
  onClose,
  onDeleted,
}: {
  detail: AgentRegistryDetail;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const [deleting, setDeleting] = React.useState(false);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.removeAgentRegistry(detail.namespace, detail.name);
      toast({ variant: "success", title: "Registry deleted", description: `${detail.name} removed.` });
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

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${detail.name}?`}
      description={`This will permanently delete the AgentRegistry "${detail.name}". Member agents are not deleted but lose their registry membership.`}
      confirmText={detail.name}
      confirmLabel="Delete registry"
      busy={deleting}
    />
  );
}

// ── New AgentRegistry page (create) ──────────────────────────────────────────
// registryId is set at creation (immutable thereafter).
// NO egress/allowlist field.

export function NewAgentRegistryPage() {
  const navigate = useNavigate();
  const [regName, setRegName] = React.useState("");
  const [regNs, setRegNs] = React.useState("");
  const [registryId, setRegistryId] = React.useState("");
  const [matchLabels, setMatchLabels] = React.useState<Record<string, string>>({});
  const [rolesText, setRolesText] = React.useState("");
  const [maxDepth, setMaxDepth] = React.useState("");
  const [hopBudget, setHopBudget] = React.useState("");
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  async function doCreate() {
    setSaveState({ kind: "saving" });
    try {
      const memberSelector: LabelSelectorDTO = {};
      if (Object.keys(matchLabels).length > 0) {
        memberSelector.matchLabels = matchLabels;
      }
      const roles = rolesText
        .split(",")
        .map((r) => r.trim())
        .filter(Boolean);
      const guards: RegistryGuardsDTO | undefined =
        maxDepth.trim() || hopBudget.trim()
          ? {
              ...(maxDepth.trim() ? { maxDepth: parseInt(maxDepth, 10) } : {}),
              ...(hopBudget.trim() ? { hopBudget: parseInt(hopBudget, 10) } : {}),
            }
          : undefined;

      const created = await api.createAgentRegistry({
        name: regName.trim(),
        namespace: regNs.trim() || undefined,
        registryId: registryId.trim(),
        memberSelector,
        guards,
        roles: roles.length > 0 ? roles : undefined,
      });
      toast({ variant: "success", title: "Registry created", description: `${created.name} created.` });
      navigate(`/registries/${encodeURIComponent(created.namespace)}/${encodeURIComponent(created.name)}`);
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSaveState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  const identityStep: WizardStep = {
    id: "identity",
    title: "Identity",
    description: "Name, namespace, and registry ID",
    content: (
      <div className="space-y-4">
        <FormField id="new-reg-name" label="Name">
          <Input
            id="new-reg-name"
            value={regName}
            onChange={(e) => setRegName(e.target.value)}
            placeholder="my-registry"
            data-testid="new-registry-name"
          />
        </FormField>
        <FormField id="new-reg-ns" label="Namespace (blank = default)">
          <Input
            id="new-reg-ns"
            value={regNs}
            onChange={(e) => setRegNs(e.target.value)}
            placeholder="default"
            data-testid="new-registry-namespace"
          />
        </FormField>
        <FormField id="new-registry-id" label="Registry ID (immutable after create)">
          <Input
            id="new-registry-id"
            value={registryId}
            onChange={(e) => setRegistryId(e.target.value)}
            placeholder="prod-registry"
            data-testid="new-registry-id"
          />
        </FormField>
      </div>
    ),
  };

  const memberStep: WizardStep = {
    id: "members",
    title: "Member selector",
    description: "Labels selecting AgentDeployments into this registry",
    content: (
      <FormField id="new-member-selector" label="Match labels">
        <MatchLabelsEditor labels={matchLabels} onChange={setMatchLabels} />
      </FormField>
    ),
  };

  const rolesStep: WizardStep = {
    id: "roles",
    title: "Roles & guards",
    description: "Custom role names and conversation guards",
    content: (
      <div className="space-y-4">
        <FormField id="new-roles-input" label="Roles (comma-separated)">
          <Input
            id="new-roles-input"
            value={rolesText}
            onChange={(e) => setRolesText(e.target.value)}
            placeholder="planner, executor"
            data-testid="new-registry-roles"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="new-max-depth" label="Max depth">
            <Input
              id="new-max-depth"
              inputMode="numeric"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
              placeholder="10"
              data-testid="new-registry-max-depth"
            />
          </FormField>
          <FormField id="new-hop-budget" label="Hop budget">
            <Input
              id="new-hop-budget"
              inputMode="numeric"
              value={hopBudget}
              onChange={(e) => setHopBudget(e.target.value)}
              placeholder="50"
              data-testid="new-registry-hop-budget"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="new-registry-review">
        <p className="text-sm font-medium">Review new AgentRegistry</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create registries"
            description="Your account can't create AgentRegistries in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="new-registry-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Name</dt>
            <dd>{regName || "—"}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Registry ID</dt>
            <dd className="font-mono text-xs" data-testid="review-new-registry-id">{registryId || "—"}</dd>
          </div>
          {rolesText.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Roles</dt>
              <dd>{rolesText}</dd>
            </div>
          )}
        </dl>
      </div>
    ),
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">New Agent Registry</h2>
        <p className="text-sm text-muted-foreground">
          Group AgentDeployments into a named registry with roles and a member
          selector. The registryId is immutable after creation.
        </p>
      </div>
      <Wizard
        steps={[identityStep, memberStep, rolesStep, reviewStep]}
        current={current}
        onStepChange={setCurrent}
        busy={saveState.kind === "saving"}
        onFinish={() => { void doCreate(); }}
        finishLabel="Create registry"
        onCancel={() => navigate("/registries")}
        dirty={regName.trim() !== ""}
      />
    </div>
  );
}
