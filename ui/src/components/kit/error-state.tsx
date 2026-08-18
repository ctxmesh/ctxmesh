import * as React from "react";
import { AlertTriangle, Lock, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// ErrorState — an error primitive that ALWAYS offers a next action (kit,
// m13.1 → real m13.4). No dead-end red text: a failed load shows what broke +
// a Retry (or a role-specific fix). The 403 variant is first-class because
// RBAC-aware chrome (spec §3) must "explain-and-suggest, never a blank screen".
//
// Production invariant (m13.4): the component is guaranteed to render at least
// one actionable affordance. `error` defaults to a Retry; `forbidden` (which
// has no retry) falls back to a description that names the next step. If a
// caller somehow supplies neither retry nor action nor description, we still
// render a default explanation so the surface is never a dead end.

export interface ErrorStateProps {
  variant?: "error" | "forbidden";
  title?: string;
  /** Human explanation — what failed and, ideally, why. */
  description?: React.ReactNode;
  /** The raw error/detail, shown in a monospace well (collapsible feel). */
  detail?: string;
  onRetry?: () => void;
  retryLabel?: string;
  /** Extra action (e.g. "Request access", "Switch namespace"). */
  action?: { label: string; onClick?: () => void };
  className?: string;
}

export function ErrorState({
  variant = "error",
  title,
  description,
  detail,
  onRetry,
  retryLabel = "Retry",
  action,
  className,
}: ErrorStateProps) {
  const forbidden = variant === "forbidden";
  // A permission boundary is a CALM, expected state — a lock, not an alarm (M99 C1). The alarming
  // amber/ShieldAlert treatment made a routine "not for your role" read like a data warning.
  const Icon = forbidden ? Lock : AlertTriangle;
  const heading =
    title ?? (forbidden ? "You don't have access" : "Something went wrong");

  // ALWAYS-a-next-action invariant. `error` gets a default Retry only when the
  // caller wired one; when it wired nothing at all, a default explanation keeps
  // the state from being a blank dead end.
  const hasButtonAction = !!onRetry || !!action;
  const resolvedDescription =
    description ??
    (forbidden
      ? "Ask an admin to grant access, or switch to a namespace you can read."
      : hasButtonAction
        ? undefined
        : "Reload the page or try again in a moment.");

  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center rounded-lg border px-6 py-12 text-center",
        forbidden
          ? "border-border bg-muted/30"
          : "border-destructive/40 bg-destructive/5",
        className,
      )}
    >
      <div
        className={cn(
          "mb-4 flex h-12 w-12 items-center justify-center rounded-xl",
          forbidden
            ? "bg-muted text-muted-foreground"
            : "bg-destructive/15 text-destructive",
        )}
      >
        <Icon className="h-6 w-6" />
      </div>
      <h3 className="text-lg font-semibold tracking-snug">{heading}</h3>
      {resolvedDescription && (
        <p className="mt-1.5 max-w-md text-sm text-muted-foreground">
          {resolvedDescription}
        </p>
      )}
      {detail && (
        <pre className="mt-4 max-w-md overflow-x-auto rounded-md bg-surface-3 px-3 py-2 text-left text-xs text-muted-foreground">
          {detail}
        </pre>
      )}
      <div className="mt-6 flex items-center gap-3">
        {onRetry && (
          <Button variant="outline" onClick={onRetry}>
            <RefreshCw className="h-4 w-4" />
            {retryLabel}
          </Button>
        )}
        {action && (
          <Button
            variant={forbidden ? "default" : "secondary"}
            onClick={action.onClick}
          >
            {action.label}
          </Button>
        )}
      </div>
    </div>
  );
}
