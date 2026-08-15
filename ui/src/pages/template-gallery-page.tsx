import * as React from "react";
import { useNavigate } from "react-router-dom";
import {
  BookOpen,
  Building2,
  Check,
  Download,
  GitFork,
  Globe,
  Link2,
  Loader2,
  Lock,
  RefreshCw,
  Search,
  Store,
  Users,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  CredentialSourceBadge,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonCard,
  useFocusTrap,
  useToast,
} from "@/components/kit";
import {
  api,
  ApiError,
  type CatalogEntry,
  type McpServerSummary,
  type TemplateEntry,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";

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

// ── Card skeleton for the catalog (T12 — not SkeletonTable for a card list) ──
function CatalogSkeleton() {
  return (
    <div className="space-y-3" role="status" aria-busy="true" aria-label="Loading servers">
      {[0, 1, 2].map((i) => (
        <SkeletonCard key={i} />
      ))}
    </div>
  );
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
  // forkingKey is the unique key of the entry currently being forked (or null if none).
  // This enables a per-entry "Forking…" spinner (U12) instead of a global disable.
  forkingKey: string | null;
  // canFork gates the Fork button for viewers who lack agent-create rights (U10).
  canFork: boolean;
}

function TemplateCard({ entry, onFork, forkingKey, canFork }: TemplateCardProps) {
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

  // U12: compute the unique key for this entry to check per-entry spinner state.
  const entryKey =
    provenance && provenance !== "builtin" && provenance.originNamespace
      ? `${provenance.originNamespace}/${provenance.originName ?? entry.name}`
      : `recipe/${entry.name}`;
  const isThisEntryForking = forkingKey === entryKey;
  // Block other entries while one is in flight (still a global disable for safety).
  const isAnyForking = forkingKey !== null;

  // U12: recipe uses Download icon + "Install" verb; published uses GitFork + "Fork" verb.
  const isRecipe = entry.source === "recipe";
  const ActionIcon = isRecipe ? Download : GitFork;
  const actionLabel = isRecipe ? "Install" : "Fork";

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
              variant={isRecipe ? "secondary" : "outline"}
              className="text-[10px]"
              data-testid={`template-source-${entry.name}`}
            >
              {isRecipe ? "built-in" : "published"}
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
          {/* U10: display-gate the Fork button for viewers without create rights */}
          {canFork ? (
            <Button
              size="sm"
              onClick={() => onFork(entry)}
              disabled={isAnyForking}
              title={isAnyForking && !isThisEntryForking ? "Another fork is in progress" : undefined}
              data-testid={`fork-template-${entry.name}`}
            >
              {isThisEntryForking ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  {isRecipe ? "Installing…" : "Forking…"}
                </>
              ) : (
                <>
                  <ActionIcon className="mr-1.5 h-3.5 w-3.5" />
                  {actionLabel}
                </>
              )}
            </Button>
          ) : (
            <Button
              size="sm"
              variant="outline"
              disabled
              title="You need agent-create rights to fork"
              data-testid={`fork-template-${entry.name}`}
            >
              <ActionIcon className="mr-1.5 h-3.5 w-3.5" />
              {actionLabel}
            </Button>
          )}
        </div>
      </div>
    </li>
  );
}

// ── Rename-on-fork dialog (U11) ───────────────────────────────────────────────
// When forking hits a 409 (name collision with a different origin), offer the user
// a prompt to retry with a different name.
interface RenamePromptProps {
  defaultName: string;
  onConfirm: (name: string) => void;
  onCancel: () => void;
}

