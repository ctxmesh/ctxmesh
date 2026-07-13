import * as React from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { UsedBySection } from "@/components/used-by-section";
import { GitBranch, Pencil, Plus, Trash2, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  ComboSelect,
  ConfirmDialog,
  DetailDrawer,
  EmptyState,
  ForbiddenInline,
  Wizard,
  type WizardStep,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import {
  api,
  ApiError,
  type ModelRouteDetail,
  type ModelRouteProviderDTO,
  type ProviderSummary,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_ROUTES } from "@/lib/nav";

// ModelRouteDetailPage — detail + edit wizard + typed-name delete for one
// ModelRoute. The edit wizard lets the caller manage the providers list (including
// apiBase for non-mock providers) and the optional rate limit. A 422 from the BFF
// (CRD validation: secretBindingRef/apiBase rule) surfaces in the form, never
// swallowed. RBAC-aware: viewers see no edit/delete affordances.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: ModelRouteDetail }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function ModelRouteDetailPage() {
  const { ns = "", name = "" } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [state, setState] = React.useState<Load>({ kind: "loading" });
  const { can } = useCapabilities();

  const editOpen = searchParams.get("edit") === "1";
  const deleteOpen = searchParams.get("delete") === "1";

  function openEdit() {
    setSearchParams((p) => {
      p.set("edit", "1");
      return p;
    });
  }
  function closeEdit() {
    setSearchParams((p) => {
      p.delete("edit");
      return p;
    });
  }
  function openDelete() {
    setSearchParams((p) => {
      p.set("delete", "1");
      return p;
    });
  }
  function closeDelete() {
    setSearchParams((p) => {
      p.delete("delete");
      return p;
    });
  }

  const load = React.useCallback(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .modelRouteDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message:
            err instanceof Error
              ? err.message
              : "couldn't load the model route",
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
        <p
          className="text-sm text-muted-foreground"
          data-testid="route-detail-loading"
        >
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
            description="Your account can't read this model route in this namespace."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="route-not-found">
          <EmptyState
            icon={GitBranch}
            title="Model route not found"
            description={`No ModelRoute "${name}" in ${ns || "this namespace"}.`}
            action={{
              label: "Back to routes",
              onClick: () => navigate("/routes"),
            }}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="route-detail-error"
        >
          Couldn't load the model route: {state.message}
        </div>
      </div>
    );
  }

  const detail = state.detail;

  return (
    <RouteDetailContent
      detail={detail}
      editOpen={editOpen}
      deleteOpen={deleteOpen}
      onOpenEdit={openEdit}
      onCloseEdit={closeEdit}
      onOpenDelete={openDelete}
      onCloseDelete={closeDelete}
      onDeleted={() => navigate("/routes")}
      onSaved={() => {
        closeEdit();
        load();
      }}
      can={can}
    />
  );
}

