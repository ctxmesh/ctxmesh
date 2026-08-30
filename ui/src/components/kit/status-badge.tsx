import { Badge } from "@/components/ui/badge";

// StatusBadge — the ONE health-status chip across the console (M99 E1, resystematized M144.1
// from the Fable audit). Every resource list rendered near-identical pills with DIVERGENT
// lexicons/casing/colour ("Ready" vs "valid" vs lowercase "ready"), AND collapsed every
// not-yet-healthy state into one amber "warning" — so "still converging" (Pending) looked the
// same as "a human must act" (AwaitingHumanPromotion) looked the same as "failed" (NotReady).
//
// M144.1 gives status ONE vocabulary with five tones that MEAN different things:
//   ready       → green   the system reconciled it; serving. Ready and ONLY Ready is green.
//   progressing → blue    the system is still converging (Pending, provisioning, scoring).
//   waiting     → amber   a HUMAN must act (approval, promotion). agentry's most important state.
//   failed      → red     it will not converge without a change (NotReady, Failed, blocked).
//   draft       → gray    not yet enabled / disabled.
// Purple (primary) is reserved for brand + interactivity and is NEVER a status.

export type StatusTone = "ready" | "progressing" | "waiting" | "failed" | "draft";

const TONE_VARIANT: Record<StatusTone, "success" | "info" | "warning" | "destructive" | "muted"> = {
  ready: "success",
  progressing: "info",
  waiting: "warning",
  failed: "destructive",
  draft: "muted",
};

// Known phase/reason strings the BFF emits → a human, sentence-case label.
const HUMAN_LABEL: Record<string, string> = {
  ready: "Ready",
  promoted: "Promoted",
  served: "Serving",
  active: "Active",
  valid: "Ready",
  succeeded: "Succeeded",
  pending: "Pending",
  provisioning: "Provisioning",
  scoring: "Scoring",
  revisionmissing: "Building revision",
  awaitinghumanpromotion: "Waiting on promotion",
  held: "Held for approval",
  requiresaction: "Waiting on approval",
  notready: "Not ready",
  failed: "Failed",
  error: "Error",
  blocked: "Blocked",
  denied: "Denied",
  cancelled: "Cancelled",
  draft: "Draft",
  disabled: "Disabled",
};

const key = (s?: string) => (s ?? "").trim().toLowerCase().replace(/[\s_-]/g, "");

// sentenceCase("NotReady") → "Not ready"; "RevisionMissing" → "Revision missing".
function sentenceCase(s: string): string {
  const spaced = s.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/[_-]+/g, " ").trim();
  if (!spaced) return "";
  return spaced.charAt(0).toUpperCase() + spaced.slice(1).toLowerCase();
}

/** Resolve a resource's (ready, phase, reason) into a semantic tone + Badge variant + label. */
export function resolveStatus(
  ready: boolean,
  phase?: string,
  reason?: string,
): { tone: StatusTone; variant: "success" | "info" | "warning" | "destructive" | "muted"; label: string } {
  const hay = `${phase ?? ""} ${reason ?? ""}`.toLowerCase();
  // A converging phase (the system is still working toward Ready) — blue, not a problem.
  const converging =
    /(pending|provision|scoring|building|reconcil|revision|creating|initiali|updating|queued|starting|in ?progress)/;
  let tone: StatusTone;
  if (/(awaiting|approval|promotion|requires[_ ]?action|\bheld\b|hitl|\bhuman\b)/.test(hay)) {
    tone = "waiting"; // a HUMAN must act — surface this even over "ready"
  } else if (ready || /(^|[^a-z])(promoted|served|succeeded|healthy)([^a-z]|$)/.test(hay)) {
    tone = "ready";
  } else if (converging.test(hay)) {
    tone = "progressing";
  } else if (/(draft|disabled|paused)/.test(hay)) {
    tone = "draft";
  } else if (hay.trim()) {
    // A NAMED not-ready state that isn't converging is a problem — NotReady, Failed,
    // Error, a validation reason (InvalidPattern, DanglingEdge, …). Red, not blue.
    tone = "failed";
  } else {
    tone = "progressing"; // no signal yet → still converging
  }
  const label =
    HUMAN_LABEL[key(phase)] ??
    HUMAN_LABEL[key(reason)] ??
    (phase && phase.trim() ? sentenceCase(phase) : "") ??
    "";
  const fallback: Record<StatusTone, string> = {
    ready: "Ready",
    progressing: "Pending",
    waiting: "Waiting",
    failed: "Not ready",
    draft: "Draft",
  };
  return { tone, variant: TONE_VARIANT[tone], label: label || fallback[tone] };
}

/** Humanize a raw controller reason string for the subordinate "cause" line (M144.1). */
export function humanizeStatusReason(reason?: string): string {
  const k = key(reason);
  if (!k) return "";
  return HUMAN_LABEL[k] ?? sentenceCase(reason!);
}

export function StatusBadge({
  ready,
  phase,
  reason,
  className,
}: {
  ready: boolean;
  phase?: string;
  reason?: string;
  className?: string;
}) {
  const { variant, label } = resolveStatus(ready, phase, reason);
  return (
    <Badge variant={variant} className={className}>
      {label}
    </Badge>
  );
}
