import * as React from "react";

import { ErrorState } from "./error-state";

// ForbiddenInline — the reusable 403 primitive (ADR 0012, ui-foundation §2–3).
// When a caller-scoped /api/* request returns 403 (K8s RBAC denied — ADR 0011),
// a surface renders THIS instead of a blank screen or a raw error: it composes
// the kit ErrorState `forbidden` variant into an explain-and-suggest block. Every
// m13.5 surface (agents list, routes/secrets/registries, etc.) feeds api.ts's
// typed 403 (ApiError.isForbidden) into this one component, so denial looks the
// same everywhere and always names the next step.
//
// It is a thin, opinionated wrapper — NOT a fork of ErrorState — so the "always
// a next action" invariant and the token-only styling come for free. Pass the
// denied `resource` (e.g. "agents") for the friendly, resource-named message and
// an optional `action` (e.g. "Switch namespace").

export interface ForbiddenInlineProps {
  /** What the caller tried to do, e.g. "list agents in team-a". Drives the copy. */
  title?: string;
  /** Human explanation; defaults to the RBAC explain-and-suggest line. */
  description?: React.ReactNode;
  /**
   * The BFF's 403 message. Accepted for source compatibility but NO LONGER SURFACED on a permission
   * boundary (M100 UI99-403) — a raw "forbidden: cannot list <kind>" is noise to a user + the audit's
   * leak. Pass `resource` for the friendly, resource-named copy instead.
   */
  detail?: string;
  /** The resource denied, e.g. "agents" — drives the friendly "view <resource>" copy (M100). */
  resource?: string;
  /** Optional next action (e.g. "Switch namespace", "Request access"). */
  action?: { label: string; onClick?: () => void };
  className?: string;
}

export function ForbiddenInline({
  title,
  description,
  detail,
  resource,
  action,
  className,
}: ForbiddenInlineProps) {
  return (
    <ErrorState
      variant="forbidden"
      title={title}
      description={description}
      detail={detail}
      resource={resource}
      action={action}
      className={className}
    />
  );
}
