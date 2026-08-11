import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Building2, Globe, Link2, Lock, RefreshCw, Store, Users } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useToast,
} from "@/components/kit";
import { api, ApiError, type CatalogEntry } from "@/lib/api";

// McpCatalogPage (m73.7) — browse discoverable MCP servers from GET /api/catalog.
// Each entry shows name, origin namespace, authType, visibility badge, tool count,
// and description. The "Connect" CTA calls POST /api/mcp/connect and on success
// redirects to /tools/mcp-servers (the materialized copy). Discovery-only: no
// secret/token is ever rendered.

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; entries: CatalogEntry[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

function VisibilityBadge({ visibility }: { visibility: string }) {
  switch (visibility) {
    case "public":
      return (
        <Badge variant="secondary" className="gap-1">
          <Globe className="h-3 w-3" />
          public
        </Badge>
      );
    case "org":
      return (
        <Badge variant="secondary" className="gap-1">
          <Building2 className="h-3 w-3" />
          org
        </Badge>
      );
    case "team":
      return (
        <Badge variant="outline" className="gap-1">
          <Users className="h-3 w-3" />
          team
        </Badge>
      );
    default:
      return (
        <Badge variant="outline" className="gap-1">
          <Lock className="h-3 w-3" />
          {visibility || "private"}
        </Badge>
      );
  }
}

function CredentialSourceBadge({ src }: { src: string | undefined }) {
  if (!src || src === "none") return null;
  if (src === "shared")
    return (
      <Badge variant="secondary" className="text-[10px]">
        shared cred
      </Badge>
    );
  if (src === "byo-oauth")
    return (
      <Badge variant="outline" className="text-[10px]">
        BYO OAuth
      </Badge>
    );
  return (
    <Badge variant="outline" className="text-[10px]">
      {src}
    </Badge>
  );
}

function AuthTypeBadge({ authType }: { authType: string | undefined }) {
  if (!authType) return <Badge variant="outline">No auth</Badge>;
  if (authType === "oauth") return <Badge variant="secondary">OAuth</Badge>;
  return <Badge variant="secondary">{authType}</Badge>;
}

interface CatalogEntryRowProps {
  entry: CatalogEntry;
  onConnect: (entry: CatalogEntry) => void;
  connecting: boolean;
}

function CatalogEntryRow({ entry, onConnect, connecting }: CatalogEntryRowProps) {
  return (
    <li
      className="rounded-lg border bg-card p-4 shadow-card"
      data-testid={`catalog-entry-${entry.name}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <p
              className="truncate font-medium"
              data-testid={`catalog-entry-name-${entry.name}`}
            >
              {entry.name}
            </p>
            <VisibilityBadge visibility={entry.visibility} />
            <AuthTypeBadge authType={entry.authType} />
            <CredentialSourceBadge src={entry.credentialSource} />
          </div>
          <p className="text-xs text-muted-foreground">
            namespace: <span className="font-mono">{entry.namespace}</span>
          </p>
          {entry.url && (
            <p className="truncate text-xs text-muted-foreground">{entry.url}</p>
          )}
          {entry.description && (
            <p className="text-sm text-muted-foreground">{entry.description}</p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Badge variant="secondary">
            {entry.toolCount} {entry.toolCount === 1 ? "tool" : "tools"}
          </Badge>
          <Button
            size="sm"
            onClick={() => onConnect(entry)}
            disabled={connecting}
            data-testid={`connect-entry-${entry.name}`}
          >
            <Link2 className="mr-1.5 h-3.5 w-3.5" />
            Connect
          </Button>
        </div>
      </div>
    </li>
  );
}

export function McpCatalogPage() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [connectingEntry, setConnectingEntry] = React.useState<string | null>(null);

  const load = React.useCallback((signal?: AbortSignal) => {
    setPage({ kind: "loading" });
    api
      .getCatalog(undefined, signal)
      .then((res) => {
        if (signal?.aborted) return;
        setPage({ kind: "ready", entries: res.entries ?? [] });
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

  async function handleConnect(entry: CatalogEntry) {
    const key = `${entry.namespace}/${entry.name}`;
    setConnectingEntry(key);
    try {
      const res = await api.connectMcpServer(entry.namespace, entry.name);
      if (res.status === "already-connected") {
        toast({
          variant: "info",
          title: "Already connected",
          description: `${entry.name} is already in your namespace.`,
        });
      } else {
        toast({
          variant: "success",
          title: "Connected",
          description: `${entry.name} is now available in your MCP servers.`,
        });
        navigate("/tools/mcp-servers");
      }
    } catch (err) {
      const msg =
        err instanceof ApiError && err.isNotFound
          ? `${entry.name} is no longer discoverable.`
          : err instanceof Error
          ? err.message
          : "connect failed";
      toast({ variant: "error", title: "Connect failed", description: msg });
    } finally {
      setConnectingEntry(null);
    }
  }

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">MCP Catalog</h2>
          <p className="text-sm text-muted-foreground">
            Discoverable MCP servers across your org. Connect one to make it available
            in your namespace — no secret is shared.
          </p>
        </div>
        <Button
          variant="ghost"
          size="icon"
          onClick={() => load()}
          aria-label="Refresh catalog"
          data-testid="mcp-catalog-refresh"
        >
          <RefreshCw className="h-4 w-4" />
        </Button>
      </div>

      {page.kind === "loading" && <SkeletonTable rows={3} />}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to browse the MCP catalog"
          description="Your account can't list the MCP catalog."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load the MCP catalog"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "ready" && page.entries.length === 0 && (
        <EmptyState
          icon={Store}
          title="No discoverable servers yet"
          description="When servers are shared team-wide, org-wide, or publicly, they appear here for you to connect."
        />
      )}

      {page.kind === "ready" && page.entries.length > 0 && (
        <ul className="space-y-2" data-testid="mcp-catalog-list">
          {page.entries.map((e) => (
            <CatalogEntryRow
              key={`${e.namespace}/${e.name}`}
              entry={e}
              onConnect={handleConnect}
              connecting={connectingEntry === `${e.namespace}/${e.name}`}
            />
          ))}
        </ul>
      )}
    </div>
  );
}
