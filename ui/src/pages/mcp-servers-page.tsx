import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ExternalLink, Plus, Trash2, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useToast,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import {
  api,
  ApiError,
  type McpServerReference,
  type McpServerSummary,
} from "@/lib/api";

// McpServersPage (m25 S10) — the LIST of registered BYO-MCP servers, with an
// "Add MCP server" action ON the page (not a separate add-only nav item). Read-open
// so a viewer sees the servers; the Add button is gated on create agentregistries.
// Each row shows the server's auth tier + tool count and links into the Tool catalog.

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; servers: McpServerSummary[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

export function McpServersPage() {
  const navigate = useNavigate();
  const { can } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");
  const canDelete = can(RES_REGISTRIES, "delete");
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  // The server currently queued for deletion (its ConfirmDialog is open).
  const [toDelete, setToDelete] = React.useState<McpServerSummary | null>(null);

  const load = React.useCallback((signal?: AbortSignal) => {
    setPage({ kind: "loading" });
    api
      .listMcpServers(signal)
      .then((res) => {
        if (signal?.aborted) return;
        setPage({ kind: "ready", servers: res.items ?? res.servers ?? [] });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setPage({ kind: "forbidden", message: err.message });
          return;
        }
        setPage({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const c = new AbortController();
    load(c.signal);
    return () => c.abort();
  }, [load]);

  const addButton = canAdd ? (
    <Button
      onClick={() => navigate("/tools/add-mcp")}
      data-testid="add-mcp-server-button"
      className="shrink-0"
    >
      <Plus className="mr-1.5 h-4 w-4" />
      Add MCP server
    </Button>
  ) : null;

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">MCP Servers</h2>
          <p className="text-sm text-muted-foreground">
            Your connected MCP servers. Each exposes tools you can attach to agents —
            browse them in the Tool catalog.
          </p>
        </div>
        {addButton}
      </div>

      {page.kind === "loading" && <SkeletonTable rows={3} />}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to list MCP servers"
          description="Your account can't list MCP servers."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load MCP servers"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "ready" && page.servers.length === 0 && (
        <EmptyState
          icon={Wrench}
          title="No MCP servers yet"
          description="Add an MCP server to give your agents tools."
          action={
            canAdd
              ? { label: "Add MCP server", icon: Plus, onClick: () => navigate("/tools/add-mcp") }
              : undefined
          }
        />
      )}

      {page.kind === "ready" && page.servers.length > 0 && (
        <ul className="space-y-2" data-testid="mcp-servers-list">
          {page.servers.map((s) => (
            <li
              key={`${s.namespace}/${s.name}`}
              className="rounded-lg border bg-card p-4 shadow-card"
              data-testid={`mcp-server-${s.name}`}
            >
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <p className="truncate font-medium">{s.name}</p>
                    {s.authType === "oauth" ? (
                      <Badge variant="secondary">OAuth</Badge>
                    ) : s.secretName ? (
                      <Badge variant="secondary">Key</Badge>
                    ) : (
                      <Badge variant="outline">No auth</Badge>
                    )}
                    {s.status === "pending" && (
                      <Badge variant="outline">Pending approval</Badge>
                    )}
                  </div>
                  <p className="truncate text-xs text-muted-foreground">{s.url}</p>
                  <p className="text-xs text-muted-foreground">{s.namespace}</p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Badge variant="secondary">
                    {s.toolCount} {s.toolCount === 1 ? "tool" : "tools"}
                  </Badge>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => navigate("/tools/catalog")}
                  >
                    <ExternalLink className="mr-1 h-3.5 w-3.5" />
                    Tools
                  </Button>
                  {canDelete && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setToDelete(s)}
                      data-testid={`delete-mcp-${s.name}`}
                      aria-label={`Delete ${s.name}`}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  )}
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}

      {toDelete && (
        <DeleteMcpDialog
          server={toDelete}
          onClose={() => setToDelete(null)}
          onDeleted={() => {
            setToDelete(null);
            load();
          }}
        />
      )}
    </div>
  );
}

// DeleteMcpDialog (m26.4) — loads the delete-impact (dependent bindings) then shows a
// typed-name ConfirmDialog. On confirm → DELETE the server bundle → reload the list.
// The dialog's impact list scrolls (the m26.1 ConfirmDialog fix), so a server with many
// dependent bindings never hides the Delete button.
type RefsLoad =
  | { kind: "loading" }
  | { kind: "ready"; references: McpServerReference[] }
  | { kind: "error"; message: string };

function DeleteMcpDialog({
  server,
  onClose,
  onDeleted,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const [refs, setRefs] = React.useState<RefsLoad>({ kind: "loading" });
  const [deleting, setDeleting] = React.useState(false);

  React.useEffect(() => {
    const c = new AbortController();
    api
      .mcpServerReferences(server.namespace, server.name, c.signal)
      .then((res) => {
        if (c.signal.aborted) return;
        setRefs({ kind: "ready", references: res.references });
      })
      .catch((err: unknown) => {
        if (c.signal.aborted) return;
        setRefs({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load delete impact",
        });
      });
    return () => c.abort();
  }, [server.namespace, server.name]);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.deleteMcpServer(server.namespace, server.name);
      toast({
        variant: "success",
        title: "MCP server deleted",
        description: `${server.name} and its tools were removed.`,
      });
      onDeleted();
    } catch (err) {
      toast({
        variant: "error",
        title: "Delete failed",
        description: err instanceof Error ? err.message : "delete failed",
      });
      setDeleting(false);
      onClose();
    }
  }

  const impact =
    refs.kind === "loading" ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-loading">
        Loading delete impact…
      </p>
    ) : refs.kind === "error" ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-error">
        Couldn't load delete impact ({refs.message}) — proceeding will still delete the
        server.
      </p>
    ) : refs.references.length === 0 ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-empty">
        No agents depend on this server's tools.
      </p>
    ) : (
      <div data-testid="mcp-refs-list">
        <p className="mb-2 text-xs font-medium text-muted-foreground">
          Deleting this server breaks {refs.references.length} bound{" "}
          {refs.references.length === 1 ? "tool" : "tools"} (they become
          RegistryNotFound):
        </p>
        <ul className="space-y-1.5">
          {refs.references.map((r) => (
            <li
              key={r.name}
              className="flex items-center justify-between gap-2 text-sm"
              data-testid={`mcp-ref-${r.name}`}
            >
              <span className="truncate font-mono text-xs">{r.name}</span>
              <Badge variant="secondary" className="text-[10px]">
                agent {r.agentRef}
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
      title={`Delete ${server.name}?`}
      description={`This permanently deletes the MCP server "${server.name}" — its catalog, credential, and egress. Bound tools break until re-pointed.`}
      confirmText={server.name}
      confirmLabel="Delete server"
      busy={deleting}
      impact={impact}
    />
  );
}
