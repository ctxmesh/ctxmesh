import * as React from "react";
import { AlertTriangle } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { useFocusTrap } from "@/components/kit/use-focus-trap";

// ConfirmDialog — the destructive-action gate (kit, m13.1 → real m13.4; spec
// §5). Delete / disable actions route through this modal. For high-blast-radius
// actions (delete an agent, a registry) pass `confirmText`: the primary button
// stays disabled until the user TYPES the resource name — the friction that
// prevents fat-finger deletes at scale. Optional `impact` slot renders a
// delete-impact preview (e.g. "3 bindings will be orphaned").
//
// Productionized in m13.4:
//   • Focus trap + focus-return + Esc (shared useFocusTrap); on open, focus
//     lands on the typed-name input (gated) or Cancel (safe default).
//   • Enter submits ONLY when confirmation is satisfied — Enter in the gate
//     input is inert until the typed name matches, mirroring the button state
//     (no keyboard bypass of the typed-name gate).
//   • aria-labelledby / aria-describedby wire the alertdialog to its heading
//     and description.

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
  const titleId = React.useId();
  const descId = React.useId();
  const panelRef = useFocusTrap<HTMLDivElement>({
    active: open,
    onEscape: onCancel,
  });

  React.useEffect(() => {
    if (!open) setTyped("");
  }, [open]);

  const gated = !!confirmText;
  const canConfirm = !gated || typed === confirmText;

  function confirmIfAllowed() {
    if (canConfirm && !busy) onConfirm();
  }

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="alertdialog"
      aria-modal="true"
      aria-labelledby={titleId}
      aria-describedby={description ? descId : undefined}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onCancel}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative flex max-h-[85vh] w-full max-w-md flex-col rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <div className="flex shrink-0 items-start gap-4">
          {destructive && (
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-destructive-surface text-destructive">
              <AlertTriangle className="h-5 w-5" />
            </div>
          )}
          <div className="min-w-0 flex-1">
            <h2
              id={titleId}
              className="font-serif text-lg font-medium tracking-snug"
            >
              {title}
            </h2>
            {description && (
              <p id={descId} className="mt-1 text-sm text-muted-foreground">
                {description}
              </p>
            )}
          </div>
        </div>

        {impact && (
          <div className="mt-4 max-h-[50vh] min-h-0 overflow-y-auto rounded-md border bg-surface-2 p-3 text-sm">
            {impact}
          </div>
        )}

        {gated && (
          <div className="mt-4 shrink-0 space-y-1.5">
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
              // Enter submits only when the typed name matches — no bypass of
              // the gate via the keyboard.
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  confirmIfAllowed();
                }
              }}
              autoComplete="off"
              placeholder={confirmText}
            />
          </div>
        )}

        <div className="mt-6 flex shrink-0 justify-end gap-2">
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
