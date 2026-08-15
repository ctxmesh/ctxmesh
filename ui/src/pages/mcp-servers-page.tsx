import * as React from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { Building2, ExternalLink, Globe, Lock, Plus, Share2, Trash2, Users, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ConfirmDialog,
  CredentialSourceBadge,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useFocusTrap,
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

// LocationState carries the highlight anchor from a post-connect navigation (T11).
interface LocationState {
  highlight?: string; // "<ns>/<name>" of the newly-connected server
}

export function McpServersPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const { can } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");
  const canDelete = can(RES_REGISTRIES, "delete");
  // Promoting a server to org scope UPDATES its ToolRegistry — the RBAC admin gate.
  const canPromote = can(RES_REGISTRIES, "update");
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  // The server currently queued for deletion (its ConfirmDialog is open).
  const [toDelete, setToDelete] = React.useState<McpServerSummary | null>(null);
  // The server queued for the unified Share flow (T5).
  const [toShare, setToShare] = React.useState<McpServerSummary | null>(null);

  // T11 — post-connect row highlight. Read once from location.state and clear so
  // a back-navigate doesn't re-trigger the flash.
  const highlightKey = (location.state as LocationState | null)?.highlight ?? null;
  const [highlighted, setHighlighted] = React.useState<string | null>(highlightKey);
  React.useEffect(() => {
    if (!highlightKey) return;
    // Clear location state so a reload doesn't re-highlight.
    window.history.replaceState({}, "");
    const t = setTimeout(() => setHighlighted(null), 2000);
    return () => clearTimeout(t);
  }, [highlightKey]);

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
          description={
            <>
              Add your own MCP server, or{" "}
              <Link to="/gallery?tab=mcp" className="underline underline-offset-2 hover:text-foreground" data-testid="gallery-discover-link">
                discover shared servers
              </Link>{" "}
              in the Gallery and connect one to your namespace.
            </>
          }
          action={
            canAdd
              ? { label: "Add MCP server", icon: Plus, onClick: () => navigate("/tools/add-mcp") }
              : undefined
          }
        />
      )}

      {page.kind === "ready" && page.servers.length > 0 && (
        <ul className="space-y-2" data-testid="mcp-servers-list">
          {page.servers.map((s) => {
            const rowKey = `${s.namespace}/${s.name}`;
            const isHighlighted = highlighted === rowKey;
            return (
              <li
                key={rowKey}
                className={[
                  "rounded-lg border bg-card p-4 shadow-card transition-colors duration-700",
                  isHighlighted ? "ring-2 ring-primary bg-primary/5" : "",
                ].join(" ")}
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
                      {s.scope && (
                        <Badge
                          variant={s.scope === "org" ? "warning" : "outline"}
                          data-testid={`scope-${s.name}`}
                        >
                          {s.scope}
                        </Badge>
                      )}
                      {s.visibility && (
                        <ServerVisibilityBadge visibility={s.visibility} name={s.name} />
                      )}
                      <CredentialSourceBadge credentialSource={s.credentialSource} name={s.name} />
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
                    {canPromote && (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setToShare(s)}
                        data-testid={`share-mcp-${s.name}`}
                        aria-label={`Share ${s.name}`}
                        title="Share"
                      >
                        <Share2 className="mr-1 h-3.5 w-3.5" />
                        Share
                      </Button>
                    )}
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
            );
          })}
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

      {toShare && (
        <ShareDialog
          server={toShare}
          onClose={() => setToShare(null)}
          onDone={() => {
            setToShare(null);
            load();
          }}
        />
      )}
    </div>
  );
}

// ServerVisibilityBadge (m73.7) — shows the m73 visibility field alongside the
// legacy scope badge in each server row.
function ServerVisibilityBadge({ visibility, name }: { visibility: string; name: string }) {
  const icon =
    visibility === "public" ? <Globe className="h-3 w-3" /> :
    visibility === "org" ? <Building2 className="h-3 w-3" /> :
    visibility === "team" ? <Users className="h-3 w-3" /> :
    <Lock className="h-3 w-3" />;
  return (
    <Badge
      variant={visibility === "public" ? "secondary" : "outline"}
      className="gap-1 text-[10px]"
      data-testid={`visibility-${name}`}
    >
      {icon}
      {visibility}
    </Badge>
  );
}

