import { useCallback, useEffect, useId, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { PlugZap, Plus, RotateCw, Sparkles, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ConfirmDialog,
  DataTable,
  useFocusTrap,
  useToast,
  type Column,
  type DataTableError,
} from "@/components/kit";
import { api, ApiError, type ProviderSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_SECRETS } from "@/lib/nav";

// ProvidersPage — the connected-providers management surface (m18.5, ADR 0018).
// Lists providers connected via the wizard (GET /api/providers), each of which is a
// Secret + SecretBinding + ModelRoute, with Rotate key + Disconnect + "create agent
// using this". SECURITY: no key material is ever rendered — only the Secret NAME as
// a reference. Rotation sends the new key ONLY in the POST body (server validates +
// stores it); it is never held client-side beyond the input.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: ProviderSummary[] }
  | { kind: "disabled" } // 404 = the connect kill-switch
  | { kind: "error"; message: string; forbidden: boolean };

type Action =
  | { kind: "idle" }
  | { kind: "rotate"; p: ProviderSummary }
  | { kind: "disconnect"; p: ProviderSummary };

export function ProvidersPage() {
  const navigate = useNavigate();
  const { can, reprobe } = useCapabilities();
  const canConnect = can(RES_SECRETS, "create");
  const canRotate = can(RES_SECRETS, "update");
  const canDisconnect = can(RES_SECRETS, "delete");

  const { toast } = useToast();
  const [query, setQuery] = useState("");
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

  const all = state.kind === "ready" ? state.items : [];
  const q = query.toLowerCase();
  const items = q
    ? all.filter(
        (p) =>
          p.name.toLowerCase().includes(q) ||
          p.provider.toLowerCase().includes(q) ||
          p.displayName.toLowerCase().includes(q),
      )
    : all;

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
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<ProviderSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (p) => <span className="font-medium">{p.displayName || p.name}</span>,
    },
    {
      id: "provider",
      header: "Provider",
      cell: (p) => <span className="text-muted-foreground">{p.provider}</span>,
    },
    {
      id: "models",
      header: "Models",
      hideOnMobile: true,
      cell: (p) => (
        <span className="text-xs text-muted-foreground">
          {p.models.length === 0
            ? "—"
            : `${p.models.length} model${p.models.length === 1 ? "" : "s"}`}
        </span>
      ),
    },
    {
      id: "secret",
      header: "Secret",
      hideOnMobile: true,
      cell: (p) => (
        // The Secret NAME as a reference — never the key material.
        <span className="font-mono text-xs text-muted-foreground">
          {p.secretName || "—"}
        </span>
      ),
    },
    {
      id: "status",
      header: "Status",
      className: "w-28",
      cell: (p) => (
        <Badge variant={p.ready ? "success" : "warning"}>
          {p.ready ? "Ready" : "Pending"}
        </Badge>
      ),
    },
    {
      id: "actions",
      header: "",
      className: "w-44 text-right",
      cell: (p) => (
        <div
          className="flex items-center justify-end gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
          onClick={(e) => e.stopPropagation()}
          data-testid={`row-actions-${p.name}`}
        >
          <Button
            variant="ghost"
            size="sm"
            className="h-7"
            aria-label={`Create agent using ${p.name}`}
            data-testid={`use-${p.name}`}
            onClick={(e) => {
              e.stopPropagation();
              navigate("/agents/new");
            }}
          >
            <Sparkles className="mr-1 h-3.5 w-3.5" />
            Use
          </Button>
          {canRotate && (
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7"
              aria-label={`Rotate key for ${p.name}`}
              data-testid={`rotate-${p.name}`}
              onClick={(e) => {
                e.stopPropagation();
                setActionError(null);
                setAction({ kind: "rotate", p });
              }}
            >
              <RotateCw className="h-3.5 w-3.5" />
            </Button>
          )}
          {canDisconnect && (
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 text-destructive hover:text-destructive"
              aria-label={`Disconnect ${p.name}`}
              data-testid={`disconnect-${p.name}`}
              onClick={(e) => {
                e.stopPropagation();
                setActionError(null);
                setAction({ kind: "disconnect", p });
              }}
            >
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
      ),
    },
  ];

  if (state.kind === "disabled") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="providers-page">
        <h2 className="text-2xl font-semibold tracking-tight">Providers</h2>
        <div
          className="rounded-lg border bg-card p-8 text-center text-sm text-muted-foreground"
          data-testid="providers-disabled"
        >
          Provider connect is disabled on this install (the Helm kill-switch). Ask
          your operator, or reference an existing SecretBinding + ModelRoute directly.
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="providers-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Providers</h2>
          <p className="text-sm text-muted-foreground">
            LLM providers connected to the platform — each is a Secret + SecretBinding
            + ModelRoute. Keys are stored server-side and never shown.
          </p>
        </div>
        {canConnect && (
          <Button
            size="sm"
            onClick={() => navigate("/providers/connect")}
            data-testid="connect-provider-button"
          >
            <Plus className="h-4 w-4" />
            Connect provider
          </Button>
        )}
      </div>

      {actionError && action.kind === "idle" && (
        <p
          className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
          role="alert"
          data-testid="providers-action-error"
        >
          {actionError}
        </p>
      )}

      <DataTable<ProviderSummary>
        columns={columns}
        rows={items}
        rowKey={(p) => `${p.namespace}/${p.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter providers…"
        ariaLabel="Connected providers"
        empty={{
          icon: PlugZap,
          title: "No providers connected",
          description:
            "Connect a provider to give your agents a model to call. You'll paste a key once — it's stored server-side.",
        }}
      />

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

// RotateKeyDialog — a modal to paste a NEW provider key. The key lives only in this
// input + the POST body; it is never stored client-side and the input is discarded
// on close. Enter submits when non-empty.
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
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onCancel });

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
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 id={titleId} className="text-lg font-semibold tracking-snug">
          Rotate key — {provider.displayName || provider.name}
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Paste a new API key. It's validated against the provider, then stored
          server-side — the old key is replaced. If the new key is invalid, nothing
          changes.
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
          <p className="mt-3 text-sm text-destructive" role="alert" data-testid="rotate-error">
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
