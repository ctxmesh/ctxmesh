import * as React from "react";
import { useNavigate } from "react-router-dom";
import {
  BookOpen,
  Building2,
  GitFork,
  Globe,
  Link2,
  Lock,
  RefreshCw,
  Store,
  Users,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useToast,
} from "@/components/kit";
import {
  api,
  ApiError,
  type CatalogEntry,
  type TemplateEntry,
} from "@/lib/api";

// TemplateGalleryPage (m74.6) — the unified gallery surface: two tabs covering
// agent templates (recipes ∪ published agents, GET /api/templates) and MCP
// servers (GET /api/catalog). Templates carry a Fork CTA; MCP servers carry a
// Connect CTA. Client-side composition — no server union.
//
// Fork flow:
//   - recipe → pre-fill create-agent with the recipe spec (existing m72 path).
//   - published agent → POST /api/agents/{ns}/{name}/fork → toast (with
//     needs-rebinding warning if needsRebinding/unresolvedRefs non-empty)
//     + navigate to the new agent detail page.
//
// Discovery-only: no secret or credential is ever rendered.

type ActiveTab = "templates" | "mcp";

// ── Shared visibility badge ──────────────────────────────────────────────────
function VisibilityBadge({ visibility }: { visibility: string | undefined }) {
  if (!visibility) return null;
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
          {visibility}
        </Badge>
      );
  }
}

