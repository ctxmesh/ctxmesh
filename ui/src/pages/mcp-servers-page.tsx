import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ExternalLink, Plus, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EmptyState, ErrorState, ForbiddenInline, SkeletonTable } from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { api, ApiError, type McpServerSummary } from "@/lib/api";

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
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });

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
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
