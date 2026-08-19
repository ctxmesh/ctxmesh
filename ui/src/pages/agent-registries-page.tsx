import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Pencil, Plus, Trash2, Users } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type AgentRegistrySummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_REGISTRIES } from "@/lib/nav";

// AgentRegistriesPage — list of AgentRegistries.
//
// NO egress/allowlist field anywhere in this UI — the egress NetworkPolicy is
// controller-managed and cannot be altered through the console.

const PAGE_LIMIT = 50;

function RowActions({
  registry,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  registry: AgentRegistrySummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (r: AgentRegistrySummary) => void;
  onDelete: (r: AgentRegistrySummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${registry.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label={`Edit ${registry.name}`}
          data-testid={`edit-${registry.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(registry);
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
          aria-label={`Delete ${registry.name}`}
          data-testid={`delete-${registry.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(registry);
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
  | { kind: "ready"; items: AgentRegistrySummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function AgentRegistriesPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_REGISTRIES, "create");
  const canEdit = can(RES_REGISTRIES, "update");
  const canDelete = can(RES_REGISTRIES, "delete");

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
      .listAgentRegistries(
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
          resource: "agent registries",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<AgentRegistrySummary>[] = [
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
      id: "registryId",
      header: "Registry ID",
      hideOnMobile: true,
      cell: (r) => <span className="font-mono text-xs text-muted-foreground">{r.registryId}</span>,
    },
    {
      id: "roles",
      header: "Roles",
      hideOnMobile: true,
      cell: (r) => (
        <span className="text-muted-foreground">
          {r.roles.length > 0 ? r.roles.join(", ") : "—"}
        </span>
      ),
    },
    {
      id: "phase",
      header: "Status",
      className: "w-32",
      cell: (r) => (
        <StatusBadge ready={r.ready} phase={r.phase} />
      ),
    },
    ...(canEdit || canDelete
      ? [
          {
            id: "actions" as const,
            header: "",
            className: "w-20 text-right",
            cell: (r: AgentRegistrySummary) => (
              <RowActions
                registry={r}
                canEdit={canEdit}
                canDelete={canDelete}
                onEdit={(reg) =>
                  navigate(
                    `/registries/${encodeURIComponent(reg.namespace)}/${encodeURIComponent(reg.name)}?edit=1`,
                  )
                }
                onDelete={(reg) =>
                  navigate(
                    `/registries/${encodeURIComponent(reg.namespace)}/${encodeURIComponent(reg.name)}?delete=1`,
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
          <h2 className="text-2xl font-semibold tracking-tight">Agent Registries</h2>
          <p className="text-sm text-muted-foreground">
            AgentRegistry CRDs — groups AgentDeployments with roles and member
            selectors. The filter is windowed to the loaded page.
          </p>
        </div>
        {canCreate && (
          <Button
            size="sm"
            onClick={() => navigate("/registries/new")}
            data-testid="create-registry-button"
          >
            <Plus className="h-4 w-4" />
            New registry
          </Button>
        )}
      </div>

      <DataTable<AgentRegistrySummary>
        columns={columns}
        rows={items}
        rowKey={(r) => `${r.namespace}/${r.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter registries on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Agent registries"
        onRowClick={(r) =>
          navigate(
            `/registries/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}`,
          )
        }
        empty={{
          icon: Users,
          title: "No agent registries yet",
          description: namespace
            ? `No AgentRegistries in ${namespace}.`
            : "No AgentRegistries visible. Create one to group agents into a named registry.",
        }}
      />
    </div>
  );
}
