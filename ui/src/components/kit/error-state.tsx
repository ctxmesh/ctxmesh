import * as React from "react";
import { AlertTriangle, Lock, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// ErrorState — an error primitive that ALWAYS offers a next action (kit,
// m13.1 → real m13.4; restyled M151 §5.8). No dead-end red text: a failed load
// shows what broke + a Retry (or a role-specific fix). The 403 variant is
// first-class because RBAC-aware chrome (spec §3) must "explain-and-suggest,
// never a blank screen".
//
// The two variants are two different truths and are drawn differently on
// purpose (M151 §7):
//   • "error"     — it broke. Crit tint, the raw reason in a mono well, Retry.
//   • "forbidden" — a permission boundary: EXPECTED, routine, and not a
//                   failure. Calm neutral frame + a lock (M99 C1), and copy
//                   that names the missing permission instead of "something
//                   went wrong". Never red, never an alarm.
//
// Production invariant (m13.4): the component is guaranteed to render at least
// one actionable affordance. `error` defaults to a Retry; `forbidden` (which
// has no retry) falls back to a description that names the next step. If a
// caller somehow supplies neither retry nor action nor description, we still
// render a default explanation so the surface is never a dead end.

/** What the caller was denied — drives the verb in the 403 copy (§7, A1/A4). */
export type ForbiddenPermission = "read" | "create" | "update" | "delete";

// Heading verb ("permission to VIEW agents") vs the RBAC verb the admin must
// actually grant ("a role that can READ agents"). They differ for `read` on
// purpose: users think in "view", roles are written in "read".
const HEADING_VERB: Record<ForbiddenPermission, string> = {
  read: "view",
  create: "create",
  update: "edit",
  delete: "delete",
};

export interface ErrorStateProps {
  variant?: "error" | "forbidden";
  title?: string;
  /** Human explanation — what failed and, ideally, why. */
  description?: React.ReactNode;
  /** The raw error/detail, shown in a monospace well (collapsible feel). */
  detail?: string;
  /**
   * The resource the caller was denied, e.g. "agents", "model routes" (M100 UI99-403). On the
   * `forbidden` variant it drives a friendly, consistent message ("You don't have permission to
   * view <resource>. Ask an admin for a role that can read <resource>.") IN PLACE OF the raw BFF
   * RBAC string — so a 403 reads the same, human way everywhere and never leaks "cannot list <kind>".
   * Ignored on the `error` variant.
   */
  resource?: string;
  /**
   * Which permission is missing on `resource` — defaults to "read". A create/edit/delete surface
   * denied at 403 must say so ("Ask an admin for a role that can create teams", §7 A4): a write
   * denial rendered as a read denial sends the user to ask for the wrong role.
   * Ignored on the `error` variant.
   */
  permission?: ForbiddenPermission;
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
  resource,
  permission = "read",
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
    title ??
    (forbidden
      ? resource
        ? `You don't have permission to ${HEADING_VERB[permission]} ${resource}`
        : "You don't have access"
      : "Something went wrong");

  // ALWAYS-a-next-action invariant. `error` gets a default Retry only when the
  // caller wired one; when it wired nothing at all, a default explanation keeps
  // the state from being a blank dead end.
  const hasButtonAction = !!onRetry || !!action;
  const resolvedDescription =
    description ??
    (forbidden
      ? resource
        ? `Ask an admin for a role that can ${permission} ${resource}.`
        : "Ask an admin to grant access, or switch to a namespace you can read."
      : hasButtonAction
        ? undefined
        : "Reload the page or try again in a moment.");

  // A permission boundary NEVER surfaces the raw BFF RBAC string ("forbidden: cannot list <kind>")
  // — it is noise to an end user and the exact leak the audit flagged (M100 UI99-403). The friendly
  // heading + description above carry the whole message; the raw detail well is for the `error`
  // variant (a 502/500 where the reason aids debugging), not for a routine 403.
  const showDetail = detail && !forbidden;

  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center rounded-lg border px-6 py-12 text-center",
        forbidden
          ? "border-border bg-surface-2/40"
          : "border-destructive/40 bg-destructive-surface/40",
        className,
      )}
    >
      <div
        className={cn(
          "mb-4 flex h-12 w-12 items-center justify-center rounded-lg",
          forbidden
            ? "bg-surface-2 text-muted-foreground"
            : "bg-destructive-surface text-destructive",
        )}
      >
        <Icon className="h-6 w-6" />
      </div>
      <h3 className="font-serif text-lg font-medium">{heading}</h3>
      {resolvedDescription && (
        <p className="mt-1.5 max-w-md text-sm text-muted-foreground">
          {resolvedDescription}
        </p>
      )}
      {showDetail && (
        <pre className="mt-4 max-w-md overflow-x-auto rounded-md bg-surface-3 px-3 py-2 text-left font-mono text-xs text-muted-foreground">
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
