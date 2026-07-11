import * as React from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { UsedBySection } from "@/components/used-by-section";
import { KeyRound, Pencil, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  ConfirmDialog,
  DetailDrawer,
  EmptyState,
  ForbiddenInline,
  Wizard,
  type WizardStep,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { api, ApiError, type SecretBindingDetail } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_SECRETS } from "@/lib/nav";

// SecretBindingDetailPage — detail + edit wizard + typed-name delete.
//
// SECURITY INVARIANT: this page NEVER renders or requests a secret value.
// All fields concern only the REFERENCE (which K8s Secret, which key) and the
// status/phase derived from the controller. There is no value/data input here.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: SecretBindingDetail }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function SecretBindingDetailPage() {
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
      .secretBindingDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the secret binding",
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
        <p className="text-sm text-muted-foreground" data-testid="secret-detail-loading">
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
            description="Your account can't read this secret binding."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="secret-not-found">
          <EmptyState
            icon={KeyRound}
            title="Secret binding not found"
            description={`No SecretBinding "${name}" in ${ns || "this namespace"}.`}
            action={{ label: "Back to bindings", onClick: () => navigate("/secrets") }}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="secret-detail-error"
        >
          Couldn't load the secret binding: {state.message}
        </div>
      </div>
    );
  }

  const detail = state.detail;
  return (
    <SecretBindingDetailContent
      detail={detail}
      editOpen={editOpen}
      deleteOpen={deleteOpen}
      onOpenEdit={openEdit}
      onCloseEdit={closeEdit}
      onOpenDelete={openDelete}
      onCloseDelete={closeDelete}
      onDeleted={() => navigate("/secrets")}
      onSaved={() => { closeEdit(); load(); }}
    />
  );
}

function SecretBindingDetailContent({
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
  detail: SecretBindingDetail;
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
  const canEdit = can(RES_SECRETS, "update");
  const canDelete = can(RES_SECRETS, "delete");

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="secret-detail-page">
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold tracking-tight">{detail.name}</h2>
          <Badge variant={detail.ready ? "success" : "warning"}>
            {detail.phase || (detail.ready ? "Resolved" : "Pending")}
          </Badge>
          <span className="text-sm text-muted-foreground">{detail.namespace}</span>
          <div className="ml-auto flex items-center gap-2">
            {canEdit && (
              <Button
                variant="outline"
                size="sm"
                onClick={onOpenEdit}
                data-testid="edit-secret-button"
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
                data-testid="delete-secret-button"
              >
                <Trash2 className="h-4 w-4" />
                Delete
              </Button>
            )}
          </div>
        </div>
      </div>

      {/* Detail — shows only the ref and phase. NO value field. */}
      <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="secret-detail-panel">
        <p className="mb-3 text-sm font-medium">Secret reference</p>
        <dl className="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
          <dt className="text-muted-foreground">Backend</dt>
          <dd>{detail.backend}</dd>
          <dt className="text-muted-foreground">K8s Secret name</dt>
          {/* Shows the NAME of the Secret — not its contents */}
          <dd className="font-mono text-xs" data-testid="secret-ref-name">{detail.secretRef.name}</dd>
          <dt className="text-muted-foreground">Key</dt>
          {/* Shows the KEY within the Secret — not the value */}
          <dd className="font-mono text-xs" data-testid="secret-ref-key">{detail.secretRef.key}</dd>
          <dt className="text-muted-foreground">Phase</dt>
          <dd>
            <Badge variant={detail.ready ? "success" : "warning"}>
              {detail.phase || (detail.ready ? "Resolved" : "Pending")}
            </Badge>
          </dd>
        </dl>
        {/* Explicit note that we never render secret values */}
        <p
          className="mt-4 text-xs text-muted-foreground"
          data-testid="no-value-note"
        >
          The console never displays or transmits secret values — only the
          reference (which Secret, which key).
        </p>
      </div>

      {/* Reverse-lookup: the ModelRoutes whose providers reference this binding. */}
      <UsedBySection
        kind="secretbinding"
        name={detail.name}
        namespace={detail.namespace}
        title="Used by model routes"
      />

      {/* Edit wizard */}
      <DetailDrawer
        open={editOpen}
        onClose={onCloseEdit}
        title={`Edit ${detail.name}`}
        subtitle="Updates the Secret reference — never the credential value"
        size="lg"
      >
        {editOpen && (
          <SecretBindingEditWizard
            detail={detail}
            onClose={onCloseEdit}
            onSaved={onSaved}
          />
        )}
      </DetailDrawer>

      {/* Delete dialog */}
      {deleteOpen && (
        <SecretBindingDeleteDialog
          detail={detail}
          onClose={onCloseDelete}
          onDeleted={onDeleted}
        />
      )}
    </div>
  );
}

