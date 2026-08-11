import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Building2, ExternalLink, Globe, Lock, Plus, Share2, Trash2, Users, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ConfirmDialog,
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

export function McpServersPage() {
  const navigate = useNavigate();
  const { can } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");
  const canDelete = can(RES_REGISTRIES, "delete");
  // Promoting a server to org scope UPDATES its ToolRegistry — the RBAC admin gate.
  const canPromote = can(RES_REGISTRIES, "update");
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  // The server currently queued for deletion (its ConfirmDialog is open).
  const [toDelete, setToDelete] = React.useState<McpServerSummary | null>(null);
  // The server currently being shared org-wide (its org-credential dialog is open).
  const [toOrg, setToOrg] = React.useState<McpServerSummary | null>(null);
  // The server queued for a visibility publish action.
  const [toPublish, setToPublish] = React.useState<McpServerSummary | null>(null);

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
                    {s.credentialSource && s.credentialSource !== "none" && (
                      <Badge
                        variant="outline"
                        className="text-[10px]"
                        data-testid={`cred-source-${s.name}`}
                      >
                        {s.credentialSource}
                      </Badge>
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
                  {canPromote && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setToOrg(s)}
                      data-testid={`org-cred-${s.name}`}
                      aria-label={`Share ${s.name} with your org`}
                    >
                      <Users className="h-3.5 w-3.5" />
                    </Button>
                  )}
                  {canPromote && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setToPublish(s)}
                      data-testid={`publish-mcp-${s.name}`}
                      aria-label={`Publish ${s.name}`}
                    >
                      <Share2 className="h-3.5 w-3.5" />
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

      {toOrg && (
        <SetOrgCredentialDialog
          server={toOrg}
          onClose={() => setToOrg(null)}
          onDone={() => {
            setToOrg(null);
            load();
          }}
        />
      )}

      {toPublish && (
        <PublishDialog
          server={toPublish}
          onClose={() => setToPublish(null)}
          onDone={() => {
            setToPublish(null);
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

// SetOrgCredentialDialog (m26.5, ADR 0029 §7) — promote a server to ORG scope and set
// its shared credential (the fully-headless path: one admin-set credential every user's
// runs inject, no per-user consent). The credential is a password-type input, wiped from
// state before the round-trip, sent ONLY in the request body → a Secret server-side; it
// is never displayed, logged, or persisted client-side.
function SetOrgCredentialDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [credential, setCredential] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  async function onSet() {
    if (!credential.trim() || busy) return;
    setBusy(true);
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
      toast({
        variant: "error",
        title: "Couldn't set org credential",
        description: err instanceof Error ? err.message : "failed",
      });
      setBusy(false);
      onClose();
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${server.name} with your org`}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">
          Share {server.name} with your org
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Promote this server to <span className="font-medium">org</span> scope and set one
          shared credential. Every user&apos;s runs then use it — no per-user connect. The
          credential is stored server-side only.
        </p>
        <div className="mt-4 space-y-1.5">
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
                void onSet();
              }
            }}
            placeholder="paste the org's token"
            data-testid="org-cred-input"
          />
        </div>
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSet()}
            disabled={!credential.trim() || busy}
            data-testid="org-cred-submit"
          >
            {busy ? "Setting…" : "Set org credential"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// PublishDialog (m73.7) — widens a server's visibility. Pick team/org/public →
// calls POST /api/mcp/publish → on success reloads the list; on 403 surfaces the
// tier requirement honestly. Do NOT auto-publish — this is an explicit user action.
type PublishVisibility = "team" | "org" | "public";

function PublishDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [selected, setSelected] = React.useState<PublishVisibility>("team");
  const [busy, setBusy] = React.useState(false);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  async function onPublish() {
    if (busy) return;
    setBusy(true);
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
      toast({
        variant: "error",
        title: "Publish failed",
        description: isForbidden
          ? `You need ${
              selected === "public"
                ? "Platform-admin"
                : selected === "org"
                ? "Tenant-admin"
                : "team-admin"
            } rights to publish ${selected}-wide.`
          : err instanceof Error
          ? err.message
          : "publish failed",
      });
      setBusy(false);
      onClose();
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Publish ${server.name}`}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">Publish {server.name}</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Widen this server&apos;s visibility. Requires the matching role — a 403 will
          tell you which role is needed.
        </p>
        <div className="mt-4 space-y-2">
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
                onChange={() => setSelected(v)}
                className="accent-primary"
              />
              <div>
                <p className="font-medium capitalize">{v}</p>
                <p className="text-xs text-muted-foreground">
                  {v === "team"
                    ? "Visible to your team's namespace"
                    : v === "org"
                    ? "Visible org-wide (Tenant-admin required)"
                    : "Visible to everyone (Platform-admin required)"}
                </p>
              </div>
            </label>
          ))}
        </div>
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onPublish()}
            disabled={busy}
            data-testid="publish-submit"
          >
            {busy ? "Publishing…" : `Publish as ${selected}`}
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