// Separate component so hooks are always called (not conditional on state).
function RouteDetailContent({
  detail,
  editOpen,
  deleteOpen,
  onOpenEdit,
  onCloseEdit,
  onOpenDelete,
  onCloseDelete,
  onDeleted,
  onSaved,
  can,
}: {
  detail: ModelRouteDetail;
  editOpen: boolean;
  deleteOpen: boolean;
  onOpenEdit: () => void;
  onCloseEdit: () => void;
  onOpenDelete: () => void;
  onCloseDelete: () => void;
  onDeleted: () => void;
  onSaved: () => void;
  can: (res: string, verb: string) => boolean;
}) {
  const canEdit = can(RES_ROUTES, "update");
  const canDelete = can(RES_ROUTES, "delete");

  return (
    <div
      className="mx-auto max-w-5xl space-y-6"
      data-testid="route-detail-page"
    >
      {/* Header */}
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold tracking-tight">
            {detail.name}
          </h2>
          <Badge variant={detail.ready ? "success" : "warning"}>
            {detail.phase || (detail.ready ? "Ready" : "Pending")}
          </Badge>
          <span className="text-sm text-muted-foreground">
            {detail.namespace}
          </span>
          <div className="ml-auto flex items-center gap-2">
            {canEdit && (
              <Button
                variant="outline"
                size="sm"
                onClick={onOpenEdit}
                data-testid="edit-route-button"
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
                data-testid="delete-route-button"
              >
                <Trash2 className="h-4 w-4" />
                Delete
              </Button>
            )}
          </div>
        </div>
      </div>

      {/* Detail panel */}
      <div className="rounded-lg border bg-card p-5 shadow-card">
        <p className="mb-3 text-sm font-medium">Providers</p>
        <div className="space-y-3">
          {detail.providers.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No providers configured.
            </p>
          ) : (
            detail.providers.map((p, i) => (
              <div
                key={i}
                className="flex flex-wrap items-start gap-x-8 gap-y-1 rounded-md border bg-surface-2/40 p-3 text-sm"
                data-testid={`provider-row-${i}`}
              >
                <KV k="Provider" v={p.provider} />
                <KV
                  k="Model"
                  v={<span className="font-mono text-xs">{p.model}</span>}
                />
                <KV k="Priority" v={String(p.priority)} />
                {p.secretBindingRef && (
                  <KV k="SecretBinding" v={p.secretBindingRef} />
                )}
                {p.apiBase && (
                  <KV
                    k="API base"
                    v={<span className="font-mono text-xs">{p.apiBase}</span>}
                  />
                )}
              </div>
            ))
          )}
        </div>
        {detail.rateLimit && (
          <div className="mt-4 flex items-center gap-2 text-sm">
            <span className="text-muted-foreground">Rate limit:</span>
            <span>{detail.rateLimit.tenantRPM} RPM per tenant</span>
          </div>
        )}
      </div>

      {/* Reverse-lookup: the agents that route through this ModelRoute (m18.9). */}
      <UsedBySection
        kind="modelroute"
        name={detail.name}
        namespace={detail.namespace}
        title="Used by agents"
      />

      {/* Edit wizard */}
      <DetailDrawer
        open={editOpen}
        onClose={onCloseEdit}
        title={`Edit ${detail.name}`}
        size="lg"
      >
        {editOpen && (
          <RouteEditWizard
            detail={detail}
            onClose={onCloseEdit}
            onSaved={onSaved}
          />
        )}
      </DetailDrawer>

      {/* Delete dialog */}
      {deleteOpen && (
        <RouteDeleteDialog
          detail={detail}
          onClose={onCloseDelete}
          onDeleted={onDeleted}
        />
      )}
    </div>
  );
}

function KV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <dt className="shrink-0 text-muted-foreground">{k}</dt>
      <dd className="min-w-0 truncate">{v}</dd>
    </div>
  );
}

// ── Edit Wizard ───────────────────────────────────────────────────────────────

type ProviderForm = {
  provider: string;
  model: string;
  priority: string;
  secretBindingRef: string;
  apiBase: string;
};

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

function emptyProvider(): ProviderForm {
  return {
    provider: "",
    model: "",
    priority: "1",
    secretBindingRef: "",
    apiBase: "",
  };
}

function providerToForm(p: ModelRouteProviderDTO): ProviderForm {
  return {
    provider: p.provider,
    model: p.model,
    priority: String(p.priority),
    secretBindingRef: p.secretBindingRef ?? "",
    apiBase: p.apiBase ?? "",
  };
}

