import * as React from "react";
import { AlertTriangle } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

// ConfirmDialog — the destructive-action gate (kit, m13.1; spec §5). Delete /
// disable actions route through this modal. For high-blast-radius actions
// (delete an agent, a registry) pass `confirmText`: the primary button stays
// disabled until the user TYPES the resource name — the friction that prevents
// fat-finger deletes at scale. Optional `impact` slot renders a delete-impact
// preview (e.g. "3 bindings will be orphaned").

export interface ConfirmDialogProps {
  open: boolean;
  onCancel: () => void;
  onConfirm: () => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Typed-name confirmation — require the user to type this to enable confirm. */
  confirmText?: string;
  confirmLabel?: string;
  /** Style the confirm button as destructive (default true). */
  destructive?: boolean;
  busy?: boolean;
  /** A preview of what the action affects (delete-impact). */
  impact?: React.ReactNode;
}

export function ConfirmDialog({
  open,
  onCancel,
  onConfirm,
  title,
  description,
  confirmText,
  confirmLabel = "Delete",
  destructive = true,
  busy = false,
  impact,
}: ConfirmDialogProps) {
  const [typed, setTyped] = React.useState("");

  React.useEffect(() => {
    if (!open) setTyped("");
  }, [open]);

  React.useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onCancel();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;

  const gated = !!confirmText;
  const canConfirm = !gated || typed === confirmText;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="alertdialog"
      aria-modal="true"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onCancel}
        aria-hidden="true"
      />
      <div className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay">
        <div className="flex items-start gap-4">
          {destructive && (
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-destructive/15 text-destructive">
              <AlertTriangle className="h-5 w-5" />
            </div>
          )}
          <div className="min-w-0 flex-1">
            <h2 className="text-lg font-semibold tracking-snug">{title}</h2>
            {description && (
              <p className="mt-1 text-sm text-muted-foreground">{description}</p>
            )}
          </div>
        </div>

        {impact && (
          <div className="mt-4 rounded-md border bg-surface-2/60 p-3 text-sm">
            {impact}
          </div>
        )}

        {gated && (
          <div className="mt-4 space-y-1.5">
            <Label htmlFor="confirm-name">
              Type{" "}
              <span className="font-mono font-semibold text-foreground">
                {confirmText}
              </span>{" "}
              to confirm
            </Label>
            <Input
              id="confirm-name"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              placeholder={confirmText}
            />
          </div>
        )}

        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant={destructive ? "destructive" : "default"}
            onClick={onConfirm}
            disabled={!canConfirm || busy}
            className={cn(!canConfirm && "opacity-60")}
          >
            {busy ? "Working…" : confirmLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}
