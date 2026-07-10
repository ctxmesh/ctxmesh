import * as React from "react";
import { Check, Info, TriangleAlert, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Toast — transient confirmation with an optional Undo (kit, m13.1; spec §5).
// Reversible actions (delete, disable) show a toast with Undo so the console
// stays forgiving at scale. This primitive renders a single toast card; the
// stack/host wiring is the parent's (a fuller ToastProvider lands in m13.4).

export type ToastVariant = "success" | "error" | "info";

export interface ToastProps {
  variant?: ToastVariant;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Reversible action affordance. */
  onUndo?: () => void;
  onDismiss?: () => void;
  className?: string;
}

const ICONS: Record<ToastVariant, typeof Check> = {
  success: Check,
  error: TriangleAlert,
  info: Info,
};

const TONE: Record<ToastVariant, string> = {
  success: "text-success",
  error: "text-destructive",
  info: "text-info",
};

export function Toast({
  variant = "info",
  title,
  description,
  onUndo,
  onDismiss,
  className,
}: ToastProps) {
  const Icon = ICONS[variant];
  return (
    <div
      role="status"
      className={cn(
        "flex w-full max-w-sm items-start gap-3 rounded-lg border bg-popover p-4 shadow-elevated",
        className,
      )}
    >
      <Icon className={cn("mt-0.5 h-5 w-5 shrink-0", TONE[variant])} />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{title}</p>
        {description && (
          <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {onUndo && (
        <Button variant="link" size="sm" className="h-auto p-0" onClick={onUndo}>
          Undo
        </Button>
      )}
      {onDismiss && (
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss"
          className="text-muted-foreground hover:text-foreground"
        >
          <X className="h-4 w-4" />
        </button>
      )}
    </div>
  );
}