// useConnectedModels loads the connected providers so the route form can offer a
// DROPDOWN of each provider's known models (via a <datalist>) instead of free-text —
// the user picks "claude-sonnet-5" rather than typing a model id. It degrades to
// free-text: a failed load (or a custom/unlisted model) still types through. Shared
// by the edit wizard and the new-route form so both get the dropdown.
function useConnectedModels(): {
  providerNames: string[];
  modelsForProvider: (provider: string) => string[];
  secretBindingNames: string[];
} {
  const [connected, setConnected] = React.useState<ProviderSummary[]>([]);
  const [bindingNames, setBindingNames] = React.useState<string[]>([]);
  React.useEffect(() => {
    const ctrl = new AbortController();
    api
      .listProviders(ctrl.signal)
      .then((r) => setConnected(r.items ?? []))
      .catch(() => {
        // No providers / no permission → the fields stay free-text (no dropdown).
      });
    // The SecretBindings the caller can see → a dropdown for secretBindingRef
    // instead of free-text (M22/U2). Degrades to Custom… on failure.
    api
      .listSecretBindings({ limit: 100 }, ctrl.signal)
      .then((r) => setBindingNames((r.items ?? []).map((b) => b.name)))
      .catch(() => {
        /* no bindings / no permission → secretBindingRef stays type-able. */
      });
    return () => ctrl.abort();
  }, []);

  const modelsByProvider = React.useMemo(() => {
    const m = new Map<string, string[]>();
    for (const p of connected) {
      const merged = new Set([...(m.get(p.provider) ?? []), ...p.models]);
      m.set(p.provider, [...merged]);
    }
    return m;
  }, [connected]);

  return {
    providerNames: [...modelsByProvider.keys()],
    modelsForProvider: (provider: string) =>
      modelsByProvider.get(provider.trim()) ?? [],
    secretBindingNames: [...new Set(bindingNames)],
  };
}


function RouteEditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: ModelRouteDetail;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const { providerNames, modelsForProvider, secretBindingNames } =
    useConnectedModels();
  const [providers, setProviders] = React.useState<ProviderForm[]>(
    detail.providers.length > 0
      ? detail.providers.map(providerToForm)
      : [emptyProvider()],
  );
  const [rateLimitRpm, setRateLimitRpm] = React.useState(
    detail.rateLimit ? String(detail.rateLimit.tenantRPM) : "",
  );
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });

  function setProvider(i: number, field: keyof ProviderForm, val: string) {
    setProviders((prev) =>
      prev.map((p, idx) => (idx === i ? { ...p, [field]: val } : p)),
    );
  }

  function addProvider() {
    setProviders((prev) => [...prev, emptyProvider()]);
  }

  function removeProvider(i: number) {
    setProviders((prev) => prev.filter((_, idx) => idx !== i));
  }

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      const providerDTOs: ModelRouteProviderDTO[] = providers.map((p) => ({
        provider: p.provider.trim(),
        model: p.model.trim(),
        priority: parseInt(p.priority, 10) || 1,
        ...(p.secretBindingRef.trim()
          ? { secretBindingRef: p.secretBindingRef.trim() }
          : {}),
        ...(p.apiBase.trim() ? { apiBase: p.apiBase.trim() } : {}),
      }));
      const rpmNum = rateLimitRpm.trim()
        ? parseInt(rateLimitRpm, 10)
        : undefined;
      await api.updateModelRoute(detail.namespace, detail.name, {
        name: detail.name,
        providers: providerDTOs,
        ...(rpmNum && rpmNum > 0 ? { rateLimit: { tenantRPM: rpmNum } } : {}),
      });
      toast({
        variant: "success",
        title: "Model route updated",
        description: `${detail.name} saved.`,
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

  const providersStep: WizardStep = {
    id: "providers",
    title: "Providers",
    description: "Ordered list of provider/model entries",
    content: (
      <div className="space-y-4">
        {providers.map((p, i) => (
          <div
            key={i}
            className="space-y-3 rounded-md border p-3"
            data-testid={`provider-entry-${i}`}
          >
            <div className="flex items-center justify-between">
              <span className="text-xs font-medium text-muted-foreground">
                Provider {i + 1}
              </span>
              {providers.length > 1 && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-6 w-6 text-muted-foreground"
                  aria-label={`Remove provider ${i + 1}`}
                  data-testid={`remove-provider-${i}`}
                  onClick={() => removeProvider(i)}
                >
                  <X className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
            <div className="grid grid-cols-2 gap-3">
              <FormField id={`provider-${i}`} label="Provider">
                {/* Real dropdown of connected providers (+ Custom… for unknown /
                    mock providers) — not a datalist that filters to the typed
                    value (M22/U2). */}
                <ComboSelect
                  id={`provider-${i}`}
                  value={p.provider}
                  options={providerNames}
                  onChange={(v) => setProvider(i, "provider", v)}
                  placeholder="— provider —"
                  customPlaceholder="anthropic"
                  testId={`provider-name-${i}`}
                />
              </FormField>
              <FormField id={`model-${i}`} label="Model">
                <ComboSelect
                  id={`model-${i}`}
                  value={p.model}
                  options={modelsForProvider(p.provider)}
                  onChange={(v) => setProvider(i, "model", v)}
                  placeholder="— model —"
                  customPlaceholder="claude-sonnet-5"
                  testId={`provider-model-${i}`}
                />
              </FormField>
            </div>
            <FormField id={`priority-${i}`} label="Priority (≥1)">
              <Input
                id={`priority-${i}`}
                inputMode="numeric"
                value={p.priority}
                onChange={(e) => setProvider(i, "priority", e.target.value)}
                data-testid={`provider-priority-${i}`}
              />
            </FormField>
            <FormField id={`secret-${i}`} label="Secret binding ref">
              <ComboSelect
                id={`secret-${i}`}
                value={p.secretBindingRef}
                options={secretBindingNames}
                onChange={(v) => setProvider(i, "secretBindingRef", v)}
                placeholder="— secret binding —"
                customPlaceholder="my-openai-binding"
                testId={`provider-secretbinding-${i}`}
              />
            </FormField>
            <FormField id={`apibase-${i}`} label="API base (optional override)">
              <Input
                id={`apibase-${i}`}
                value={p.apiBase}
                onChange={(e) => setProvider(i, "apiBase", e.target.value)}
                placeholder="https://api.openai.com/v1"
                data-testid={`provider-apibase-${i}`}
              />
            </FormField>
          </div>
        ))}
        <Button
          variant="outline"
          size="sm"
          onClick={addProvider}
          data-testid="add-provider-button"
        >
          <Plus className="h-4 w-4" />
          Add provider
        </Button>
      </div>
    ),
  };

  const rateLimitStep: WizardStep = {
    id: "rate-limit",
    title: "Rate limit",
    description: "Optional per-tenant rate cap",
    content: (
      <div className="space-y-4">
        <FormField
          id="rate-limit-rpm"
          label="Tenant RPM (leave blank to clear)"
        >
          <Input
            id="rate-limit-rpm"
            inputMode="numeric"
            value={rateLimitRpm}
            onChange={(e) => setRateLimitRpm(e.target.value)}
            placeholder="60"
            data-testid="rate-limit-rpm"
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
      <div className="space-y-4" data-testid="route-edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this route"
            description="Your account can't update ModelRoutes in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p
            className="text-sm text-destructive"
            role="alert"
            data-testid="route-edit-error"
          >
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          {providers.map((p, i) => (
            <div key={i} className="px-3 py-2 text-xs">
              <span className="font-medium">
                {p.provider}/{p.model}
              </span>{" "}
              <span className="text-muted-foreground">
                priority {p.priority}
              </span>
              {p.secretBindingRef && (
                <span className="ml-2 text-muted-foreground">
                  → {p.secretBindingRef}
                </span>
              )}
              {p.apiBase && (
                <span className="ml-2 font-mono text-muted-foreground">
                  {p.apiBase}
                </span>
              )}
            </div>
          ))}
          {rateLimitRpm && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Rate limit</dt>
              <dd>{rateLimitRpm} RPM</dd>
            </div>
          )}
        </dl>
      </div>
    ),
  };

  return (
    <Wizard
      steps={[providersStep, rateLimitStep, reviewStep]}
      current={current}
      onStepChange={setCurrent}
      busy={saveState.kind === "saving"}
      onFinish={() => {
        void doSave();
      }}
      finishLabel="Save changes"
      onCancel={onClose}
    />
  );
}

// ── Delete Dialog ─────────────────────────────────────────────────────────────

function RouteDeleteDialog({
  detail,
  onClose,
  onDeleted,
}: {
  detail: ModelRouteDetail;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const [deleting, setDeleting] = React.useState(false);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.removeModelRoute(detail.namespace, detail.name);
      toast({
        variant: "success",
        title: "Model route deleted",
        description: `${detail.name} removed.`,
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

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${detail.name}?`}
      description={`This will permanently delete the ModelRoute "${detail.name}" and any traffic routing it provided.`}
      confirmText={detail.name}
      confirmLabel="Delete route"
      busy={deleting}
    />
  );
}

// ── New route page (create) ───────────────────────────────────────────────────

export function NewModelRoutePage() {
  const navigate = useNavigate();
  const [providers, setProviders] = React.useState<ProviderForm[]>([
    emptyProvider(),
  ]);
  const [routeName, setRouteName] = React.useState("");
  const [routeNs, setRouteNs] = React.useState("");
  const [rateLimitRpm, setRateLimitRpm] = React.useState("");
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const { providerNames, modelsForProvider, secretBindingNames } =
    useConnectedModels();

  function setProvider(i: number, field: keyof ProviderForm, val: string) {
    setProviders((prev) =>
      prev.map((p, idx) => (idx === i ? { ...p, [field]: val } : p)),
    );
  }

  function addProvider() {
    setProviders((prev) => [...prev, emptyProvider()]);
  }

  function removeProvider(i: number) {
    setProviders((prev) => prev.filter((_, idx) => idx !== i));
  }

  async function doCreate() {
    setSaveState({ kind: "saving" });
    try {
      const providerDTOs: ModelRouteProviderDTO[] = providers.map((p) => ({
        provider: p.provider.trim(),
        model: p.model.trim(),
        priority: parseInt(p.priority, 10) || 1,
        ...(p.secretBindingRef.trim()
          ? { secretBindingRef: p.secretBindingRef.trim() }
          : {}),
        ...(p.apiBase.trim() ? { apiBase: p.apiBase.trim() } : {}),
      }));
      const rpmNum = rateLimitRpm.trim()
        ? parseInt(rateLimitRpm, 10)
        : undefined;
      const created = await api.createModelRoute({
        name: routeName.trim(),
        namespace: routeNs.trim() || undefined,
        providers: providerDTOs,
        ...(rpmNum && rpmNum > 0 ? { rateLimit: { tenantRPM: rpmNum } } : {}),
      });
      toast({
        variant: "success",
        title: "Model route created",
        description: `${created.name} created.`,
      });
      navigate(
        `/routes/${encodeURIComponent(created.namespace)}/${encodeURIComponent(created.name)}`,
      );
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
    description: "Name and namespace for the new route",
    content: (
      <div className="space-y-4">
        <FormField id="route-name" label="Name">
          <Input
            id="route-name"
            value={routeName}
            onChange={(e) => setRouteName(e.target.value)}
            placeholder="my-model-route"
            data-testid="new-route-name"
          />
        </FormField>
        <FormField id="route-ns" label="Namespace (blank = default)">
          <Input
            id="route-ns"
            value={routeNs}
            onChange={(e) => setRouteNs(e.target.value)}
            placeholder="default"
            data-testid="new-route-namespace"
          />
        </FormField>
      </div>
    ),
  };

  const providersStep: WizardStep = {
    id: "providers",
    title: "Providers",
    description: "Ordered list of provider/model entries",
    content: (
      <div className="space-y-4">
        {providers.map((p, i) => (
          <div
            key={i}
            className="space-y-3 rounded-md border p-3"
            data-testid={`new-provider-entry-${i}`}
          >
            <div className="flex items-center justify-between">
              <span className="text-xs font-medium text-muted-foreground">
                Provider {i + 1}
              </span>
              {providers.length > 1 && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-6 w-6"
                  aria-label={`Remove provider ${i + 1}`}
                  data-testid={`new-remove-provider-${i}`}
                  onClick={() => removeProvider(i)}
                >
                  <X className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
            <div className="grid grid-cols-2 gap-3">
              <FormField id={`new-provider-${i}`} label="Provider">
                <ComboSelect
                  id={`new-provider-${i}`}
                  value={p.provider}
                  options={providerNames}
                  onChange={(v) => setProvider(i, "provider", v)}
                  placeholder="— provider —"
                  customPlaceholder="anthropic"
                  testId={`new-provider-name-${i}`}
                />
              </FormField>
              <FormField id={`new-model-${i}`} label="Model">
                <ComboSelect
                  id={`new-model-${i}`}
                  value={p.model}
                  options={modelsForProvider(p.provider)}
                  onChange={(v) => setProvider(i, "model", v)}
                  placeholder="— model —"
                  customPlaceholder="claude-sonnet-5"
                  testId={`new-provider-model-${i}`}
                />
              </FormField>
            </div>
            <FormField id={`new-priority-${i}`} label="Priority (≥1)">
              <Input
                id={`new-priority-${i}`}
                inputMode="numeric"
                value={p.priority}
                onChange={(e) => setProvider(i, "priority", e.target.value)}
                data-testid={`new-provider-priority-${i}`}
              />
            </FormField>
            <FormField id={`new-secret-${i}`} label="Secret binding ref">
              <ComboSelect
                id={`new-secret-${i}`}
                value={p.secretBindingRef}
                options={secretBindingNames}
                onChange={(v) => setProvider(i, "secretBindingRef", v)}
                placeholder="— secret binding —"
                customPlaceholder="my-openai-binding"
                testId={`new-provider-secretbinding-${i}`}
              />
            </FormField>
            <FormField id={`new-apibase-${i}`} label="API base">
              <Input
                id={`new-apibase-${i}`}
                value={p.apiBase}
                onChange={(e) => setProvider(i, "apiBase", e.target.value)}
                placeholder="https://api.openai.com/v1"
                data-testid={`new-provider-apibase-${i}`}
              />
            </FormField>
          </div>
        ))}
        <Button
          variant="outline"
          size="sm"
          onClick={addProvider}
          data-testid="new-add-provider"
        >
          <Plus className="h-4 w-4" />
          Add provider
        </Button>
      </div>
    ),
  };

  const rateLimitStep: WizardStep = {
    id: "rate-limit",
    title: "Rate limit",
    description: "Optional per-tenant rate cap",
    content: (
      <FormField id="new-rate-limit-rpm" label="Tenant RPM (optional)">
        <Input
          id="new-rate-limit-rpm"
          inputMode="numeric"
          value={rateLimitRpm}
          onChange={(e) => setRateLimitRpm(e.target.value)}
          placeholder="60"
          data-testid="new-rate-limit-rpm"
        />
      </FormField>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="new-route-review">
        <p className="text-sm font-medium">Review new ModelRoute</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create routes"
            description="Your account can't create ModelRoutes in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p
            className="text-sm text-destructive"
            role="alert"
            data-testid="new-route-error"
          >
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Name</dt>
            <dd>{routeName || "—"}</dd>
          </div>
          {routeNs && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Namespace</dt>
              <dd>{routeNs}</dd>
            </div>
          )}
          {providers.map((p, i) => (
            <div key={i} className="px-3 py-2 text-xs">
              <span className="font-medium">
                {p.provider}/{p.model}
              </span>
              <span className="ml-2 text-muted-foreground">
                priority {p.priority}
              </span>
              {p.apiBase && (
                <span className="ml-2 font-mono text-muted-foreground">
                  {p.apiBase}
                </span>
              )}
            </div>
          ))}
          {rateLimitRpm && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Rate limit</dt>
              <dd>{rateLimitRpm} RPM</dd>
            </div>
          )}
        </dl>
      </div>
    ),
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">
          New Model Route
        </h2>
        <p className="text-sm text-muted-foreground">
          A ModelRoute routes AI calls to one or more providers in priority
          order.
        </p>
      </div>
      <Wizard
        steps={[identityStep, providersStep, rateLimitStep, reviewStep]}
        current={current}
        onStepChange={setCurrent}
        busy={saveState.kind === "saving"}
        onFinish={() => {
          void doCreate();
        }}
        finishLabel="Create route"
        onCancel={() => navigate("/routes")}
        dirty={routeName.trim() !== ""}
      />
    </div>
  );
}