function RenameOnForkDialog({ defaultName, onConfirm, onCancel }: RenamePromptProps) {
  const [name, setName] = React.useState(defaultName + "-copy");
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onCancel });
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label="Rename fork"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onCancel}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-sm rounded-lg border bg-card p-6 shadow-overlay outline-none"
        data-testid="rename-fork-dialog"
      >
        <h2 className="text-base font-semibold">Name already taken</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          An agent named &ldquo;{defaultName}&rdquo; already exists in your namespace with a
          different origin. Choose a different name for your fork.
        </p>
        <div className="mt-3">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="New name for your fork"
            data-testid="rename-fork-input"
            autoFocus
          />
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={onCancel}>Cancel</Button>
          <Button
            onClick={() => onConfirm(name.trim())}
            disabled={!name.trim()}
            data-testid="rename-fork-confirm"
          >
            Fork as {name.trim() || "…"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function TemplatesTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { can } = useCapabilities();
  // U10: display-gate the Fork button for viewers lacking agent-create rights.
  const canFork = can(RES_AGENTS, "create");
  const [state, setState] = React.useState<TemplateState>({ kind: "loading" });
  // U12: forkingEntry tracks the unique key of the entry currently in flight for per-entry spinner.
  const [forkingEntry, setForkingEntry] = React.useState<string | null>(null);
  // U11: rename-on-fork dialog state — shown when a 409 collision is detected.
  const [renameDialog, setRenameDialog] = React.useState<{
    entry: TemplateEntry;
    defaultName: string;
  } | null>(null);

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

  // doFork is called with an optional `localName` override (U11 rename-on-fork).
  async function doFork(entry: TemplateEntry, localName?: string) {
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

    // U12: unique key uses origin ns to avoid same-name collision from different namespaces.
    const key = `${prov.originNamespace}/${prov.originName ?? entry.name}`;
    setForkingEntry(key);
    try {
      const res = await api.forkAgent(
        prov.originNamespace,
        prov.originName ?? entry.name,
        localName,
      );

      // Navigate to the FORK's own coordinates, not the origin's.
      // res.agent carries the fork's namespace + name (the caller's namespace).
      const forkNs = res.agent?.namespace ?? "";
      const forkName = res.agent?.name ?? entry.name;

      if (res.status === "already-forked") {
        // U11: link to the existing fork rather than a generic message.
        toast({
          variant: "info",
          title: "Already forked",
          description: `You already have a fork of ${entry.name}. Opening it now.`,
        });
        navigate(`/agents/${encodeURIComponent(forkNs)}/${encodeURIComponent(forkName)}`);
        return;
      }

      // U9: celebrate resolved refs (tools auto-connected via compose-connect).
      const resolvedCount = res.resolvedRefs?.length ?? 0;
      const hasDangling =
        (res.needsRebinding?.length ?? 0) > 0 ||
        (res.unresolvedRefs?.length ?? 0) > 0;

      if (hasDangling) {
        const items = [
          ...(res.needsRebinding ?? []),
          ...(res.unresolvedRefs ?? []),
        ].join(", ");
        const resolvedNote =
          resolvedCount > 0
            ? ` (${resolvedCount} tool${resolvedCount > 1 ? "s" : ""} connected automatically)`
            : "";
        toast({
          variant: "info",
          title: "Forked — needs attention",
          description: `${entry.name} was forked${resolvedNote} but has dangling references: ${items}. Open the agent to fix them.`,
        });
      } else if (resolvedCount > 0) {
        // U9: the "compounding moment" toast — all tools connected automatically.
        toast({
          variant: "success",
          title: "Forked",
          description: `${entry.name} is now in your namespace — ${resolvedCount} tool${resolvedCount > 1 ? "s" : ""} connected automatically.`,
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
      if (isConflict) {
        // U11: 409 — offer rename-on-fork instead of a dead-end error.
        setRenameDialog({ entry, defaultName: entry.name });
      } else {
        const msg = isNotFound
          ? `${entry.name} is no longer discoverable.`
          : err instanceof Error
          ? err.message
          : "fork failed";
        toast({ variant: "error", title: "Fork failed", description: msg });
      }
    } finally {
      setForkingEntry(null);
    }
  }

  async function handleFork(entry: TemplateEntry) {
    // Recipe: pre-fill the create-agent flow via ?recipe=<name>. CreateAgentPage fetches
    // the recipe list and finds the spec by name — avoids a fragile ?spec= blob in the URL
    // that URL-length limits or encoding differences could corrupt (m74 P1-2 fix).
    if (entry.source === "recipe") {
      navigate(`/agents/new?recipe=${encodeURIComponent(entry.name)}`);
      return;
    }
    await doFork(entry);
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

      {state.kind === "loading" && <CatalogSkeleton />}

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
          {state.entries.map((e) => {
            // U12: include origin namespace in the key to avoid React key collision for
            // same-named templates published from different namespaces.
            const prov = e.provenance;
            const originNs =
              prov && prov !== "builtin" && prov.originNamespace
                ? prov.originNamespace
                : "builtin";
            const cardKey = `${e.source}/${originNs}/${e.name}`;
            return (
              <TemplateCard
                key={cardKey}
                entry={e}
                onFork={handleFork}
                forkingKey={forkingEntry}
                canFork={canFork}
              />
            );
          })}
        </ul>
      )}

      {/* U11: rename-on-fork dialog — shown on 409 collision */}
      {renameDialog && (
        <RenameOnForkDialog
          defaultName={renameDialog.defaultName}
          onConfirm={(newName) => {
            const entry = renameDialog.entry;
            setRenameDialog(null);
            void doFork(entry, newName);
          }}
          onCancel={() => setRenameDialog(null)}
        />
      )}
    </div>
  );
}

// ── MCP Catalog tab ──────────────────────────────────────────────────────────
// T10: cross-checks the catalog against the caller's owned servers to render a
// "Connected ✓" disabled state for entries already in the caller's namespace.
// T11: after a successful Connect, navigates with location.state.highlight so
// McpServersPage can briefly flash the new row.
// T12: SkeletonCard (not SkeletonTable), "Connecting…" label, catalog search/filter,
// consistent "org" vocabulary.

type CatalogState =
  | { kind: "loading" }
  | { kind: "ready"; entries: CatalogEntry[]; ownedKeys: Set<string> }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

function McpCatalogTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const [state, setState] = React.useState<CatalogState>({ kind: "loading" });
  const [connectingEntry, setConnectingEntry] = React.useState<string | null>(null);
  // T12 catalog search filter
  const [search, setSearch] = React.useState("");

  const load = React.useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });

    // T10: fetch catalog AND owned servers in parallel; cross-check by name+ns.
    Promise.all([
      api.getCatalog(undefined, signal),
      api.listMcpServers(signal),
    ])
      .then(([catalogRes, ownedRes]) => {
        if (signal?.aborted) return;
        const owned = ownedRes.items ?? ownedRes.servers ?? [];
        // Build a key set of the caller's owned servers: "<ns>/<name>"
        const ownedKeys = new Set(owned.map((s: McpServerSummary) => `${s.namespace}/${s.name}`));
        setState({ kind: "ready", entries: catalogRes.entries ?? [], ownedKeys });
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
        // T11: pass the new copy's ns/name as a highlight anchor so McpServersPage
        // can flash the new row. The new copy is in the caller's own namespace (from
        // res.namespace) or falls back to the origin namespace if the BFF omits it.
        const connectedNs = res.namespace ?? entry.namespace;
        const connectedName = res.name ?? entry.name;
        navigate("/tools/mcp-servers", {
          state: { highlight: `${connectedNs}/${connectedName}` },
        });
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

  // T12: client-side filter over name/namespace/description
  const filteredEntries = React.useMemo(() => {
    if (state.kind !== "ready") return [];
    if (!search.trim()) return state.entries;
    const q = search.toLowerCase();
    return state.entries.filter(
      (e) =>
        e.name.toLowerCase().includes(q) ||
        e.namespace.toLowerCase().includes(q) ||
        (e.description ?? "").toLowerCase().includes(q),
    );
  }, [state, search]);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
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

      {/* T12: catalog search input */}
      {state.kind === "ready" && state.entries.length > 0 && (
        <div className="relative">
          <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder="Search servers…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="pl-8"
            data-testid="mcp-catalog-search"
          />
        </div>
      )}

      {/* T12: SkeletonCard instead of SkeletonTable */}
      {state.kind === "loading" && <CatalogSkeleton />}

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

      {state.kind === "ready" && state.entries.length > 0 && filteredEntries.length === 0 && (
        <p className="text-sm text-muted-foreground" data-testid="mcp-catalog-no-results">
          No servers match &ldquo;{search}&rdquo;.
        </p>
      )}

      {state.kind === "ready" && filteredEntries.length > 0 && (
        <ul className="space-y-2" data-testid="mcp-catalog-tab-list">
          {filteredEntries.map((e) => {
            const entryKey = `${e.namespace}/${e.name}`;
            // T10: cross-check against the caller's owned list by ns+name
            const isConnected = state.ownedKeys.has(entryKey);
            const isConnecting = connectingEntry === entryKey;
            return (
              <li
                key={entryKey}
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
                      {/* T8: human-label credentialSource badge */}
                      <CredentialSourceBadge credentialSource={e.credentialSource} name={e.name} />
                      {/* T10: "Connected ✓" badge for already-owned entries */}
                      {isConnected && (
                        <Badge
                          variant="success"
                          className="gap-1"
                          data-testid={`mcp-catalog-connected-${e.name}`}
                        >
                          <Check className="h-3 w-3" />
                          Connected
                        </Badge>
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
                    {/* T10: disabled state for already-connected; T12: "Connecting…" */}
                    <Button
                      size="sm"
                      onClick={() => handleConnect(e)}
                      disabled={isConnected || isConnecting}
                      data-testid={`connect-mcp-tab-${e.name}`}
                    >
                      {isConnected ? (
                        <>
                          <Check className="mr-1.5 h-3.5 w-3.5" />
                          Connected
                        </>
                      ) : (
                        <>
                          <Link2 className="mr-1.5 h-3.5 w-3.5" />
                          {isConnecting ? "Connecting…" : "Connect"}
                        </>
                      )}
                    </Button>
                  </div>
                </div>
              </li>
            );
          })}
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
          Shared servers
        </button>
      </div>

      {activeTab === "templates" && <TemplatesTab />}
      {activeTab === "mcp" && <McpCatalogTab />}
    </div>
  );
}
