import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Filter, PlugZap, Plus, RotateCw, Sparkles, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  CellEntity,
  ClosingNote,
  ConfirmDialog,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  StatusBadge,
  nextStepRank,
  resourcePath,
  useFocusTrap,
  useToast,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
} from "@/components/kit";
import { api, ApiError, type ProviderSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_SECRETS } from "@/lib/nav";

// ProvidersPage — the connected-providers admin surface (m18.5, ADR 0018;
// re-housed on the editorial system in M151, spec §6.2: "Rotate/Disconnect as
// row actions; connect CTA in header").
//
// A row here is one connection: a Secret + SecretBinding + ModelRoute created by
// the connect wizard. SECURITY, unchanged and non-negotiable: no key material is
// ever rendered — the list shows the connection, its provider, and how many
// models it exposes, never a key and (since m23.7/audit B7) not even the backing
// Secret's name, which is storage plumbing rather than something a person
// reasons about. Rotation sends the new key ONLY in the POST body; it is never
// held client-side beyond the input, which is discarded on close.
//
// ── WHAT THE PAGE SAYS TO DO ────────────────────────────────────────────────
// A connection that is not Ready cannot serve a model, and this list cannot
// know WHY — the reason lives on the ModelRoute's conditions. So the Next step
// sends the reader to the route itself rather than guessing at a cause; a route
// that is Ready but exposes no models gets the one honest instruction that
// fixes it. Everything else reads "Nothing needed", because it is.
//
// ── THE ROW ACTIONS RENDER AT REST ──────────────────────────────────────────
// They used to be `opacity-0 group-hover:opacity-100` inside a normal column —
// but the DataTable's `<tr>` carries no `group` class, so the hover rule could
// never fire and Rotate/Disconnect were invisible at every width unless
// focused. A control unreachable by mouse is a defect, not restraint. They now
// live in the table's own `rowActions` slot and are quiet-but-present: faint
// icons that firm up on hover.
//
// RBAC: display-only gating (the API is the real gate, ADR 0011). A viewer sees
// the list and no write affordances at all — no greyed-out buttons.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: ProviderSummary[] }
  | { kind: "disabled" } // 404 = the connect kill-switch
  | { kind: "error"; message: string; forbidden: boolean };

type Action =
  | { kind: "idle" }
  | { kind: "rotate"; p: ProviderSummary }
  | { kind: "disconnect"; p: ProviderSummary };

// ── Triage ──────────────────────────────────────────────────────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Triaged {
  p: ProviderSummary;
  next: NextStep;
}

/**
 * One connection → its next step.
 *
 * `ready` is the backing ModelRoute's Ready condition, and only that route
 * knows why it is false (an unresolvable SecretBinding, a rejected key, a still-
 * reconciling revision). The step therefore points AT the route, where the
 * condition message is, instead of asserting a cause this list cannot see.
 */
function triage(p: ProviderSummary): Triaged {
  const to = resourcePath("route", p.namespace, p.name) ?? undefined;
  if (!p.ready) {
    // Deliberately NOT crit: `ready: false` covers a rejected key AND a route
    // that is still reconciling, and this list cannot tell them apart. §5.19
    // reserves the crit link for a target that IS a failure or a stop, so the
    // link stays pine and the readiness chip carries the state.
    return { p, next: { label: "Open its route", tone: "default", to } };
  }
  if (p.models.length === 0) {
    // Ready, but exposing nothing an agent can call. A real, fixable gap.
    return { p, next: { label: "Add a model", tone: "default", to } };
  }
  return { p, next: { tone: "none" } };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "all" | "attention" | "serving";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "all", label: "Everything", match: () => true },
  { id: "attention", label: "Needs you", match: (t) => t.next.tone !== "none" },
  {
    id: "serving",
    label: "Serving models",
    match: (t) => t.p.ready && t.p.models.length > 0,
  },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  attention: {
    title: "Nothing needs a person",
    description:
      "Every connection is Ready and exposing at least one model. Show everything to see them all.",
  },
  serving: {
    title: "Nothing is serving a model yet",
    description:
      "No connection is both Ready and exposing a model. Show everything to see what is still coming up.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. The providers endpoint returns every connection in one
 * response, so these counts are the whole truth and not a page of it.
 */
export function closingLine(rows: Triaged[]): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const quiet = rows.filter((t) => nextStepRank(t.next.tone) !== 0);
  const needs = total - quiet.length;
  // Counted over the SERVING rows only. Summing every row's models would credit
  // the sentence's "the other N are serving M models" with models that belong
  // to a connection which is not serving at all — a small lie, in the sentence
  // whose whole job is to restate the table accurately.
  const models = quiet.reduce((n, t) => n + t.p.models.length, 0);
  if (needs === 0) {
    return total === 1
      ? `One connection, serving ${models} model${models === 1 ? "" : "s"} to your agents. Nothing here needs a person.`
      : `All ${total} connections are serving — ${models} models between them — and none of them needs a person.`;
  }
  if (needs === total) {
    return total === 1
      ? "The one connection here needs a person before an agent can call it."
      : `All ${total} connections need a person before an agent can call them.`;
  }
  return `${needs} of the ${total} connections need${needs === 1 ? "s" : ""} a person. The other ${quiet.length} are serving ${models} models between them.`;
}

