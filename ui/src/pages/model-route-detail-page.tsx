import * as React from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { UsedBySection } from "@/components/used-by-section";
import { Pencil, Plus, Trash2, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
// One import for the whole module. The create wizard below briefly carried its
// own second `@/components/kit` statement so the two surfaces could be edited
// independently; TypeScript scopes imports per MODULE, so the moment both
// surfaces needed `PageHeader` that became a duplicate-identifier error. The
// components are still independently editable — the import list is not the
// seam that keeps them so.
import {
  ComboSelect,
  ConfirmDialog,
  DetailDrawer,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  Meter,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  ResourceLink,
  SectionHeader,
  Skeleton,
  SkeletonCard,
  StatusBadge,
  UNKNOWN,
  Wizard,
  formatCount,
  type KeyValueItem,
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

// ModelRouteDetailPage — the route reader + its edit form (M151 §6.1 archetype
// A2, form-edit variant). Route: /routes/:ns/:name
//
// ── THE PAGE'S ONE IDEA: A ROUTE IS AN ORDER, NOT A SET ─────────────────────
// A ModelRoute's providers are not a bag of options; they are a ranked list,
// and the only question a reader brings here is "what actually serves my
// traffic, and what happens when it can't?". So the main column renders them as
// an ORDERED spine — first choice, then what it falls back to — while the rail
// states the bound facts that order is made of. The old page rendered the same
// array as unordered chips with "Priority 2" buried mid-row: the same data,
// saying nothing.
//
// ── AN UNSAVED CHANGE IS VISIBLY UNSAVED (this variant's contract) ──────────
// The rail states what the route IS; the drawer edits it. Between the two there
// is a window where the form and the record disagree, and the form says so:
// every step carries the unsaved line, each diverged field carries its own
// "Changed — not saved yet" hint, the review renders was → now rather than a
// flat restatement, and Cancel/Esc route through the discard guard. Dirtiness is
// COMPUTED against the record rather than tracked as a flag, so a field edited
// and then edited back is correctly not a change.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ────────────────────────────────────
// `GET /api/modelroutes/{ns}/{name}` returns identity, the provider list, the
// optional rate limit and the phase. It returns NO request counts, NO per-
// provider health and NO spend. So the rate-cap Meter draws the bound it was
// given and states that usage against it is not recorded here — it does not
// draw a fill at zero, because zero would be a claim the platform never made.
// A 422 from the BFF (the CRD's secretBindingRef/apiBase rule) surfaces in the
// form, never swallowed. RBAC-aware: viewers see no edit/delete affordances.
//
// data-testid contract:
//   route-detail-page      — root container (ready state)
//   route-detail-loading   — the loading state
//   route-detail-error     — the generic error state
//   route-not-found        — the 404 state
//   provider-row-{i}       — one provider in the failover order
//   route-edit-review      — the edit wizard's review step
//   route-edit-error       — a non-403 save failure (422 included)
//   route-edit-unsaved     — the unsaved-changes line in the edit form

/** The A2 breadcrumb — the same trail in every state, so a failed load still
 *  offers the way back the ready page offers. */
const CRUMBS = (name: string) => [
  { label: "Model routes", to: "/routes" },
  { label: name },
];

/** How the failover order reads. Past the third entry the eyebrow is the rank. */
const RANK = ["First choice", "Falls back to", "Then to"];

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

  // The §7 A2 loading shape: the header band instantly, a SkeletonCard where the
  // panel will be, and kv-line bars in the rail — so nothing re-lays-out under
  // the reader when the answer arrives.
  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6" data-testid="route-detail-loading">
        <PageHeader
          breadcrumb={CRUMBS(name)}
          title={name || "Model route"}
          titleMono
          loading
        />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <SkeletonCard className="min-w-0" />
          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <div
                role="status"
                aria-busy="true"
                aria-label="Loading the route's record"
              >
                {[0, 1, 2, 3, 4, 5].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  if (state.kind === "error") {
    // §7 A2: a permission boundary replaces the page under the header — calm,
    // resource-named, and never the raw RBAC string (M100 UI99-403).
    if (state.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader breadcrumb={CRUMBS(name)} title={name} titleMono />
          <ForbiddenInline
            title={`You don't have permission to view ${name}.`}
            resource="model routes"
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="min-w-0 space-y-6" data-testid="route-not-found">
          <PageHeader
            breadcrumb={[{ label: "Model routes", to: "/routes" }]}
            title="Model route not found"
            lede="Nothing in this workspace answers to that name."
          />
          <QuietNote title="No model route with this name was found.">
            There is no ModelRoute{" "}
            <span className="font-mono text-xs">{name}</span> in{" "}
            <span className="font-mono text-xs">{ns || "this namespace"}</span>.
            It may have been deleted, or the link may name a route from another
            cluster. Nothing is missing from this page — there is simply no route
            to show.
            <span className="mt-3 block">
              <NextStepLink label="Back to routes" to="/routes" />
            </span>
          </QuietNote>
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="route-detail-error">
        <PageHeader breadcrumb={CRUMBS(name)} title={name} titleMono />
        <ErrorState
          title="The model route didn't load."
          description="Nothing has changed about the route itself — only this page failed to read it."
          detail={state.message}
          onRetry={() => load()}
        />
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

  // The order IS the fact, so it is established once, here, rather than trusted
  // to whatever order the array happened to arrive in. Ties keep their relative
  // position (a stable sort), so two providers at the same priority read in the
  // order the CR declares them.
  const ordered = [...detail.providers].sort((a, b) => a.priority - b.priority);
  const count = ordered.length;
  const first = ordered[0];

  // The rail: the bound facts, in the kv register. No table here — §4.7.
  const facts: KeyValueItem[] = [
    { key: "Route", value: detail.name, title: detail.name },
    { key: "Workspace", value: detail.namespace, absent: "not recorded" },
    {
      key: "Providers",
      value: <QuantityValue value={count} />,
      mono: false,
    },
    {
      key: "First choice",
      value: first ? (
        <span
          className="block truncate"
          title={`${first.provider} / ${first.model}`}
        >
          {first.provider} / {first.model}
        </span>
      ) : undefined,
      absent: "none declared",
      title: "This route declares no providers, so nothing serves its traffic.",
    },
  ];

  return (
    <div className="min-w-0 space-y-6" data-testid="route-detail-page">
      <PageHeader
        breadcrumb={CRUMBS(detail.name)}
        title={detail.name}
        titleMono
        status={<StatusBadge ready={detail.ready} phase={detail.phase} />}
        meta={`${detail.namespace} · ${count} provider${count === 1 ? "" : "s"}`}
        actionsSlot={
          canEdit || canDelete ? (
            <>
              {canEdit && (
                <Button
                  variant="outline"
                  size="sm"
                  className="text-sm"
                  onClick={onOpenEdit}
                  data-testid="edit-route-button"
                >
                  <Pencil className="h-4 w-4" />
                  Edit
                </Button>
              )}
              {canDelete && (
                // Crit as an ACTION colour is §2.3's one sanctioned exception:
                // a destructive control, told apart from a crit STATUS by form
                // (a filled button, never an uppercase mono tag).
                <Button
                  variant="destructive"
                  size="sm"
                  className="text-sm"
                  onClick={onOpenDelete}
                  data-testid="delete-route-button"
                >
                  <Trash2 className="h-4 w-4" />
                  Delete
                </Button>
              )}
            </>
          ) : undefined
        }
      />

      {/* §4.7 hub grid: the order on the left, the bound facts in the 300px
          rail, which stacks UNDER the main column below `lg`. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          <Card className="min-w-0">
            <PanelHeader
              title="The failover order"
              meta={count > 0 ? `${count} provider${count === 1 ? "" : "s"}` : undefined}
            />
            <CardContent>
              {count === 0 ? (
                // §7 A2: a missing SECTION teaches rather than sitting blank.
                // A route with no providers is not an empty list, it is a route
                // that cannot serve anything, and the copy says which.
                <QuietNote title="This route has no providers yet.">
                  Nothing serves its traffic: an agent pointed at this route has
                  nowhere to send a model call. Add at least one provider — the
                  first one you add becomes its first choice.
                  {canEdit && (
                    <span className="mt-3 block">
                      <NextStepLink
                        label="Add a provider"
                        to={`/routes/${encodeURIComponent(detail.namespace)}/${encodeURIComponent(detail.name)}?edit=1`}
                      />
                    </span>
                  )}
                </QuietNote>
              ) : (
                <>
                  <p className="mb-2 max-w-[64ch] text-sm text-secondary-foreground">
                    The route tries these in priority order. The first one that
                    answers serves the request; the rest are what it falls back
                    to when it can&rsquo;t.
                  </p>
                  <ol>
                    {ordered.map((p, i) => (
                      <li
                        key={`${p.provider}/${p.model}/${i}`}
                        className="border-b border-border-soft py-4 last:border-0"
                        data-testid={`provider-row-${i}`}
                      >
                        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                          {/* The rank is the eyebrow, not a Tag: it names a
                              POSITION, and a position is not a status — no
                              semantic hue may claim it (§2.2). */}
                          <span className="font-mono text-2xs uppercase tracking-wide text-faint">
                            {RANK[i] ?? `Then to (${i + 1})`}
                          </span>
                          <span
                            className="min-w-0 truncate font-mono text-sm font-medium"
                            title={`${p.provider} / ${p.model}`}
                          >
                            <span>{p.provider}</span>
                            <span aria-hidden="true" className="text-ghost">
                              {" / "}
                            </span>
                            <span>{p.model}</span>
                          </span>
                          <span className="ml-auto whitespace-nowrap font-mono text-xs tabular-nums text-faint">
                            priority {p.priority}
                          </span>
                        </div>
                        <KeyValueList
                          className="mt-1"
                          items={[
                            {
                              key: "Credential",
                              value: p.secretBindingRef ? (
                                // If it names a resource, it is a link (M22).
                                <ResourceLink
                                  kind="secretbinding"
                                  namespace={detail.namespace}
                                  name={p.secretBindingRef}
                                  className="text-sm"
                                />
                              ) : undefined,
                              absent: "not attached",
                              title:
                                "No SecretBinding is attached to this provider. The CRD requires one, or an API base, for every non-mock provider.",
                              mono: false,
                            },
                            {
                              key: "API base",
                              value: p.apiBase ? (
                                <span className="block truncate" title={p.apiBase}>
                                  {p.apiBase}
                                </span>
                              ) : undefined,
                              absent: "the provider default",
                              title:
                                "No override is set, so calls go to the provider's own endpoint.",
                            },
                          ]}
                        />
                      </li>
                    ))}
                  </ol>
                </>
              )}
            </CardContent>
          </Card>

          {/* Reverse-lookup: the agents that route through this ModelRoute
              (m18.9). A list of links, so it stays in the main column — §4.7
              rails carry kv-lists and meters only. */}
          <UsedBySection
            kind="modelroute"
            name={detail.name}
            namespace={detail.namespace}
            title="Used by agents"
          />
        </div>

        <div className="min-w-0 space-y-5">
          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <KeyValueList items={facts} />
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="The tenant rate cap" />
            <CardContent>
              {detail.rateLimit ? (
                // A known cap with no known usage: the Meter draws the bound
                // (it is real) and omits the fill (it is not). A zero-width bar
                // labelled 0 would claim this route has served nothing.
                <Meter
                  label="Requests per minute"
                  used={UNKNOWN}
                  cap={detail.rateLimit.tenantRPM}
                  format={(n) => `${formatCount(n)}/min`}
                  thing="route"
                />
              ) : (
                <KeyValueList
                  items={[
                    {
                      key: "Requests per minute",
                      absent: "not capped",
                      title:
                        "This route sets no per-tenant rate cap, so requests are bounded only by the provider's own limits.",
                    },
                  ]}
                />
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      {/* Edit wizard */}
      <DetailDrawer
        open={editOpen}
        onClose={onCloseEdit}
        title={`Edit ${detail.name}`}
        subtitle="Repoints the route — which providers it tries, and in what order"
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

// The page's bespoke kv row is gone: `KeyValueList` (§5.25) is the console's
// one fact-row vocabulary, and a second one here is how a design system decays.

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

  // ── What is unsaved, and how the form says so ─────────────────────────────
  //
  // Dirtiness is COMPUTED against the record rather than tracked as a flag: a
  // field edited and then edited back is not a change, and a flag that says it
  // is makes the discard guard cry wolf until people click through it.
  const bound: ProviderForm[] =
    detail.providers.length > 0
      ? detail.providers.map(providerToForm)
      : [emptyProvider()];
  /** Normalised so whitespace and "1" vs 1 are not mistaken for edits. */
  const shape = (list: ProviderForm[]) =>
    JSON.stringify(
      list.map((p) => ({
        provider: p.provider.trim(),
        model: p.model.trim(),
        priority: parseInt(p.priority, 10) || 1,
        secretBindingRef: p.secretBindingRef.trim(),
        apiBase: p.apiBase.trim(),
      })),
    );
  const boundRpm = detail.rateLimit ? String(detail.rateLimit.tenantRPM) : "";
  const providersChanged = shape(providers) !== shape(bound);
  const rpmChanged = rateLimitRpm.trim() !== boundRpm;
  const dirty = providersChanged || rpmChanged;

  /** True when THIS provider row differs from the one at the same position in
   *  the record — including "there was no row here", which is also a change. */
  const rowChanged = (i: number) =>
    shape([providers[i]]) !== (bound[i] ? shape([bound[i]]) : "[]");

  /** The unsaved line. It rides at the top of EVERY step, not only the review,
   *  because the moment the form diverges from the record is the moment the
   *  reader needs to know nothing has been written yet. */
  const unsavedNote = dirty ? (
    <p className="text-sm text-secondary-foreground" data-testid="route-edit-unsaved">
      <span className="font-mono text-2xs uppercase tracking-wide text-faint">
        Unsaved
      </span>{" "}
      This form differs from the route as it stands. Nothing changes until you
      press Save changes.
    </p>
  ) : null;

  /** One review row: what it is now, and what it becomes. An unchanged row says
   *  so rather than looking identical to a changed one. */
  function change(was: string, next: string, isChanged: boolean): React.ReactNode {
    if (!isChanged) {
      return (
        <span className="block truncate text-faint" title={was}>
          {was || "—"} <span className="text-ghost">(unchanged)</span>
        </span>
      );
    }
    return (
      <span className="block truncate" title={`${was || "—"} → ${next || "—"}`}>
        <span className="text-faint line-through">{was || "—"}</span>{" "}
        <span aria-hidden="true" className="text-ghost">
          →
        </span>{" "}
        {next || "—"}
      </span>
    );
  }

  /** "anthropic / claude-opus-4 @1" — one provider as one comparable line. */
  const line = (p?: ProviderForm) =>
    p ? `${p.provider.trim() || "—"} / ${p.model.trim() || "—"} @${parseInt(p.priority, 10) || 1}` : "";

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
        <SectionHeader
          title="Providers, in order"
          lede="The lowest priority serves the traffic; the ones after it are what the route falls back to."
          as="h3"
        />
        {unsavedNote}
        {providers.map((p, i) => (
          <div
            key={i}
            className="space-y-3 rounded-lg border border-border p-3"
            data-testid={`provider-entry-${i}`}
          >
            <div className="flex items-center justify-between">
              <span className="font-mono text-2xs uppercase tracking-wide text-faint">
                Provider {i + 1}
                {rowChanged(i) && (
                  <span className="ml-2 normal-case text-secondary-foreground">
                    changed — not saved yet
                  </span>
                )}
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
        <SectionHeader
          title="Rate limit"
          lede="A ceiling on requests per minute, per tenant. Clear it and the route carries no cap of its own."
          as="h3"
        />
        {unsavedNote}
        <FormField
          id="rate-limit-rpm"
          label="Tenant RPM (leave blank to clear)"
          hint={rpmChanged ? "Changed — not saved yet" : undefined}
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
        <SectionHeader
          title={dirty ? "What changes when you save" : "Nothing has changed"}
          lede={
            dirty
              ? "The route still serves what it served. Saving repoints it."
              : "This form matches the route as it stands, so saving would write the same thing back."
          }
          as="h3"
        />
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="You don't have permission to edit this route."
            resource="model routes"
            permission="update"
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          // The API's own words, verbatim: a CRD validation message names the
          // field that has to change, and paraphrasing loses it. It sits in a
          // code well (§4.5) so a long rule keeps its shape and scrolls in its
          // own frame rather than widening the step.
          <div role="alert">
            <p className="font-serif text-md font-medium text-destructive">
              The route wasn&rsquo;t saved.
            </p>
            <pre
              className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground"
              data-testid="route-edit-error"
            >
              {saveState.message}
            </pre>
            <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
              Nothing was written to the cluster. Go back, fix what it named, and
              save again.
            </p>
          </div>
        )}

        {/* The providers, position by position, so a reordering reads as a
            reordering rather than as five unrelated edits. */}
        <div className="space-y-1">
          <p className="font-mono text-2xs uppercase tracking-wide text-faint">
            Providers, in priority order
          </p>
          <KeyValueList
            items={Array.from(
              { length: Math.max(providers.length, bound.length) },
              (_, i): KeyValueItem => ({
                key: RANK[i] ?? `Then to (${i + 1})`,
                value: change(line(bound[i]), line(providers[i]), rowChanged(i)),
              }),
            )}
          />
        </div>

        <KeyValueList
          items={[
            {
              key: "Tenant rate cap",
              value: change(
                boundRpm ? `${boundRpm}/min` : "not capped",
                rateLimitRpm.trim() ? `${rateLimitRpm.trim()}/min` : "not capped",
                rpmChanged,
              ),
            },
          ]}
        />
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
      // An edit that has diverged from the record routes Cancel/Esc through the
      // discard guard, so an unsaved change cannot vanish on a stray keystroke.
      dirty={dirty}
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
//
// The create surface is M151 §6.1 archetype A4 and adopts the editorial kit.
// Its kit imports live in the single module-level statement at the top of this
// file (see the note there).

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
      <div className="space-y-5">
        <SectionHeader
          title="Identity"
          lede="What the route is called, and which namespace it lives in. Agents reference it by this name."
        />
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
      </div>
    ),
  };

  const providersStep: WizardStep = {
    id: "providers",
    title: "Providers",
    description: "Ordered list of provider/model entries",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="Providers, in order"
          lede="The first entry serves the traffic; the ones below it are the fallbacks, tried in priority order when it can’t."
        />
        {providers.map((p, i) => (
          <div
            key={i}
            className="space-y-3 rounded-lg border border-border p-3"
            data-testid={`new-provider-entry-${i}`}
          >
            <div className="flex items-center justify-between">
              <span className="font-mono text-2xs uppercase tracking-wide text-faint">
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
      <div className="space-y-5">
        <SectionHeader
          title="Rate limit"
          lede="An optional ceiling on requests per minute, per tenant. Leave it blank and the route carries no cap of its own."
        />
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
      </div>
    ),
  };

  // The choices, as facts. A blank field is not a blank row (kit KeyValueList):
  // it states what leaving it blank MEANS, in the ghost register, so the review
  // never shows an empty cell the reader has to interpret.
  const routeFacts: KeyValueItem[] = [
    { key: "Name", value: routeName.trim() || undefined, absent: "not named yet" },
    {
      key: "Namespace",
      value: routeNs.trim() || undefined,
      absent: "default",
      title: "Left blank — the route is created in the default namespace.",
    },
    { key: "Providers", value: providers.length },
    {
      key: "Rate limit",
      value: rateLimitRpm.trim() ? `${rateLimitRpm.trim()} RPM` : undefined,
      absent: "no cap",
      title: "Left blank — this route carries no per-tenant rate limit of its own.",
    },
  ];

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-5" data-testid="new-route-review">
        <SectionHeader
          title="Review the route"
          lede="Nothing exists in the cluster yet. Creating it writes one ModelRoute — and only that."
        />
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create routes"
            description="Your account can't create ModelRoutes in this cluster."
            resource="model routes"
            permission="create"
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <div
            className="rounded-lg border border-destructive/40 bg-destructive-surface/40 px-4 py-3"
            role="alert"
            data-testid="new-route-error"
          >
            <p className="font-serif text-md font-medium text-destructive">
              The route wasn’t created.
            </p>
            {/* The API's own words, verbatim — a CRD validation message names
                the field that has to change, and paraphrasing loses it. */}
            <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
              {saveState.message}
            </pre>
            <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
              Nothing was written to the cluster. Go back, fix what it named, and
              create it again.
            </p>
          </div>
        )}
        <KeyValueList items={routeFacts} />
        <div className="space-y-2">
          <p className="font-mono text-2xs uppercase tracking-wide text-faint">
            Providers, in priority order
          </p>
          {providers.map((p, i) => (
            <div
              key={i}
              className="flex flex-wrap items-baseline gap-x-3 gap-y-1 rounded-md border border-border bg-surface-2/40 px-3 py-2"
            >
              <span className="font-mono text-sm">
                {p.provider || "—"}/{p.model || "—"}
              </span>
              <span className="font-mono text-xs text-faint">
                priority {p.priority}
              </span>
              {p.apiBase && (
                <span className="min-w-0 break-all font-mono text-xs text-faint">
                  {p.apiBase}
                </span>
              )}
            </div>
          ))}
        </div>
      </div>
    ),
  };

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="New model route"
        lede="A route decides which provider serves a model call, and which one takes over when the first can’t. Agents point at the route, never at a vendor."
      />
      {/* §6.1 A4: the Wizard's 15rem rail + its 2rem gap + the archetype's
          46rem content column. Capping the outer column sizes the inner one. */}
      <div className="max-w-[63rem]">
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
    </div>
  );
}
