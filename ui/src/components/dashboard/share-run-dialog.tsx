import * as React from "react";
import { Copy, Check, Share2, Trash2, X, ChevronDown, ChevronUp } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useFocusTrap } from "@/components/kit/use-focus-trap";
import { useToast } from "@/components/kit";
import { api, ApiError, type RunShare, type CreateRunShareResponse, type SharedRunView } from "@/lib/api";

// ShareRunDialog — the run share story (m75.4).
// Share action → redaction-honest preview of the projection → create → show link once.
// Also surfaces manage/revoke for existing shares.
//
// data-testid contract:
//   share-run-dialog             — root modal panel
//   share-include-content        — the transcript toggle checkbox
//   share-projection-preview     — the projection preview block
//   share-preview-expander       — the "Preview what will be shared" toggle (V9)
//   share-preview-content        — the expanded real-projection content (V9)
//   share-ttl-select             — the TTL picker
//   share-create-btn             — the Create Share Link button
//   share-link-once              — the one-time link display block
//   share-link-copy              — the copy button for the link
//   share-link-value             — the link text element (for test reading)
//   share-link-done              — the "I've saved the link" confirmation button (V8)
//   share-manage-section         — the manage/revoke section
//   share-row-{id}               — one existing share row
//   share-revoke-{id}            — the revoke button for a specific share
//   share-expired-badge-{id}     — expired badge on a share row (V11)
//   share-revoked-badge-{id}     — revoked badge on a share row (V11)

export interface ShareRunDialogProps {
  open: boolean;
  onClose: () => void;
  runId: string;
  // Optional: run metadata for future use — no longer used in the preview
  // (V7: we dropped fake parenthesized numbers; V9 fetches the real projection)
  runData?: {
    agent?: string;
    namespace?: string;
    status?: string;
    messageCount?: number;
    messageRoles?: string[];
    errorCategory?: string;
  };
  // canShare: when false the dialog explains shares can't be minted (V12: MemStore / no store).
  // The trace page omits this and the dialog is fully operational; set to false when the
  // Share button knows the store is absent (e.g. after a 501 response).
  canShare?: boolean;
}

type CreateState =
  | { kind: "idle" }
  | { kind: "creating" }
  | { kind: "done"; result: CreateRunShareResponse; link: string; linkSaved: boolean }
  | { kind: "error"; message: string; isNotImplemented?: boolean; isConflict?: boolean };

type ManageState =
  | { kind: "loading" }
  | { kind: "ready"; shares: RunShare[] }
  | { kind: "error"; message: string };

// PreviewState: the V9 real-projection expander state.
type PreviewState =
  | { kind: "idle" }          // not expanded yet
  | { kind: "loading" }
  | { kind: "ready"; view: SharedRunView }
  | { kind: "error" };

const TTL_OPTIONS = [
  { label: "24 hours", hours: 24 },
  { label: "7 days", hours: 168 },
  { label: "30 days", hours: 720 },
  { label: "90 days", hours: 2160 },
];

