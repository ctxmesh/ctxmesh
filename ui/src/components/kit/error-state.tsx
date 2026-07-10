import * as React from "react";
import { AlertTriangle, RefreshCw, ShieldAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// ErrorState — an error primitive that ALWAYS offers a next action (kit,
// m13.1). No dead-end red text: a failed load shows what broke + a Retry (or
// a role-specific fix). The 403 variant is first-class because RBAC-aware
// chrome (spec §3) must "explain-and-suggest, never a blank screen".

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
  const Icon = forbidden ? ShieldAlert : AlertTriangle;
  const heading =
    title ?? (forbidden ? "You don't have access" : "Something went wrong");

  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center rounded-lg border px-6 py-12 text-center",
        forbidden
          ? "border-warning/40 bg-warning/5"
          : "border-destructive/40 bg-destructive/5",
        className,
      )}
    >
      <div
        className={cn(
          "mb-4 flex h-12 w-12 items-center justify-center rounded-xl",
          forbidden
            ? "bg-warning/15 text-warning"
            : "bg-destructive/15 text-destructive",
        )}
      >
        <Icon className="h-6 w-6" />
      </div>
      <h3 className="text-lg font-semibold tracking-snug">{heading}</h3>
      {description && (
        <p className="mt-1.5 max-w-md text-sm text-muted-foreground">
          {description}
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