// ── Edit Wizard ───────────────────────────────────────────────────────────────
// SECURITY: the form edits ONLY secretRef.name + secretRef.key (and backend).
// There is NO value/credential field here.

type EditForm = {
  backend: string;
  secretRefName: string;
  secretRefKey: string;
};

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

function SecretBindingEditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: SecretBindingDetail;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  const [form, setForm] = React.useState<EditForm>({
    backend: detail.backend,
    secretRefName: detail.secretRef.name,
    secretRefKey: detail.secretRef.key,
  });
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });

  function set<K extends keyof EditForm>(k: K, v: EditForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      await api.updateSecretBinding(detail.namespace, detail.name, {
        name: detail.name,
        backend: form.backend.trim() || "kubernetes",
        // SECURITY: submitting only the ref — no value field.
        secretRef: {
          name: form.secretRefName.trim(),
          key: form.secretRefKey.trim(),
        },
      });
      toast({ variant: "success", title: "Secret binding updated", description: `${detail.name} saved.` });
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

  const refStep: WizardStep = {
    id: "ref",
    title: "Secret reference",
    description: "Which K8s Secret holds the credential (never the value itself)",
    content: (
      <div className="space-y-4">
        {/* Explicit security reminder in the form header */}
        <div
          className="rounded-md border border-border bg-surface-2/40 px-3 py-2 text-xs text-muted-foreground"
          data-testid="no-value-form-note"
        >
          This form edits the <strong>reference</strong> to a Kubernetes Secret
          (the Secret name and key within it). It does not accept, display, or
          store credential values. The credential lives only in the K8s Secret.
        </div>
        <FormField id="sb-backend" label="Backend">
          <Input
            id="sb-backend"
            value={form.backend}
            onChange={(e) => set("backend", e.target.value)}
            placeholder="kubernetes"
            data-testid="edit-sb-backend"
          />
        </FormField>
        <FormField id="sb-secret-name" label="K8s Secret name (not the value)">
          <Input
            id="sb-secret-name"
            value={form.secretRefName}
            onChange={(e) => set("secretRefName", e.target.value)}
            placeholder="my-api-key-secret"
            data-testid="edit-sb-secret-name"
          />
        </FormField>
        <FormField id="sb-secret-key" label="Key within the Secret (not the value)">
          <Input
            id="sb-secret-key"
            value={form.secretRefKey}
            onChange={(e) => set("secretRefKey", e.target.value)}
            placeholder="apiKey"
            data-testid="edit-sb-secret-key"
          />
        </FormField>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="secret-edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this binding"
            description="Your account can't update SecretBindings in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="secret-edit-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Backend</dt>
            <dd>{form.backend || "kubernetes"}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Secret name</dt>
            <dd className="font-mono text-xs">{form.secretRefName}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Key</dt>
            <dd className="font-mono text-xs">{form.secretRefKey}</dd>
          </div>
        </dl>
      </div>
    ),
  };

  return (
    <Wizard
      steps={[refStep, reviewStep]}
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

function SecretBindingDeleteDialog({
  detail,
  onClose,
  onDeleted,
}: {
  detail: SecretBindingDetail;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const [deleting, setDeleting] = React.useState(false);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.removeSecretBinding(detail.namespace, detail.name);
      toast({ variant: "success", title: "Secret binding deleted", description: `${detail.name} removed.` });
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
      description={`This will permanently delete the SecretBinding "${detail.name}". Any ModelRoute that references it will lose its credential link.`}
      confirmText={detail.name}
      confirmLabel="Delete binding"
      busy={deleting}
    />
  );
}

// ── New SecretBinding page (create) ──────────────────────────────────────────
// SECURITY: the form accepts ONLY a ref (Secret name + key). No value field.

export function NewSecretBindingPage() {
  const navigate = useNavigate();
  const [bindingName, setBindingName] = React.useState("");
  const [bindingNs, setBindingNs] = React.useState("");
  const [backend, setBackend] = React.useState("kubernetes");
  const [secretRefName, setSecretRefName] = React.useState("");
  const [secretRefKey, setSecretRefKey] = React.useState("");
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  async function doCreate() {
    setSaveState({ kind: "saving" });
    try {
      const created = await api.createSecretBinding({
        name: bindingName.trim(),
        namespace: bindingNs.trim() || undefined,
        backend: backend.trim() || "kubernetes",
        // SECURITY: submitting only the ref — no value field.
        secretRef: {
          name: secretRefName.trim(),
          key: secretRefKey.trim(),
        },
      });
      toast({ variant: "success", title: "Secret binding created", description: `${created.name} created.` });
      navigate(`/secrets/${encodeURIComponent(created.namespace)}/${encodeURIComponent(created.name)}`);
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
    description: "Name and namespace",
    content: (
      <div className="space-y-4">
        <FormField id="new-sb-name" label="Name">
          <Input
            id="new-sb-name"
            value={bindingName}
            onChange={(e) => setBindingName(e.target.value)}
            placeholder="my-openai-binding"
            data-testid="new-sb-name"
          />
        </FormField>
        <FormField id="new-sb-ns" label="Namespace (blank = default)">
          <Input
            id="new-sb-ns"
            value={bindingNs}
            onChange={(e) => setBindingNs(e.target.value)}
            placeholder="default"
            data-testid="new-sb-namespace"
          />
        </FormField>
      </div>
    ),
  };

  const refStep: WizardStep = {
    id: "ref",
    title: "Secret reference",
    description: "Which K8s Secret holds the credential (not the value itself)",
    content: (
      <div className="space-y-4">
        {/* Explicit security notice — no value field */}
        <div
          className="rounded-md border border-border bg-surface-2/40 px-3 py-2 text-xs text-muted-foreground"
          data-testid="no-value-create-note"
        >
          Provide the <strong>reference</strong> to an existing Kubernetes Secret
          (its name and the key within it). The console never accepts, displays,
          or stores credential values.
        </div>
        <FormField id="new-sb-backend" label="Backend">
          <Input
            id="new-sb-backend"
            value={backend}
            onChange={(e) => setBackend(e.target.value)}
            placeholder="kubernetes"
            data-testid="new-sb-backend"
          />
        </FormField>
        <FormField id="new-sb-secret-name" label="K8s Secret name">
          <Input
            id="new-sb-secret-name"
            value={secretRefName}
            onChange={(e) => setSecretRefName(e.target.value)}
            placeholder="my-api-key-secret"
            data-testid="new-sb-secret-name"
          />
        </FormField>
        <FormField id="new-sb-secret-key" label="Key within the Secret">
          <Input
            id="new-sb-secret-key"
            value={secretRefKey}
            onChange={(e) => setSecretRefKey(e.target.value)}
            placeholder="apiKey"
            data-testid="new-sb-secret-key"
          />
        </FormField>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="new-secret-review">
        <p className="text-sm font-medium">Review new SecretBinding</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create bindings"
            description="Your account can't create SecretBindings in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="new-secret-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Name</dt>
            <dd>{bindingName || "—"}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Backend</dt>
            <dd>{backend}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Secret name</dt>
            <dd className="font-mono text-xs">{secretRefName}</dd>
          </div>
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Key</dt>
            <dd className="font-mono text-xs">{secretRefKey}</dd>
          </div>
        </dl>
      </div>
    ),
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">New Secret Binding</h2>
        <p className="text-sm text-muted-foreground">
          Reference an existing Kubernetes Secret. The console never touches the
          credential value — only the ref (which Secret, which key).
        </p>
      </div>
      <Wizard
        steps={[identityStep, refStep, reviewStep]}
        current={current}
        onStepChange={setCurrent}
        busy={saveState.kind === "saving"}
        onFinish={() => { void doCreate(); }}
        finishLabel="Create binding"
        onCancel={() => navigate("/secrets")}
        dirty={bindingName.trim() !== ""}
      />
    </div>
  );
}
