import { Badge } from "@/components/ui/badge";

// StatusBadge — the ONE health-status chip across the console (M99 E1, resystematized M144.1
// from the Fable audit). Every resource list rendered near-identical pills with DIVERGENT
// lexicons/casing/colour ("Ready" vs "valid" vs lowercase "ready"), AND collapsed every
// not-yet-healthy state into one amber "warning" — so "still converging" (Pending) looked the
// same as "a human must act" (AwaitingHumanPromotion) looked the same as "failed" (NotReady).
//
// M144.1 gives status ONE vocabulary with five tones that MEAN different things. M151
// (ADR 0128, spec §2.2/§2.4/§2.5) kept the tones and the routing EXACTLY as they were and
// only recoloured them, because the state model was right and two of its hues were not:
//   ready       → ok           green        the system reconciled it; serving. Ready and ONLY Ready is green.
//   progressing → progressing  pine tint    the machine is converging on its own (Pending, provisioning, scoring).
//   waiting     → hold         violet       a HUMAN must act (approval, promotion). ctxmesh's most important state.
//   failed      → crit         red          it will not converge without a change (NotReady, Failed, blocked).
//   draft       → muted        gray         not yet enabled / disabled — off, not a problem.
// Pine (primary) is reserved for brand + interactivity and is NEVER a status.
//
// Two hues moved and one left the palette entirely:
//   - `waiting` was amber. Amber (`warn`) now means ONLY "a bound is near or crossed,
//     degraded but serving" (§2.2) — a meter/quota signal, not a readiness state — so NO
//     tone maps to it here. "A person must act" gets its own hue, violet `hold`, because
//     "the system hit a bound" and "a person is blocking work right now" are the two states
//     ops tooling most often confuses and they demand different urgency.
//   - `progressing` was blue. Blue is gone; hold-violet must not absorb converging, because
//     converging needs no person — it is the machine doing its own work, the one state that
//     is legitimately the system's own colour, safe at annotation strength.

export type StatusTone = "ready" | "progressing" | "waiting" | "failed" | "draft";

/** The Tag variants a status may render as (§5.1). Never `default`/pine: pine is not a status. */
export type StatusVariant = "ok" | "progressing" | "hold" | "crit" | "muted";

const TONE_VARIANT: Record<StatusTone, StatusVariant> = {
  ready: "ok",
  progressing: "progressing",
  waiting: "hold",
  failed: "crit",
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
): { tone: StatusTone; variant: StatusVariant; label: string } {
  const hay = `${phase ?? ""} ${reason ?? ""}`.toLowerCase();
  // A converging phase (the system is still working toward Ready) — the pine-tint
  // `progressing` chip, not a problem.
  const converging =
    /(pending|provision|scoring|building|reconcil|revision|creating|initiali|updating|queued|starting|ingest|in ?progress)/;
  // ...unless the SAME string also says it stopped. `converging` matches on word
  // fragments, so "RevisionFailed" and "ProvisioningTimeout" hit `revision` and
  // `provision` and used to render as "still working" — a terminal failure
  // wearing the calm chip, which is the worst way for this component to be
  // wrong. An explicit terminal token always wins over a converging one (M151).
  //
  // Deliberately NOT terminal: "missing" / "not found". "RevisionMissing" during
  // a reconcile means the controller has not created it YET, which is exactly
  // the converging case — so those stay calm.
  const terminal =
    /(fail|error|\bdenied\b|refused|invalid|backoff|crashloop|unschedulable|timeout|timed ?out|exceeded|evicted|oom|rejected|aborted|unreachable)/;
  let tone: StatusTone;
  if (/(awaiting|approval|promotion|requires[_ ]?action|\bheld\b|hitl|\bhuman\b)/.test(hay)) {
    tone = "waiting"; // a HUMAN must act — surface this even over "ready"
  } else if (ready || /(^|[^a-z])(promoted|served|succeeded|healthy)([^a-z]|$)/.test(hay)) {
    tone = "ready";
  } else if (converging.test(hay) && !terminal.test(hay)) {
    tone = "progressing";
  } else if (/(draft|disabled|paused)/.test(hay)) {
    tone = "draft";
  } else if (hay.trim()) {
    // A NAMED not-ready state that isn't converging is a problem — NotReady, Failed,
    // Error, a validation reason (InvalidPattern, DanglingEdge, …). Red, not converging.
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
  const { tone, variant, label } = resolveStatus(ready, phase, reason);
  return (
    // The chip is the console's answer to "is my agent running?", so it carries a
    // stable hook. `data-status-tone` is the machine-readable half: the LABEL is
    // humanised per phase/reason and varies across resources, but the TONE is the
    // resolved verdict, which is what a journey test actually needs to assert (M153).
    <Badge
      variant={variant}
      className={className}
      data-testid="status-badge"
      data-status-tone={tone}
    >
      {label}
    </Badge>
  );
}
