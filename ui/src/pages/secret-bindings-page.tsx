import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { KeyRound, Pencil, Plus, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type SecretBindingSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_SECRETS } from "@/lib/nav";

// SecretBindingsPage — list of SecretBindings.
//
// SECURITY INVARIANT: this page (and all SecretBinding UI) NEVER renders,
// requests, or transmits a secret value. The DTO carries only:
//   - secretRef.name  — which K8s Secret object holds the credential
//   - secretRef.key   — which key within that Secret
//   - phase/ready     — the controller's "Resolved" condition
// There is no value/data field in the DTO or in any form in this UI.

const PAGE_LIMIT = 50;

function RowActions({
  sb,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  sb: SecretBindingSummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (s: SecretBindingSummary) => void;
  onDelete: (s: SecretBindingSummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${sb.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label={`Edit ${sb.name}`}
          data-testid={`edit-${sb.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(sb);
          }}
        >
          <Pencil className="h-3.5 w-3.5" />
        </Button>
      )}
      {canDelete && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-destructive hover:text-destructive"
          aria-label={`Delete ${sb.name}`}
          data-testid={`delete-${sb.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(sb);
          }}
        >
          <Trash2 className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  );
}

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: SecretBindingSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function SecretBindingsPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_SECRETS, "create");
  const canEdit = can(RES_SECRETS, "update");
  const canDelete = can(RES_SECRETS, "delete");

  const [query, setQuery] = useState("");
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [state, setState] = useState<Load>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listSecretBindings(
        {
          limit: PAGE_LIMIT,
          cursor: cursor || undefined,
          q: query || undefined,
          namespace: namespace || undefined,
        },
        controller.signal,
      )
      .then((res) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", items: res.items, nextCursor: res.nextCursor });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const forbidden = err instanceof ApiError && err.isForbidden;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden,
        });
      });
  }, [cursor, query, namespace]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const resetPaging = useCallback(() => setPageStack([""]), []);

  function onQueryChange(q: string) {
    setQuery(q);
    resetPaging();
  }

  const prevNs = useRef(namespace);
  useEffect(() => {
    if (prevNs.current !== namespace) {
      prevNs.current = namespace;
      resetPaging();
    }
  }, [namespace, resetPaging]);

  const items = state.kind === "ready" ? state.items : [];
  const nextCursor = state.kind === "ready" ? state.nextCursor : "";
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;

  function onNext() {
    if (!hasNext) return;
    setPageStack((s) => [...s, nextCursor]);
  }
  function onPrev() {
    if (!hasPrev) return;
    setPageStack((s) => s.slice(0, -1));
  }

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<SecretBindingSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (s) => <span className="font-medium">{s.name}</span>,
    },
    {
      id: "namespace",
      header: "Namespace",
      cell: (s) => <span className="text-muted-foreground">{s.namespace}</span>,
    },
    {
      id: "secretRef",
      header: "K8s Secret / Key",
      hideOnMobile: true,
      cell: (s) => (
        // Renders the REF (which Secret, which key) — never a credential value.
        <span className="font-mono text-xs text-muted-foreground">
          {s.secretRef.name}/{s.secretRef.key}
        </span>
      ),
    },
    {
      id: "phase",
      header: "Status",
      className: "w-32",
      cell: (s) => (
        <StatusBadge ready={s.ready} phase={s.phase} />
      ),
    },
    ...(canEdit || canDelete
      ? [
          {
            id: "actions" as const,
            header: "",
            className: "w-20 text-right",
            cell: (s: SecretBindingSummary) => (
              <RowActions
                sb={s}
                canEdit={canEdit}
                canDelete={canDelete}
                onEdit={(sb) =>
                  navigate(
                    `/secrets/${encodeURIComponent(sb.namespace)}/${encodeURIComponent(sb.name)}?edit=1`,
                  )
                }
                onDelete={(sb) =>
                  navigate(
                    `/secrets/${encodeURIComponent(sb.namespace)}/${encodeURIComponent(sb.name)}?delete=1`,
                  )
                }
              />
            ),
          },
        ]
      : []),
  ];

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Secret Bindings</h2>
          <p className="text-sm text-muted-foreground">
            How the platform references your stored provider secrets (a pointer to
            a Kubernetes Secret — never its contents). Connecting a provider
            creates one for you; this page is here for operators who want to
            inspect them directly.
          </p>
        </div>
        {canCreate && (
          <Button
            size="sm"
            onClick={() => navigate("/secrets/new")}
            data-testid="create-secret-button"
          >
            <Plus className="h-4 w-4" />
            New binding
          </Button>
        )}
      </div>

      <DataTable<SecretBindingSummary>
        columns={columns}
        rows={items}
        rowKey={(s) => `${s.namespace}/${s.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter bindings on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Secret bindings"
        onRowClick={(s) =>
          navigate(
            `/secrets/${encodeURIComponent(s.namespace)}/${encodeURIComponent(s.name)}`,
          )
        }
        empty={{
          icon: KeyRound,
          title: "No secret bindings yet",
          description: namespace
            ? `No SecretBindings in ${namespace}.`
            : "No SecretBindings visible. Create one to reference a K8s Secret.",
        }}
      />
    </div>
  );
}
