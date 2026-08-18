import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { GitBranch, Pencil, Plus, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type ModelRouteSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_ROUTES } from "@/lib/nav";

// ModelRoutesPage — list of ModelRoutes with the standard DataTable list contract.
// Row-click → detail/edit/delete surface (/routes/{ns}/{name}).
// Create button in the header navigates to /routes/new.
// RBAC-aware: create/edit/delete affordances gated on modelroutes/create,
// update, delete. Viewers see a clean read-only table.

const PAGE_LIMIT = 50;

function RowActions({
  route,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  route: ModelRouteSummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (r: ModelRouteSummary) => void;
  onDelete: (r: ModelRouteSummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${route.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label={`Edit ${route.name}`}
          data-testid={`edit-${route.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(route);
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
          aria-label={`Delete ${route.name}`}
          data-testid={`delete-${route.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(route);
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
  | { kind: "ready"; items: ModelRouteSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function ModelRoutesPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_ROUTES, "create");
  const canEdit = can(RES_ROUTES, "update");
  const canDelete = can(RES_ROUTES, "delete");

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
      .listModelRoutes(
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

  const columns: Column<ModelRouteSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (r) => <span className="font-medium">{r.name}</span>,
    },
    {
      id: "namespace",
      header: "Namespace",
      cell: (r) => <span className="text-muted-foreground">{r.namespace}</span>,
    },
    {
      id: "providers",
      header: "Providers",
      hideOnMobile: true,
      cell: (r) => (
        <span className="text-muted-foreground">
          {r.providers.map((p) => `${p.provider}/${p.model}`).join(", ") || "—"}
        </span>
      ),
    },
    {
      id: "phase",
      header: "Status",
      className: "w-32",
      cell: (r) => (
        <Badge variant={r.ready ? "success" : "warning"}>
          {r.phase || (r.ready ? "Ready" : "Pending")}
        </Badge>
      ),
    },
    ...(canEdit || canDelete
      ? [
          {
            id: "actions" as const,
            header: "",
            className: "w-20 text-right",
            cell: (r: ModelRouteSummary) => (
              <RowActions
                route={r}
                canEdit={canEdit}
                canDelete={canDelete}
                onEdit={(route) =>
                  navigate(
                    `/routes/${encodeURIComponent(route.namespace)}/${encodeURIComponent(route.name)}?edit=1`,
                  )
                }
                onDelete={(route) =>
                  navigate(
                    `/routes/${encodeURIComponent(route.namespace)}/${encodeURIComponent(route.name)}?delete=1`,
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
          <h2 className="text-2xl font-semibold tracking-tight">Model Routes</h2>
          <p className="text-sm text-muted-foreground">
            The provider bindings behind your models. The platform creates one
            automatically when you connect a provider and pick a model; inspect
            or hand-author routing here.
          </p>
        </div>
        {canCreate && (
          <Button
            size="sm"
            onClick={() => navigate("/routes/new")}
            data-testid="create-route-button"
          >
            <Plus className="h-4 w-4" />
            New route
          </Button>
        )}
      </div>

      <DataTable<ModelRouteSummary>
        columns={columns}
        rows={items}
        rowKey={(r) => `${r.namespace}/${r.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter routes on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Model routes"
        onRowClick={(r) =>
          navigate(
            `/routes/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}`,
          )
        }
        empty={{
          icon: GitBranch,
          title: "No model routes yet",
          description: namespace
            ? `No ModelRoutes in ${namespace}. Create one to route AI calls to a provider.`
            : "No ModelRoutes visible. Create one to route AI calls to a provider.",
        }}
      />
    </div>
  );
}