// RowActions renders the per-row affordances, RBAC-aware and hidden entirely
// for a viewer. The row-click is stopped on the group so a button never fires
// twice (this table has no row navigation today, but the guard is free and the
// next person to add one should not have to remember).
function RowActions({
  p,
  canRotate,
  canDisconnect,
  onUse,
  onRotate,
  onDisconnect,
}: {
  p: ProviderSummary;
  canRotate: boolean;
  canDisconnect: boolean;
  onUse: (p: ProviderSummary) => void;
  onRotate: (p: ProviderSummary) => void;
  onDisconnect: (p: ProviderSummary) => void;
}) {
  return (
    <div
      className="flex items-center justify-end gap-1"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${p.name}`}
    >
      <Button
        variant="ghost"
        size="sm"
        className="h-7 text-faint hover:text-foreground"
        aria-label={`Create agent using ${p.name}`}
        title={`Create agent using ${p.displayName || p.name}`}
        data-testid={`use-${p.name}`}
        onClick={(e) => {
          e.stopPropagation();
          onUse(p);
        }}
      >
        <Sparkles className="h-3.5 w-3.5" />
        {/* The label costs ~90px, which is exactly what pushes Disconnect out
            of the table's scroll frame below 1024. Below that the button is the
            icon alone — still named for assistive tech and still hoverable. */}
        <span className="hidden lg:inline">Create agent</span>
      </Button>
      {canRotate && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-foreground"
          aria-label={`Rotate key for ${p.name}`}
          data-testid={`rotate-${p.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onRotate(p);
          }}
        >
          <RotateCw className="h-3.5 w-3.5" />
        </Button>
      )}
      {canDisconnect && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-destructive"
          aria-label={`Disconnect ${p.name}`}
          data-testid={`disconnect-${p.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDisconnect(p);
          }}
        >
          <Trash2 className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  );
}

export function ProvidersPage() {
  const navigate = useNavigate();
  const { can, canFlow, reprobe } = useCapabilities();
  // Both of these are now the SERVER's answer about the whole flow, not a conjunction this
  // page assembles. It got that wrong twice: connect omitted the ModelRoute the handler
  // writes (and the Secret is written first, so the denial landed after a live credential
  // was in the cluster), and rotate asked about `secretbindings.update` while the write that
  // matters is the core Secret. See internal/bff/flows.go.
  const canConnect = canFlow("connectProvider");
  const canRotate = canFlow("rotateProviderKey");
  const canDisconnect = can(RES_SECRETS, "delete");

  const { toast } = useToast();
  const [query, setQuery] = useState("");
  const [view, setView] = useState<ViewId>("all");
  const [state, setState] = useState<Load>({ kind: "loading" });
  const [action, setAction] = useState<Action>({ kind: "idle" });
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listProviders(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // Read `items` (the console key), fall back to `providers`, default [].
        setState({ kind: "ready", items: res.items ?? res.providers ?? [] });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.status === 404) {
          setState({ kind: "disabled" });
          return;
        }
        const forbidden = err instanceof ApiError && err.isForbidden;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const all = useMemo(
    () => (state.kind === "ready" ? state.items : []),
    [state],
  );

  // Triage once, sort once. Attention-first (§6.1): anything a person has to
  // touch sits above everything that is simply working, then alphabetically.
  const sorted = useMemo(() => {
    const rows = all.map(triage);
    rows.sort(
      (a, b) =>
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        (a.p.displayName || a.p.name).localeCompare(b.p.displayName || b.p.name),
    );
    return rows;
  }, [all]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[0];
  const q = query.trim().toLowerCase();
  const visible = useMemo(() => {
    const byView = sorted.filter(activeView.match);
    return q
      ? byView.filter(
          (t) =>
            t.p.name.toLowerCase().includes(q) ||
            t.p.provider.toLowerCase().includes(q) ||
            t.p.displayName.toLowerCase().includes(q),
        )
      : byView;
  }, [sorted, activeView, q]);

  // GET /api/providers returns every connection in ONE response (no cursor), so
  // these counts describe the whole set — the condition the FilterChipRow
  // contract requires before a chip may carry a number at all.
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: state.kind === "ready" ? sorted.filter(v.match).length : undefined,
  }));

  function closeAction() {
    setAction({ kind: "idle" });
    setActionError(null);
  }

  async function onRotate(p: ProviderSummary, apiKey: string) {
    setBusy(true);
    setActionError(null);
    try {
      await api.rotateProviderKey(p.name, apiKey, p.namespace);
      toast({
        variant: "success",
        title: `Rotated ${p.displayName || p.name}`,
        description: "The provider key was validated and updated server-side.",
      });
      closeAction();
      load();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setActionError(err instanceof Error ? err.message : "rotate failed");
    } finally {
      setBusy(false);
    }
  }

  async function onDisconnect(p: ProviderSummary) {
    setBusy(true);
    setActionError(null);
    try {
      await api.disconnectProvider(p.name, p.namespace);
      toast({
        variant: "success",
        title: `Disconnected ${p.displayName || p.name}`,
        description: "The provider's route, binding, and secret were removed.",
      });
      closeAction();
      load();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setActionError(err instanceof Error ? err.message : "disconnect failed");
    } finally {
      setBusy(false);
    }
  }

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          resource: "connected providers",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 resource-list budget, in visual order. Entity, State and Next step
  // survive every width — the row's identity, its condition, and what to do
  // about it. Provider kind is the first to go (priority 4): it is usually
  // already legible in the display name.
  const columns: Column<Triaged>[] = [
    {
      id: "name",
      header: "Connection",
      priority: 1,
      className: "max-w-[20rem]",
      cell: ({ p }) => (
        <CellEntity
          name={p.displayName || p.name}
          title={p.displayName || p.name}
          namespace={p.namespace}
        />
      ),
    },
    {
      id: "provider",
      header: "Provider",
      priority: 4,
      cell: ({ p }) => (
        <span className="whitespace-nowrap font-mono text-xs text-faint">
          {p.provider}
        </span>
      ),
    },
    {
      id: "models",
      header: "Models",
      priority: 3,
      numeric: true,
      // The count is DISCOVERABLE (M100 UI99-refs): hovering names the models
      // rather than leaving a dead number — there is no per-model page to link.
      // A connection exposing none renders a real `0`, not a dash: zero models
      // is something we measured, and the Next step column says what to do.
      cell: ({ p }) => (
        <span
          title={p.models.length ? p.models.join(", ") : "This connection exposes no models."}
          data-testid={`provider-models-${p.name}`}
        >
          <QuantityValue value={p.models.length} />
        </span>
      ),
    },
    {
      id: "status",
      header: "State",
      priority: 1,
      className: "w-[9rem]",
      cell: ({ p }) => <StatusBadge ready={p.ready} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4).
      priority: 1,
      className: "w-[10rem]",
      cell: ({ p, next }) => (
        <NextStepLink
          label={next.label}
          to={next.to}
          tone={next.tone}
          ariaLabel={next.label ? `${next.label} — ${p.displayName || p.name}` : undefined}
          testId={`provider-next-${p.name}`}
        />
      ),
    },
  ];

  // Connect is kill-switched by Helm (404). Nothing is broken and nothing is
  // hidden — this install simply does not offer the flow (§7.1, calm).
  if (state.kind === "disabled") {
    return (
      <div className="min-w-0 space-y-6" data-testid="providers-page">
        <PageHeader
          title="Providers"
          lede="The LLM connections your agents run on. Connect a provider once and its models become available to every agent."
        />
        <div data-testid="providers-disabled">
          <QuietNote title="Connecting a provider is switched off on this install.">
            An operator disabled the connect flow (the Helm kill-switch), so this
            console cannot create or list provider connections here. Ask your
            operator to connect one for you, or wire the ModelRoute and
            SecretBinding directly. Nothing is missing from the platform — only
            from this page.
          </QuietNote>
        </div>
      </div>
    );
  }

  // A chip view that empties is the "empty-filtered" truth (§7), not the
  // first-run one: it offers a way back out rather than teaching someone with
  // six connections how to make their first.
  const viewEmptied = all.length > 0 && visible.length === 0 && view !== "all" && q === "";
  const empty: EmptyStateProps = viewEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: all.length,
        countNoun: "connections",
      }
    : {
        icon: PlugZap,
        title: "No providers connected",
        description:
          "Connect one to give agents a model to run on. You paste a key once and it is stored server-side — the console never shows it again.",
        action: canConnect
          ? {
              label: "Connect a provider",
              icon: Plus,
              onClick: () => navigate("/providers/connect"),
            }
          : undefined,
      };

  const closing = state.kind === "ready" ? closingLine(sorted) : null;
  const metaLine =
    state.kind === "ready"
      ? `${all.length} connection${all.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="providers-page">
      <PageHeader
        title="Providers"
        meta={metaLine}
        lede="The LLM connections your agents run on. Connect a provider once and its models become available to every agent. Keys are stored server-side and never shown."
        // Through `actionsSlot` rather than the structured `actions` list:
        // `PageHeaderAction` carries no `testId`, and the viewer suite asserts
        // that `connect-provider-button` is ABSENT for a viewer — an assertion
        // that would silently pass forever if the id disappeared.
        actionsSlot={
          canConnect ? (
            <Button
              size="sm"
              className="text-sm"
              onClick={() => navigate("/providers/connect")}
              data-testid="connect-provider-button"
            >
              <Plus className="h-4 w-4" />
              Connect provider
            </Button>
          ) : undefined
        }
      />

      {(state.kind === "loading" || all.length > 0) && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter providers"
          className="min-w-0"
        />
      )}

      {actionError && action.kind === "idle" && (
        <p
          className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
          role="alert"
          data-testid="providers-action-error"
        >
          {actionError}
        </p>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={({ p }) => `${p.namespace}/${p.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter providers…"
        ariaLabel="Connected providers"
        // Rotate and Disconnect are RBAC-gated inside RowActions, so a viewer
        // gets a clean table with no dead or greyed-out buttons.
        rowActions={(t) => (
          <RowActions
            p={t.p}
            canRotate={canRotate}
            canDisconnect={canDisconnect}
            onUse={(p) =>
              // m21: carry the provider so create-agent defaults its model
              // picker to this provider's models (provider-as-model-home).
              navigate(`/agents/new?provider=${encodeURIComponent(p.provider)}`)
            }
            onRotate={(p) => {
              setActionError(null);
              setAction({ kind: "rotate", p });
            }}
            onDisconnect={(p) => {
              setActionError(null);
              setAction({ kind: "disconnect", p });
            }}
          />
        )}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {action.kind === "rotate" && (
        <RotateKeyDialog
          provider={action.p}
          busy={busy}
          error={actionError}
          onCancel={closeAction}
          onRotate={(key) => void onRotate(action.p, key)}
        />
      )}

      <ConfirmDialog
        open={action.kind === "disconnect"}
        onCancel={closeAction}
        onConfirm={() => {
          if (action.kind === "disconnect") void onDisconnect(action.p);
        }}
        title="Disconnect this provider?"
        description={
          action.kind === "disconnect"
            ? `Removes the ModelRoute, SecretBinding, and Secret for "${action.p.displayName || action.p.name}". Agents using it will fail until it is reconnected.`
            : undefined
        }
        confirmText={action.kind === "disconnect" ? action.p.name : undefined}
        confirmLabel="Disconnect"
        busy={busy}
        destructive
      />
    </div>
  );
}

// RotateKeyDialog — a modal to paste a NEW provider key. The key lives only in
// this input + the POST body; it is never stored client-side and the input is
// discarded on close. Enter submits when non-empty.
function RotateKeyDialog({
  provider,
  busy,
  error,
  onCancel,
  onRotate,
}: {
  provider: ProviderSummary;
  busy: boolean;
  error: string | null;
  onCancel: () => void;
  onRotate: (key: string) => void;
}) {
  const [key, setKey] = useState("");
  const titleId = useId();
  const panelRef = useFocusTrap<HTMLDivElement>({
    active: true,
    onEscape: onCancel,
  });

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onCancel}
        aria-hidden="true"
      />
      {/* A genuinely floating layer — one of the few places §2.7 allows a
          shadow, and it keeps its border too. */}
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 id={titleId} className="font-serif text-xl font-medium tracking-snug">
          Rotate key — {provider.displayName || provider.name}
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Paste a new API key. It is validated against the provider, then stored
          server-side — the old key is replaced. If the new key is invalid,
          nothing changes.
        </p>
        <div className="mt-4 space-y-1.5">
          <Label htmlFor="rotate-key">New API key</Label>
          <Input
            id="rotate-key"
            type="password"
            autoComplete="off"
            value={key}
            placeholder="sk-…"
            data-testid="rotate-key-input"
            onChange={(e) => setKey(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && key.trim() && !busy) onRotate(key);
            }}
          />
        </div>
        {error && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="rotate-error"
          >
            {error}
          </p>
        )}
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => onRotate(key)}
            disabled={!key.trim() || busy}
            data-testid="rotate-confirm"
          >
            {busy ? "Rotating…" : "Rotate key"}
          </Button>
        </div>
      </div>
    </div>
  );
}
