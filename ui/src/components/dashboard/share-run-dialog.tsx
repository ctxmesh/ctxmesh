import * as React from "react";
import { Copy, Check, Share2, Trash2, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useFocusTrap } from "@/components/kit/use-focus-trap";
import { useToast } from "@/components/kit";
import { api, type RunShare, type CreateRunShareResponse } from "@/lib/api";

// ShareRunDialog — the run share story (m75.4).
// Share action → redaction-honest preview of the projection → create → show link once.
// Also surfaces manage/revoke for existing shares.
//
// data-testid contract:
//   share-run-dialog           — root modal panel
//   share-include-content      — the transcript toggle checkbox
//   share-projection-preview   — the projection preview block
//   share-ttl-select           — the TTL picker
//   share-create-btn           — the Create Share Link button
//   share-link-once            — the one-time link display block
//   share-link-copy            — the copy button for the link
//   share-link-value           — the link text element (for test reading)
//   share-manage-section       — the manage/revoke section
//   share-row-{id}             — one existing share row
//   share-revoke-{id}          — the revoke button for a specific share

export interface ShareRunDialogProps {
  open: boolean;
  onClose: () => void;
  runId: string;
  // Optional: the run data already on the page to build the preview
  runData?: {
    agent?: string;
    namespace?: string;
    status?: string;
    messageCount?: number;
    messageRoles?: string[];
    errorCategory?: string;
  };
}

type CreateState =
  | { kind: "idle" }
  | { kind: "creating" }
  | { kind: "done"; result: CreateRunShareResponse; link: string }
  | { kind: "error"; message: string };

type ManageState =
  | { kind: "loading" }
  | { kind: "ready"; shares: RunShare[] }
  | { kind: "error"; message: string };

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

export function ShareRunDialog({ open, onClose, runId, runData }: ShareRunDialogProps) {
  const [includeContent, setIncludeContent] = React.useState(false);
  const [ttlHours, setTtlHours] = React.useState(168);
  const [createState, setCreateState] = React.useState<CreateState>({ kind: "idle" });
  const [manageState, setManageState] = React.useState<ManageState>({ kind: "loading" });
  const [revoking, setRevoking] = React.useState<Set<string>>(new Set());
  const [copied, setCopied] = React.useState(false);
  const { toast } = useToast();
  const titleId = React.useId();

  const panelRef = useFocusTrap<HTMLDivElement>({ active: open, onEscape: onClose });

  // Load existing shares on open
  React.useEffect(() => {
    if (!open) return;
    setManageState({ kind: "loading" });
    setCreateState({ kind: "idle" });
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

  async function onCreate() {
    setCreateState({ kind: "creating" });
    try {
      const result = await api.createRunShare(runId, includeContent, ttlHours);
      const link = `${window.location.origin}/shared/runs/${result.token}`;
      setCreateState({ kind: "done", result, link });
      // Refresh the manage list
      api.listRunShares(runId).then((shares) =>
        setManageState({ kind: "ready", shares })
      ).catch(() => {});
    } catch (err) {
      setCreateState({
        kind: "error",
        message: err instanceof Error ? err.message : "failed to create share",
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

  // Projection preview — what will be public for the current toggle state
  const metadataFields = [
    "Agent name and namespace",
    "Run status",
    "Created and updated timestamps",
    `Message count (${runData?.messageCount ?? "–"}) and roles (${(runData?.messageRoles ?? []).join(", ") || "–"})`,
    runData?.errorCategory ? `Error category: ${runData.errorCategory}` : "Error category (if any)",
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
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
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
          <button
            onClick={onClose}
            className="rounded p-1 text-muted-foreground hover:text-foreground"
            aria-label="Close"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        {/* Create section */}
        {createState.kind !== "done" && (
          <div className="mt-5 space-y-5">
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

            {/* Projection preview — the redaction-honest core */}
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
              <p className="text-sm text-destructive" role="alert">{createState.message}</p>
            )}

            <Button
              data-testid="share-create-btn"
              onClick={() => void onCreate()}
              disabled={createState.kind === "creating"}
              className="w-full"
            >
              {createState.kind === "creating" ? "Creating…" : "Create share link"}
            </Button>
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
          </div>
        )}

        {/* Manage / revoke section */}
        <div data-testid="share-manage-section" className="mt-6">
          <h3 className="text-sm font-medium text-muted-foreground">Existing shares</h3>
          <div className="mt-2">
            {manageState.kind === "loading" && (
              <p className="text-xs text-muted-foreground">Loading…</p>
            )}
            {manageState.kind === "error" && (
              <p className="text-xs text-destructive">{manageState.message}</p>
            )}
            {manageState.kind === "ready" && manageState.shares.length === 0 && (
              <p className="text-xs text-muted-foreground">No active shares.</p>
            )}
            {manageState.kind === "ready" && manageState.shares.map((share) => (
              <div
                key={share.id}
                data-testid={`share-row-${share.id}`}
                className="flex items-center gap-3 rounded-md border p-2 mt-2 text-xs"
              >
                <div className="min-w-0 flex-1 space-y-0.5">
                  <div className="flex items-center gap-2">
                    <span className={share.revoked ? "text-muted-foreground line-through" : "text-foreground"}>
                      {share.includeContent ? "With transcript" : "Metadata only"}
                    </span>
                    {share.revoked && (
                      <span className="rounded bg-destructive/15 px-1 text-destructive text-xs">Revoked</span>
                    )}
                  </div>
                  <p className="text-muted-foreground">
                    Created {fmtExpiry(share.createdAt)} · Expires {fmtExpiry(share.expiresAt)}
                  </p>
                </div>
                {!share.revoked && (
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
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}