// ── Template tab ─────────────────────────────────────────────────────────────
type TemplateState =
  | { kind: "loading" }
  | { kind: "ready"; entries: TemplateEntry[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

interface TemplateCardProps {
  entry: TemplateEntry;
  onFork: (entry: TemplateEntry) => void;
  forking: boolean;
}

function TemplateCard({ entry, onFork, forking }: TemplateCardProps) {
  const provenance = entry.provenance;
  const originLabel =
    provenance === "builtin" || provenance === undefined
      ? "built-in"
      : provenance.originNamespace
      ? `${provenance.originNamespace}/${provenance.originName ?? entry.name}`
      : provenance.originName ?? entry.name;

  const versionLabel =
    provenance && provenance !== "builtin" && provenance.version
      ? provenance.version
      : null;

  return (
    <li
      className="rounded-lg border bg-card p-4 shadow-card"
      data-testid={`template-card-${entry.name}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <p
              className="truncate font-medium"
              data-testid={`template-name-${entry.name}`}
            >
              {entry.name}
            </p>
            <Badge variant="outline" className="text-[10px]">
              {entry.kind}
            </Badge>
            <Badge
              variant={entry.source === "recipe" ? "secondary" : "outline"}
              className="text-[10px]"
              data-testid={`template-source-${entry.name}`}
            >
              {entry.source === "recipe" ? "built-in" : "published"}
            </Badge>
            <VisibilityBadge visibility={entry.visibility} />
          </div>
          <p className="text-xs text-muted-foreground" data-testid={`template-origin-${entry.name}`}>
            origin: <span className="font-mono">{originLabel}</span>
            {versionLabel && (
              <span className="ml-1 text-muted-foreground">@ {versionLabel}</span>
            )}
          </p>
          {entry.description && (
            <p className="text-sm text-muted-foreground">{entry.description}</p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            onClick={() => onFork(entry)}
            disabled={forking}
            data-testid={`fork-template-${entry.name}`}
          >
            <GitFork className="mr-1.5 h-3.5 w-3.5" />
            {entry.source === "recipe" ? "Install" : "Fork"}
          </Button>
        </div>
      </div>
    </li>
  );
}

function TemplatesTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const [state, setState] = React.useState<TemplateState>({ kind: "loading" });
  const [forkingEntry, setForkingEntry] = React.useState<string | null>(null);

  const load = React.useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });
    api
      .getTemplates(undefined, signal)
      .then((entries) => {
        if (signal?.aborted) return;
        setState({ kind: "ready", entries });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setState({ kind: "forbidden", message: err.message });
          return;
        }
        setState({
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

  async function handleFork(entry: TemplateEntry) {
    // Recipe: pre-fill the create-agent flow via ?recipe=<name>. CreateAgentPage fetches
    // the recipe list and finds the spec by name — avoids a fragile ?spec= blob in the URL
    // that URL-length limits or encoding differences could corrupt (m74 P1-2 fix).
    if (entry.source === "recipe") {
      navigate(`/agents/new?recipe=${encodeURIComponent(entry.name)}`);
      return;
    }

    // Published agent: POST fork.
    const prov = entry.provenance;
    if (!prov || prov === "builtin" || !prov.originNamespace) {
      toast({
        variant: "error",
        title: "Cannot fork",
        description: "This template is missing origin information.",
      });
      return;
    }

    const key = `${prov.originNamespace}/${prov.originName ?? entry.name}`;
    setForkingEntry(key);
    try {
      const res = await api.forkAgent(prov.originNamespace, prov.originName ?? entry.name);

      // Navigate to the FORK's own coordinates, not the origin's.
      // res.agent carries the fork's namespace + name (the caller's namespace).
      const forkNs = res.agent?.namespace ?? "";
      const forkName = res.agent?.name ?? entry.name;

      if (res.status === "already-forked") {
        toast({
          variant: "info",
          title: "Already forked",
          description: `You already have a fork of ${entry.name} in your namespace.`,
        });
        navigate(`/agents/${encodeURIComponent(forkNs)}/${encodeURIComponent(forkName)}`);
        return;
      }

      const hasDangling =
        (res.needsRebinding?.length ?? 0) > 0 ||
        (res.unresolvedRefs?.length ?? 0) > 0;

      if (hasDangling) {
        const items = [
          ...(res.needsRebinding ?? []),
          ...(res.unresolvedRefs ?? []),
        ].join(", ");
        toast({
          variant: "info",
          title: "Forked — needs attention",
          description: `${entry.name} was forked but has dangling references: ${items}. Open the agent to fix them.`,
        });
      } else {
        toast({
          variant: "success",
          title: "Forked",
          description: `${entry.name} is now in your namespace.`,
        });
      }
      navigate(`/agents/${encodeURIComponent(forkNs)}/${encodeURIComponent(forkName)}`);
    } catch (err) {
      const isNotFound = err instanceof ApiError && err.isNotFound;
      const isConflict = err instanceof ApiError && err.status === 409;
      const msg = isNotFound
        ? `${entry.name} is no longer discoverable.`
        : isConflict
        ? `An agent with the name "${entry.name}" already exists in your namespace with a different origin.`
        : err instanceof Error
        ? err.message
        : "fork failed";
      toast({ variant: "error", title: "Fork failed", description: msg });
    } finally {
      setForkingEntry(null);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          Built-in recipes and published agents you can fork into your namespace.
        </p>
        <Button
          variant="ghost"
          size="icon"
          onClick={() => load()}
          aria-label="Refresh templates"
          data-testid="templates-refresh"
        >
          <RefreshCw className="h-4 w-4" />
        </Button>
      </div>

      {state.kind === "loading" && <SkeletonTable rows={3} />}

      {state.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to browse templates"
          description="Your account can't list the template gallery."
          detail={state.message}
        />
      )}

      {state.kind === "error" && (
        <ErrorState
          title="Couldn't load templates"
          description={state.message}
          onRetry={() => load()}
        />
      )}

      {state.kind === "ready" && state.entries.length === 0 && (
        <EmptyState
          icon={BookOpen}
          title="No templates yet"
          description="When agents are published as templates, they appear here for you to fork."
        />
      )}

      {state.kind === "ready" && state.entries.length > 0 && (
        <ul className="space-y-2" data-testid="template-list">
          {state.entries.map((e) => (
            <TemplateCard
              key={`${e.source}/${e.name}`}
              entry={e}
              onFork={handleFork}
              forking={forkingEntry !== null}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

// ── MCP Catalog tab (thin wrapper — reuses the data from GET /api/catalog) ──
type CatalogState =
  | { kind: "loading" }
  | { kind: "ready"; entries: CatalogEntry[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

function McpCatalogTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const [state, setState] = React.useState<CatalogState>({ kind: "loading" });
  const [connectingEntry, setConnectingEntry] = React.useState<string | null>(null);

  const load = React.useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });
    api
      .getCatalog(undefined, signal)
      .then((res) => {
        if (signal?.aborted) return;
        setState({ kind: "ready", entries: res.entries ?? [] });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setState({ kind: "forbidden", message: err.message });
          return;
        }
        setState({
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
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          Discoverable MCP servers across your org. Connect one to make it available in your namespace.
        </p>
        <Button
          variant="ghost"
          size="icon"
          onClick={() => load()}
          aria-label="Refresh MCP catalog"
          data-testid="mcp-catalog-tab-refresh"
        >
          <RefreshCw className="h-4 w-4" />
        </Button>
      </div>

      {state.kind === "loading" && <SkeletonTable rows={3} />}

      {state.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to browse the MCP catalog"
          description="Your account can't list the MCP catalog."
          detail={state.message}
        />
      )}

      {state.kind === "error" && (
        <ErrorState
          title="Couldn't load the MCP catalog"
          description={state.message}
          onRetry={() => load()}
        />
      )}

      {state.kind === "ready" && state.entries.length === 0 && (
        <EmptyState
          icon={Store}
          title="No discoverable servers yet"
          description="When servers are shared team-wide, org-wide, or publicly, they appear here for you to connect."
        />
      )}

      {state.kind === "ready" && state.entries.length > 0 && (
        <ul className="space-y-2" data-testid="mcp-catalog-tab-list">
          {state.entries.map((e) => (
            <li
              key={`${e.namespace}/${e.name}`}
              className="rounded-lg border bg-card p-4 shadow-card"
              data-testid={`mcp-catalog-entry-${e.name}`}
            >
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0 space-y-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <p className="truncate font-medium">{e.name}</p>
                    <VisibilityBadge visibility={e.visibility} />
                    {e.authType && (
                      <Badge variant="secondary">{e.authType}</Badge>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground">
                    namespace: <span className="font-mono">{e.namespace}</span>
                  </p>
                  {e.description && (
                    <p className="text-sm text-muted-foreground">{e.description}</p>
                  )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Badge variant="secondary">
                    {e.toolCount} {e.toolCount === 1 ? "tool" : "tools"}
                  </Badge>
                  <Button
                    size="sm"
                    onClick={() => handleConnect(e)}
                    disabled={connectingEntry === `${e.namespace}/${e.name}`}
                    data-testid={`connect-mcp-tab-${e.name}`}
                  >
                    <Link2 className="mr-1.5 h-3.5 w-3.5" />
                    Connect
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

// ── Page ─────────────────────────────────────────────────────────────────────
export function TemplateGalleryPage() {
  const [activeTab, setActiveTab] = React.useState<ActiveTab>("templates");

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Gallery</h2>
        <p className="text-sm text-muted-foreground">
          Start from a template — fork an agent recipe or connect an MCP server.
        </p>
      </div>

      {/* Tab bar */}
      <div className="flex gap-1 border-b" role="tablist" aria-label="Gallery sections">
        <button
          role="tab"
          aria-selected={activeTab === "templates"}
          onClick={() => setActiveTab("templates")}
          data-testid="gallery-tab-templates"
          className={`-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
            activeTab === "templates"
              ? "border-primary text-foreground"
              : "border-transparent text-muted-foreground hover:text-foreground"
          }`}
        >
          Agent Templates
        </button>
        <button
          role="tab"
          aria-selected={activeTab === "mcp"}
          onClick={() => setActiveTab("mcp")}
          data-testid="gallery-tab-mcp"
          className={`-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
            activeTab === "mcp"
              ? "border-primary text-foreground"
              : "border-transparent text-muted-foreground hover:text-foreground"
          }`}
        >
          MCP Servers
        </button>
      </div>

      {activeTab === "templates" && <TemplatesTab />}
      {activeTab === "mcp" && <McpCatalogTab />}
    </div>
  );
}