function fmtExpiry(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

export function ShareRunDialog({ open, onClose, runId, canShare = true }: ShareRunDialogProps) {
  const [includeContent, setIncludeContent] = React.useState(false);
  const [ttlHours, setTtlHours] = React.useState(168);
  const [createState, setCreateState] = React.useState<CreateState>({ kind: "idle" });
  const [manageState, setManageState] = React.useState<ManageState>({ kind: "loading" });
  const [revoking, setRevoking] = React.useState<Set<string>>(new Set());
  const [copied, setCopied] = React.useState(false);
  // V9: preview expander state — fetches the real projection on demand.
  const [previewState, setPreviewState] = React.useState<PreviewState>({ kind: "idle" });
  const [previewOpen, setPreviewOpen] = React.useState(false);
  const { toast } = useToast();
  const titleId = React.useId();

  // V8: guard dismissal in the done state — the link is shown once; a stray backdrop
  // click / Escape before confirmation would destroy it forever.
  const isDone = createState.kind === "done";
  const canDismiss = !isDone || (isDone && createState.linkSaved);
  const guardedOnClose = React.useCallback(() => {
    if (canDismiss) onClose();
  }, [canDismiss, onClose]);

  const panelRef = useFocusTrap<HTMLDivElement>({ active: open, onEscape: guardedOnClose });

  // Load existing shares on open (includes revoked rows for honest lifecycle — V11).
  React.useEffect(() => {
    if (!open) return;
    setManageState({ kind: "loading" });
    setCreateState({ kind: "idle" });
    setPreviewState({ kind: "idle" });
    setPreviewOpen(false);
    const controller = new AbortController();
    api.listRunShares(runId, controller.signal)
      .then((shares) => setManageState({ kind: "ready", shares }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setManageState({
          kind: "error",
          message: err instanceof Error ? err.message : "failed to load shares",
        });
      });
    return () => controller.abort();
  }, [open, runId]);

  // V9: fetch the real projection when the preview expander opens, using the token from the
  // just-created share. The plaintext token is shown to the sharer ONCE (the store keeps only a
  // hash), so the client only holds it right after minting — that is why the preview is available
  // then and not on a later reopen. It is NOT because the link is single-use: a share link is
  // multi-fetch until it expires or is revoked (SharedRun.IsLive, m75.2 / V14).
  async function onTogglePreview() {
    const next = !previewOpen;
    setPreviewOpen(next);
    if (!next) return;
    // Only fetch if we hold the freshly-minted token in memory (shown once). Absent it, the preview
    // shows the structural field list only — the link still works, we just can't re-derive the token.
    if (createState.kind !== "done") {
      return;
    }
    if (previewState.kind === "ready" || previewState.kind === "loading") return;
    setPreviewState({ kind: "loading" });
    try {
      const token = createState.result.token;
      const view = await api.getSharedRun(token);
      setPreviewState({ kind: "ready", view });
    } catch {
      setPreviewState({ kind: "error" });
    }
  }

  async function onCreate() {
    setCreateState({ kind: "creating" });
    try {
      const result = await api.createRunShare(runId, includeContent, ttlHours);
      const link = `${window.location.origin}/shared/runs/${result.token}`;
      setCreateState({ kind: "done", result, link, linkSaved: false });
      // Refresh the manage list
      api.listRunShares(runId).then((shares) =>
        setManageState({ kind: "ready", shares })
      ).catch(() => {});
    } catch (err) {
      // V12: soften 501 (store not configured) and 409 (not durable) errors.
      const isNotImplemented = err instanceof ApiError && err.isNotImplemented;
      const isConflict = err instanceof ApiError && err.status === 409;
      setCreateState({
        kind: "error",
        message: err instanceof Error ? err.message : "failed to create share",
        isNotImplemented,
        isConflict,
      });
    }
  }

  async function onRevoke(shareId: string) {
    setRevoking((prev) => new Set(prev).add(shareId));
    try {
      await api.revokeRunShare(runId, shareId);
      setManageState((prev) => {
        if (prev.kind !== "ready") return prev;
        return {
          kind: "ready",
          shares: prev.shares.map((s) =>
            s.id === shareId ? { ...s, revoked: true } : s
          ),
        };
      });
      toast({ variant: "success", title: "Share revoked" });
    } catch (err) {
      toast({
        variant: "error",
        title: "Revoke failed",
        description: err instanceof Error ? err.message : "could not revoke share",
      });
    } finally {
      setRevoking((prev) => {
        const next = new Set(prev);
        next.delete(shareId);
        return next;
      });
    }
  }

  async function onCopy(link: string) {
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast({ variant: "error", title: "Could not copy — please copy manually" });
    }
  }

  if (!open) return null;

  // V7: projection preview — honest field LIST only; no fake parenthesized numbers.
  // The real values come from the server's newSharedRunView allowlist (V9 previews them).
  const metadataFields = [
    "Agent name and namespace",
    "Run status",
    "Created and updated timestamps",
    "Message count and roles",
    "Error category (if any)",
  ];
  const contentFields = [
    "Full input text",
    "Full message transcript (all turns)",
    "Full error text (if any)",
  ];

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
    >
      {/* V8: backdrop click is blocked in the done state until the link is confirmed saved. */}
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={guardedOnClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        data-testid="share-run-dialog"
        className="relative flex max-h-[90vh] w-full max-w-lg flex-col overflow-y-auto rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        {/* Header */}
        <div className="flex shrink-0 items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <Share2 className="h-5 w-5 text-muted-foreground" />
            <h2 id={titleId} className="text-lg font-semibold tracking-snug">
              Share this run
            </h2>
          </div>
          {/* V8: close button is disabled while in done-but-unconfirmed state. */}
          <button
            onClick={guardedOnClose}
            disabled={!canDismiss}
            className="rounded p-1 text-muted-foreground hover:text-foreground disabled:opacity-40 disabled:cursor-not-allowed"
            aria-label={canDismiss ? "Close" : "Copy or confirm the link before closing"}
            title={canDismiss ? undefined : "Copy or confirm the link before closing"}
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        {/* Create section */}
        {createState.kind !== "done" && (
          <div className="mt-5 space-y-5">
            {/* V12: gate Share UI when canShare=false (MemStore run / no store). */}
            {!canShare && (
              <div className="rounded-md border border-muted bg-muted/20 p-3 text-xs text-muted-foreground">
                Sharing is not available for this run. Runs must be durably stored to be shared.
              </div>
            )}

            {canShare && (
              <>
                {/* Transcript toggle */}
                <div className="flex items-start gap-3">
                  <input
                    id="include-content"
                    type="checkbox"
                    data-testid="share-include-content"
                    checked={includeContent}
                    onChange={(e) => setIncludeContent(e.target.checked)}
                    className="mt-0.5 h-4 w-4 rounded border-border accent-primary"
                  />
                  <label htmlFor="include-content" className="cursor-pointer space-y-0.5">
                    <span className="text-sm font-medium">Include transcript (input + messages + error)</span>
                    <p className="text-xs text-muted-foreground">
                      Off by default. When on, the full conversation is public — no login required.
                    </p>
                  </label>
                </div>

                {/* V7: Projection preview — honest field LIST only, no fake numbers. */}
                <div
                  data-testid="share-projection-preview"
                  className="rounded-md border bg-surface-2/60 p-3 text-xs"
                >
                  <p className="font-medium text-foreground">Public viewers will see:</p>
                  <ul className="mt-2 space-y-1 text-muted-foreground">
                    {metadataFields.map((f) => (
                      <li key={f} className="flex items-start gap-1.5">
                        <span className="mt-0.5 text-success">✓</span>
                        <span>{f}</span>
                      </li>
                    ))}
                    {includeContent && contentFields.map((f) => (
                      <li key={f} className="flex items-start gap-1.5">
                        <span className="mt-0.5 text-warning">+</span>
                        <span>{f}</span>
                      </li>
                    ))}
                  </ul>
                  {!includeContent && (
                    <p className="mt-2 text-muted-foreground">
                      Input, messages, and error text are <strong>not included</strong> (metadata-only).
                    </p>
                  )}
                </div>

                {/* TTL picker */}
                <div className="flex items-center gap-3">
                  <label htmlFor="share-ttl" className="text-sm font-medium whitespace-nowrap">
                    Expires after
                  </label>
                  <select
                    id="share-ttl"
                    data-testid="share-ttl-select"
                    value={ttlHours}
                    onChange={(e) => setTtlHours(Number(e.target.value))}
                    className="h-8 rounded-md border border-border bg-background px-2 text-sm"
                  >
                    {TTL_OPTIONS.map((o) => (
                      <option key={o.hours} value={o.hours}>{o.label}</option>
                    ))}
                  </select>
                </div>

                {createState.kind === "error" && (
                  /* V12: soften 501 / 409 errors — show calm explanatory text instead of raw ops message. */
                  createState.isNotImplemented ? (
                    <p className="text-sm text-muted-foreground" role="alert">
                      Share links are not configured on this installation — set{" "}
                      <span className="font-mono">CONTROLPLANE_DSN</span> to enable them.
                    </p>
                  ) : createState.isConflict ? (
                    <p className="text-sm text-muted-foreground" role="alert">
                      This run is not durably stored and cannot be shared. Set{" "}
                      <span className="font-mono">RUN_STORE_DSN</span> to enable durable run storage.
                    </p>
                  ) : (
                    <p className="text-sm text-destructive" role="alert">{createState.message}</p>
                  )
                )}

                <Button
                  data-testid="share-create-btn"
                  onClick={() => void onCreate()}
                  disabled={createState.kind === "creating"}
                  className="w-full"
                >
                  {createState.kind === "creating" ? "Creating…" : "Create share link"}
                </Button>
              </>
            )}
          </div>
        )}

        {/* One-time link display */}
        {createState.kind === "done" && (
          <div
            data-testid="share-link-once"
            className="mt-5 space-y-4 rounded-md border border-success/40 bg-success/5 p-4"
          >
            <div>
              <p className="text-sm font-medium text-success">Share link created</p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                This is the only time you'll see this link. Copy it now.
              </p>
            </div>
            <div className="flex items-center gap-2 rounded-md border bg-card p-2">
              <span
                data-testid="share-link-value"
                className="min-w-0 flex-1 truncate font-mono text-xs"
              >
                {createState.link}
              </span>
              <Button
                data-testid="share-link-copy"
                variant="outline"
                size="sm"
                onClick={() => void onCopy(createState.link)}
                className="shrink-0"
              >
                {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                {copied ? "Copied" : "Copy"}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              Expires: {fmtExpiry(createState.result.expiresAt)}
              {" · "}
              {createState.result.includeContent ? "Includes transcript" : "Metadata only"}
            </p>
            {/* V14: the real link semantics — multi-fetch until expiry/revoke (no single-use marking;
                the store keeps only a hash). Shown so a sharer isn't misled into thinking a preview or
                a first open "burns" the link. */}
            <p className="text-xs text-muted-foreground" data-testid="share-link-semantics">
              Anyone with this link can open it as many times as they like until it expires or you
              revoke it.
            </p>

            {/* V9: "Preview what will be shared" expander — shows the real projection. */}
            <div className="border-t pt-3">
              <button
                data-testid="share-preview-expander"
                type="button"
                onClick={() => void onTogglePreview()}
                className="flex w-full items-center justify-between text-xs text-muted-foreground hover:text-foreground"
              >
                <span>Preview what will be shared</span>
                {previewOpen ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
              </button>
              {previewOpen && (
                <div data-testid="share-preview-content" className="mt-2 space-y-2 text-xs">
                  {previewState.kind === "loading" && (
                    <p className="text-muted-foreground">Loading preview…</p>
                  )}
                  {previewState.kind === "error" && (
                    <p className="text-muted-foreground">Preview unavailable right now — try again in a moment.</p>
                  )}
                  {previewState.kind === "idle" && (
                    <p className="text-muted-foreground">
                      Preview shows the actual content that will be public. The link itself keeps working for anyone
                      who has it until it expires or you revoke it.
                    </p>
                  )}
                  {previewState.kind === "ready" && (
                    <div className="space-y-2 rounded-md border bg-card p-3">
                      <div className="grid grid-cols-2 gap-x-4 gap-y-1">
                        <span className="text-muted-foreground">Agent</span>
                        <span className="font-mono">{previewState.view.agent}</span>
                        <span className="text-muted-foreground">Namespace</span>
                        <span className="font-mono">{previewState.view.namespace}</span>
                        <span className="text-muted-foreground">Status</span>
                        <span className="capitalize">{previewState.view.status}</span>
                        <span className="text-muted-foreground">Messages</span>
                        <span>{previewState.view.messageCount}
                          {previewState.view.messageRoles.length > 0 && (
                            <span className="ml-1 text-muted-foreground">
                              ({previewState.view.messageRoles.join(", ")})
                            </span>
                          )}
                        </span>
                      </div>
                      {previewState.view.input !== undefined && (
                        <div>
                          <p className="text-muted-foreground mb-1">Input</p>
                          <pre className="max-h-24 overflow-auto whitespace-pre-wrap rounded bg-muted/30 p-2 font-mono text-xs">
                            {typeof previewState.view.input === "string"
                              ? previewState.view.input
                              : JSON.stringify(previewState.view.input, null, 2)}
                          </pre>
                        </div>
                      )}
                      {previewState.view.messages && previewState.view.messages.length > 0 && (
                        <div>
                          <p className="text-muted-foreground mb-1">Transcript ({previewState.view.messages.length} messages)</p>
                          <div className="max-h-32 overflow-auto space-y-1">
                            {previewState.view.messages.map((msg, i) => (
                              <div key={i} className="rounded bg-muted/30 p-1.5">
                                <span className="font-medium capitalize text-muted-foreground">{msg.role}: </span>
                                <span className="line-clamp-2">{msg.content}</span>
                              </div>
                            ))}
                          </div>
                        </div>
                      )}
                    </div>
                  )}
                </div>
              )}
            </div>

            {/* V8: "I've saved the link" confirmation — allows the dialog to be closed once confirmed. */}
            {!createState.linkSaved && (
              <Button
                data-testid="share-link-done"
                variant="outline"
                size="sm"
                className="w-full"
                onClick={() => setCreateState((prev) =>
                  prev.kind === "done" ? { ...prev, linkSaved: true } : prev
                )}
              >
                <Check className="h-3.5 w-3.5" />
                I&apos;ve saved the link
              </Button>
            )}
            {createState.linkSaved && (
              <p className="text-center text-xs text-muted-foreground">Link confirmed — you can close this dialog.</p>
            )}
          </div>
        )}

        {/* Manage / revoke section (V11: shows revoked rows with badge; adds Expired badge). */}
        <div data-testid="share-manage-section" className="mt-6">
          <h3 className="text-sm font-medium text-muted-foreground">Share history</h3>
          <div className="mt-2">
            {manageState.kind === "loading" && (
              <p className="text-xs text-muted-foreground">Loading…</p>
            )}
            {manageState.kind === "error" && (
              <p className="text-xs text-destructive">{manageState.message}</p>
            )}
            {manageState.kind === "ready" && manageState.shares.length === 0 && (
              <p className="text-xs text-muted-foreground">No shares yet.</p>
            )}
            {manageState.kind === "ready" && manageState.shares.map((share) => {
              const isExpired = !share.revoked && new Date(share.expiresAt) <= new Date();
              const isActive = !share.revoked && !isExpired;
              return (
                <div
                  key={share.id}
                  data-testid={`share-row-${share.id}`}
                  className="flex items-center gap-3 rounded-md border p-2 mt-2 text-xs"
                >
                  <div className="min-w-0 flex-1 space-y-0.5">
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className={!isActive ? "text-muted-foreground line-through" : "text-foreground"}>
                        {share.includeContent ? "With transcript" : "Metadata only"}
                      </span>
                      {share.revoked && (
                        <span
                          data-testid={`share-revoked-badge-${share.id}`}
                          className="rounded bg-destructive/15 px-1 text-destructive text-xs"
                        >
                          Revoked
                        </span>
                      )}
                      {isExpired && (
                        <span
                          data-testid={`share-expired-badge-${share.id}`}
                          className="rounded bg-muted px-1 text-muted-foreground text-xs"
                        >
                          Expired
                        </span>
                      )}
                    </div>
                    <p className="text-muted-foreground">
                      Created {fmtExpiry(share.createdAt)} · Expires {fmtExpiry(share.expiresAt)}
                    </p>
                  </div>
                  {isActive && (
                    <Button
                      data-testid={`share-revoke-${share.id}`}
                      variant="ghost"
                      size="sm"
                      className="shrink-0 text-destructive hover:bg-destructive/10 hover:text-destructive"
                      onClick={() => void onRevoke(share.id)}
                      disabled={revoking.has(share.id)}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                      {revoking.has(share.id) ? "Revoking…" : "Revoke"}
                    </Button>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      </div>
    </div>
  );
}