// ShareDialog (m76.2 T5) — ONE unified "Share" entry point that replaces the two
// side-by-side share controls. The user picks a mode first:
//   - "byo" (BYO-safe, recommended): publish the server definition; teammates connect
//     their OWN accounts — no credential sharing. Reveals a visibility picker.
//   - "shared-cred": share ONE org credential so every user's runs inject it.
//     Reveals a credential input. Warns clearly that this is the non-BYO path.
//
// Both backend paths are preserved; the entry point is unified (T5 fix).
//
// T7: dialogs stay OPEN on failure, showing an inline error. Only close on success.

type ShareMode = "byo" | "shared-cred";
type PublishVisibility = "team" | "org" | "public";

function ShareDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [mode, setMode] = React.useState<ShareMode>("byo");
  const [inlineError, setInlineError] = React.useState<string | null>(null);

  // BYO publish sub-state
  const defaultVisibility: PublishVisibility =
    server.visibility === "org" || server.visibility === "public" || server.visibility === "team"
      ? (server.visibility as PublishVisibility)
      : "team";
  const [selected, setSelected] = React.useState<PublishVisibility>(defaultVisibility);
  const [publicConfirmed, setPublicConfirmed] = React.useState(false);

  // Shared-cred sub-state
  const [credential, setCredential] = React.useState("");

  const [busy, setBusy] = React.useState(false);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  function handleModeChange(m: ShareMode) {
    setMode(m);
    setInlineError(null);
    if (m !== "byo") setPublicConfirmed(false);
  }

  function handleVisibilitySelect(v: PublishVisibility) {
    setSelected(v);
    if (v !== "public") setPublicConfirmed(false);
    setInlineError(null);
  }

  const isSubmitDisabled =
    busy ||
    (mode === "byo" && selected === "public" && !publicConfirmed) ||
    (mode === "shared-cred" && !credential.trim());

  async function onSubmit() {
    if (isSubmitDisabled) return;
    setBusy(true);
    setInlineError(null);

    if (mode === "byo") {
      try {
        await api.publishMcpServer(server.namespace, server.name, selected);
        toast({
          variant: "success",
          title: "Visibility updated",
          description: `${server.name} is now ${selected}-visible.`,
        });
        onDone();
      } catch (err) {
        const isForbidden = err instanceof ApiError && err.isForbidden;
        const serverMsg = err instanceof ApiError ? err.message : null;
        const fallback = isForbidden
          ? `You need org-admin rights to publish ${selected}-wide.`
          : err instanceof Error
          ? err.message
          : "publish failed";
        // T7: stay open with inline error; use server message when available (T12)
        setInlineError(serverMsg || fallback);
        setBusy(false);
      }
    } else {
      // shared-cred mode
      const cred = credential;
      setCredential(""); // wipe before the round-trip — the secret never lingers in state
      try {
        await api.setOrgCredential({
          server: server.name,
          namespace: server.namespace,
          credential: cred,
        });
        toast({
          variant: "success",
          title: "Org credential set",
          description: `${server.name} is now shared org-wide.`,
        });
        onDone();
      } catch (err) {
        const serverMsg = err instanceof ApiError ? err.message : null;
        const fallback = err instanceof Error ? err.message : "failed";
        // T7: stay open with inline error; use server message when available (T12)
        setInlineError(serverMsg || fallback);
        // Restore credential so user can fix without re-pasting
        setCredential(cred);
        setBusy(false);
      }
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${server.name}`}
      data-testid="share-dialog"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-lg rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">Share {server.name}</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Choose how teammates can access this server.
        </p>
        {server.visibility && (
          <p className="mt-1 text-xs text-muted-foreground" data-testid="share-current-visibility">
            Currently: <span className="font-medium">{server.visibility}</span>
          </p>
        )}

        {/* Mode picker */}
        <div className="mt-4 space-y-2">
          <label
            className="flex cursor-pointer items-start gap-3 rounded-md border p-3 hover:bg-accent/40"
            data-testid="share-mode-byo"
          >
            <input
              type="radio"
              name="share-mode"
              value="byo"
              checked={mode === "byo"}
              onChange={() => handleModeChange("byo")}
              className="mt-0.5 accent-primary"
              data-testid="share-mode-byo-radio"
            />
            <div>
              <p className="font-medium">Teammates connect their own accounts <span className="text-xs text-muted-foreground font-normal">(recommended)</span></p>
              <p className="text-xs text-muted-foreground">
                Publish the server definition — teammates discover it and connect their OWN accounts. Your credentials are never shared.
              </p>
            </div>
          </label>

          <label
            className="flex cursor-pointer items-start gap-3 rounded-md border p-3 hover:bg-accent/40"
            data-testid="share-mode-shared-cred"
          >
            <input
              type="radio"
              name="share-mode"
              value="shared-cred"
              checked={mode === "shared-cred"}
              onChange={() => handleModeChange("shared-cred")}
              className="mt-0.5 accent-primary"
              data-testid="share-mode-shared-cred-radio"
            />
            <div>
              <p className="font-medium">Share one credential everyone uses</p>
              <p className="text-xs text-muted-foreground">
                Set one shared org credential — every user&apos;s runs inject it automatically, no per-user connect.
              </p>
            </div>
          </label>
        </div>

        {/* BYO sub-form: visibility picker */}
        {mode === "byo" && (
          <div className="mt-4 space-y-2" data-testid="share-byo-section">
            <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Visibility</p>
            {(["team", "org", "public"] as PublishVisibility[]).map((v) => (
              <label
                key={v}
                className="flex cursor-pointer items-center gap-3 rounded-md border p-3 hover:bg-accent/40"
                data-testid={`publish-option-${v}`}
              >
                <input
                  type="radio"
                  name="visibility"
                  value={v}
                  checked={selected === v}
                  onChange={() => handleVisibilitySelect(v)}
                  className="accent-primary"
                />
                <div>
                  <p className="font-medium capitalize">{v}</p>
                  <p className="text-xs text-muted-foreground">
                    {v === "team"
                      ? "Visible to your team's namespace"
                      : v === "org"
                      ? "Visible org-wide (org-admin required)"
                      : "Visible to everyone (Platform-admin required)"}
                  </p>
                </div>
              </label>
            ))}
            {selected === "public" && (
              <div className="mt-2 space-y-2">
                <p className="text-sm text-amber-600 dark:text-amber-400" data-testid="publish-public-warning">
                  Public means every tenant on this cluster can discover it.
                </p>
                <label className="flex cursor-pointer items-center gap-2 text-sm" data-testid="publish-public-confirm-label">
                  <input
                    type="checkbox"
                    checked={publicConfirmed}
                    onChange={(e) => setPublicConfirmed(e.target.checked)}
                    className="accent-primary"
                    data-testid="publish-public-confirm"
                  />
                  I understand this is discoverable by all tenants
                </label>
              </div>
            )}
            {server.credentialSource === "shared" && (
              <p className="mt-2 text-sm text-amber-600 dark:text-amber-400" data-testid="publish-shared-cred-warning">
                Caution: this server uses a shared credential — widening visibility also widens
                access to that credential.
              </p>
            )}
          </div>
        )}

        {/* Shared-cred sub-form: credential input */}
        {mode === "shared-cred" && (
          <div className="mt-4 space-y-3" data-testid="share-shared-cred-section">
            <p className="text-sm text-amber-600 dark:text-amber-400" data-testid="org-cred-caution">
              This shares ONE credential with everyone — every user&apos;s runs use it. If teammates
              should connect their own accounts instead, choose the option above.
            </p>
            <div className="space-y-1.5">
              <Label htmlFor="org-cred">Shared credential (bearer token)</Label>
              <Input
                id="org-cred"
                type="password"
                autoComplete="off"
                value={credential}
                onChange={(e) => setCredential(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    void onSubmit();
                  }
                }}
                placeholder="paste the org's token"
                data-testid="org-cred-input"
              />
            </div>
          </div>
        )}

        {/* T7: inline error — stays visible without closing the dialog */}
        {inlineError && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="share-inline-error"
          >
            {inlineError}
          </p>
        )}

        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={isSubmitDisabled}
            data-testid="share-submit"
          >
            {busy
              ? mode === "byo"
                ? "Sharing…"
                : "Setting…"
              : mode === "byo"
              ? "Share"
              : "Set org credential"}
          </Button>
        </div>
      </div>
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
