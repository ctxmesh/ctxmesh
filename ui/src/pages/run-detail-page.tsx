import * as React from "react";
import { Link, useParams } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import {
  ClosingNote,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuietNote,
  Skeleton,
  Timeline,
  TimelineSkeleton,
  truncateId,
  type KeyValueItem,
  type TimelineStep,
} from "@/components/kit";
import { api, ApiError, type RunDetail, type RunTree, type RunTreeNode } from "@/lib/api";
import { formatDateTime, formatLatency, formatRelativeTime } from "@/lib/format";

// RunDetailPage — the run reader (V5/M112; M151 §6.1 archetype A5). Route: /runs/:id
//
// ── THE PAGE'S ONE IDEA: ONE STORY, NOT TWO SYSTEMS ─────────────────────────
// A model call, a tool result and an approval gate are not "the run" and "the
// governance system". To the person reading the page they are one sequence of
// things that happened, and the question they came to answer — "why is this
// sitting here?" — is answered by the next line down, not by correlating two
// clocks across two panels. So the hold renders IN the spine (kit Timeline,
// §5.26), annotated with its own hue and wash, exactly where it interrupted the
// work. Governance never gets its own lane on this page.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `GET /api/runs/{id}` returns identity, status, lineage, the message list and
// the pending action. It returns NO per-step timing, NO token counts and NO
// cost. Two consequences the page states out loud rather than papering over:
//
//   • Spend is UNKNOWN, never `$0.0000`. Mid-run that is "not yet known" —
//     tool spend is attributed when the run closes, and zero would be a claim
//     the platform has not made. After close it is "not recorded here": the run
//     endpoint records what a run DID, not what it cost. Either way the figure
//     is absent and the QuietNote says which of the two absences it is.
//   • Only the first and last moments carry a clock (createdAt / updatedAt).
//     Per-step timing lives in the trace, so intermediate steps show no time
//     rather than a fabricated one.
//
// Guardrail and redaction events are likewise NOT in this payload — they are
// span-level facts. The panel says so once instead of implying a run that
// crossed no guardrail.
//
// ── A HELD RUN IS HELD, NOT FAILED ──────────────────────────────────────────
// `requires_action` renders `hold` (violet, §2.4) — never crit and never the
// bound-crossed amber. Nothing has been lost: the tool has not been called and
// the run is holding its place. The copy says that in the timeline step and in
// the decision panel, because "held" and "failed" are the two states an
// operator most needs kept apart during an incident.
//
// data-testid contract:
//   run-detail-page        — root container (ready state)
//   run-detail-header      — the page band (id / agent / status)
//   run-detail-not-found   — the 404 state
//   run-detail-error       — the generic error state
//   run-approval-panel     — the decision panel (requires_action = approval)
//   run-approve-btn        — the Approve button
//   run-deny-btn           — the Deny button
//   run-deny-reason        — the optional denial reason
//   run-waiting-since      — how long the run has been held
//   run-original-request   — the ask, in full, for a held run
//   run-result             — what a succeeded run produced
//   run-nested-approvals   — the nested-approvals section
//   nested-run-{runId}     — each descendant's next step
//   run-orchestration      — the delegation tree (multi-agent runs only)

// ── The run-status vocabulary ────────────────────────────────────────────────

/** The Tag variants a run status may wear. Never `default`/pine — pine is not a status (§2.1). */
export type RunStatusVariant = "ok" | "progressing" | "hold" | "crit" | "muted";

/**
 * One run status → its semantic hue (§2.2).
 *
 * Two of these were wrong before M151 and both were wrong in the same way — a
 * state was wearing a hue that means something else:
 *
 *   • `running` returned `"default"`, the BRAND variant. A run that is
 *     executing was drawn in pine, and pine may never be a status. Converging
 *     under its own power is `progressing` (§2.5).
 *   • `requires_action` / `paused` returned `"warning"`. Amber means a bound is
 *     near or crossed while the thing keeps serving — a quota signal. A run
 *     waiting on a PERSON is `hold` (§2.4), the console's most important state
 *     and the one that must never be confused with "the system hit a limit".
 *
 * `cancelled` also moves, from the sunk `secondary` to `crit`: gray means "not
 * in motion and not a problem" (Draft, Disabled, idle) — a configuration, not
 * an outcome. A cancelled run will not proceed without a change, which is
 * exactly what crit means, and it is what the shared `resolveStatus` already
 * says about the same word.
 */
export function statusVariant(status: string): RunStatusVariant {
  switch (status) {
    case "succeeded":
    case "completed":
    case "done":
      return "ok";
    case "failed":
    case "error":
    case "cancelled":
      return "crit";
    case "requires_action":
    case "paused":
      return "hold";
    case "running":
    case "queued":
    case "pending":
    case "starting":
      return "progressing";
    default:
      return "muted";
  }
}

/** The tag word. Sentence-case here; the Badge recipe uppercases it. */
const STATUS_WORD: Record<string, string> = {
  succeeded: "Succeeded",
  completed: "Succeeded",
  done: "Done",
  failed: "Failed",
  error: "Failed",
  running: "Running",
  queued: "Queued",
  pending: "Pending",
  starting: "Starting",
  requires_action: "Waiting on approval",
  paused: "Held",
  cancelled: "Cancelled",
};

// Exported so the team page can render a run's status with the SAME words. A
// second copy of this map is how one console ends up with two lexicons for the
// same five states (the exact drift M144.1 was written to end).
export function fmtStatus(status: string): string {
  if (!status) return "Unknown";
  return (
    STATUS_WORD[status] ??
    status.replace(/[_-]+/g, " ").replace(/^./, (c) => c.toUpperCase())
  );
}

/** A run that has stopped moving. Drives the spend copy and the closing line. */
const TERMINAL = new Set(["succeeded", "completed", "failed", "error", "cancelled"]);

// ── The absence copy (§7.1) ──────────────────────────────────────────────────

/**
 * The canonical A5 sentence. It is the reason this panel exists: a mid-run
 * spend figure is not zero, it is unattributed, and the two must never share a
 * glyph.
 */
const SPEND_OPEN =
  "Tool spend is attributed when the run closes. It reads not yet known rather than $0.0000, because zero would be a claim we can't make.";

const SPEND_CLOSED =
  "This endpoint records what the run did, not what it cost. Per-step tokens and spend live in the trace backend; nothing here is estimated, the figures are simply absent.";

const SPEND_TITLE_OPEN =
  "Unattributed until the run closes — unknown, not zero.";
const SPEND_TITLE_CLOSED =
  "GET /api/runs/{id} does not report spend. It is unknown — not zero.";

/** Said once under the timeline, so a reader does not read absence as innocence. */
const STORY_LIMIT =
  "This is the run as the platform recorded it: the messages it exchanged and the decision it is waiting on. Per-step timing, token counts and guardrail events are span-level facts — they live in the trace, not here.";

// ── The story: one run → an ordered list of things that happened ─────────────

/**
 * A clock for a moment we actually recorded. Steps whose time is unknown carry
 * NO time rather than an interpolated one — a fabricated clock on a forensic
 * surface is the worst kind of confident wrongness.
 */
function clock(iso?: string): string | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return undefined;
  return new Date(t).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** The gate sentence per pause kind. A title is a SENTENCE, never an event enum (§5.26). */
const HOLD_TITLE: Record<string, string> = {
  approval: "A person has to approve this before it goes on",
  plan_approval: "A person has to approve the plan before it runs",
  consent_required: "The owner has to connect an account before this goes on",
};

/** The half of the hold that operators get wrong, said where they are reading. */
const HOLD_REASSURANCE =
  "Nothing has been lost. The tool has not been called and the run is holding its place — it is held, not failed.";

interface Story {
  steps: TimelineStep[];
  /** How many steps are waiting on a person — the PanelHeader's second figure. */
  held: number;
}

/**
 * The run's story, in order, from what the payload actually carries.
 *
 * `omitRequestDetail` exists because the ask is shown in full in its own panel
 * for a held run, and one fact belongs in one place on a surface — a reader who
 * meets the same paragraph twice starts to distrust both copies.
 */
function buildStory(detail: RunDetail, omitRequestDetail: boolean): Story {
  const steps: TimelineStep[] = [];
  const msgs = detail.messages ?? [];
  const held =
    detail.status === "requires_action" || detail.status === "paused";
  const terminal = TERMINAL.has(detail.status);
  const lastIdx = msgs.length - 1;
  let seenUser = false;

  msgs.forEach((m, i) => {
    const id = `msg-${i}`;
    if (m.role === "user") {
      const first = !seenUser;
      seenUser = true;
      steps.push({
        id,
        time: i === 0 ? clock(detail.createdAt) : undefined,
        title: first ? "The request arrived" : "The person added to the request",
        // Suppressed when the request panel is carrying the same words.
        detail:
          first && omitRequestDetail ? undefined : (
            <span className="whitespace-pre-wrap">{m.content}</span>
          ),
      });
      return;
    }
    if (m.role === "assistant") {
      // The closing answer is the Result panel's job; here it is the step that
      // ENDS the story, and it says so without repeating the prose.
      const closes = i === lastIdx && detail.status === "succeeded";
      steps.push({
        id,
        time: closes ? clock(detail.updatedAt) : undefined,
        title: closes ? "The run finished and answered" : "The model replied",
        detail: closes ? undefined : (
          <span className="whitespace-pre-wrap">{m.content}</span>
        ),
        tone: closes ? "done" : "step",
      });
      return;
    }
    if (m.role === "tool") {
      steps.push({
        id,
        title: "A tool returned its result",
        // Machine words are EVIDENCE, so they render inline-mono in the detail
        // line rather than becoming vocabulary the reader has to learn.
        detail: <span className="break-words font-mono text-xs">{m.content}</span>,
      });
      return;
    }
    steps.push({
      id,
      title: `A ${m.role} message was recorded`,
      detail: <span className="whitespace-pre-wrap">{m.content}</span>,
    });
  });

  // The gate, in the same spine as the work it interrupted.
  if (held && detail.requiresAction) {
    const kind = detail.requiresAction.kind;
    steps.push({
      id: "hold",
      time: clock(detail.updatedAt),
      title: HOLD_TITLE[kind] ?? "A person has to decide before this goes on",
      detail: HOLD_REASSURANCE,
      tone: "hold",
    });
  }

  if (terminal) {
    if (detail.status === "cancelled") {
      steps.push({
        id: "end",
        time: clock(detail.updatedAt),
        title: "The run was stopped before it finished",
        detail:
          detail.error ??
          "It will not go on. Nothing it had not already done was done.",
        tone: "failed",
      });
    } else if (detail.status === "failed" || detail.status === "error") {
      steps.push({
        id: "end",
        time: clock(detail.updatedAt),
        title: "The run stopped and did not finish",
        detail: detail.error ? (
          <span className="whitespace-pre-wrap">{detail.error}</span>
        ) : undefined,
        tone: "failed",
      });
    } else if (steps.every((s) => s.tone !== "done")) {
      steps.push({
        id: "end",
        time: clock(detail.updatedAt),
        title: "The run finished",
        tone: "done",
      });
    }
  }

  return { steps, held: steps.filter((s) => s.tone === "hold").length };
}

/** The §5.18 closing line: restates, in words, what the spine already showed. */
function closingLine(detail: RunDetail, stepCount: number): string | null {
  if (stepCount === 0) return null;
  const n = `${stepCount} step${stepCount === 1 ? "" : "s"}`;
  if (detail.status === "requires_action" || detail.status === "paused") {
    return `${n} so far, and the next one needs a person. Nothing is lost while it waits.`;
  }
  if (detail.status === "failed" || detail.status === "error") {
    return `${n}, and the last one is why it stopped.`;
  }
  if (detail.status === "cancelled") {
    return `${n} before it was stopped. What it had already done stands.`;
  }
  if (detail.status === "succeeded" || detail.status === "completed") {
    return `${n}, start to finish, with nothing waiting on a person.`;
  }
  return `${n} recorded so far. More appear as the run makes them.`;
}

/** The next step for one nested hold, verb-first and ≤22 chars (§7.2). */
function descendantStep(kind: string): string {
  if (kind === "plan_approval") return "Review the plan";
  if (kind === "consent_required") return "Connect the account";
  return "Review the hold";
}

// ── Approval state machine ────────────────────────────────────────────────────

type ApprovalState =
  | { kind: "idle" }
  | { kind: "submitting"; decision: "approve" | "deny" }
  | { kind: "done"; decision: "approve" | "deny" }
  | { kind: "error"; message: string };

// ── Page state ────────────────────────────────────────────────────────────────

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; detail: RunDetail }
  | { kind: "not-found" }
  | { kind: "error"; message: string; forbidden: boolean };

// ── RunDetailPage ─────────────────────────────────────────────────────────────

export function RunDetailPage() {
  const { id = "" } = useParams();
  const [state, setState] = React.useState<PageState>({ kind: "loading" });
  const [approval, setApproval] = React.useState<ApprovalState>({ kind: "idle" });
  // Optional free-text reason a reviewer can attach when DENYING (V16, m115.4) — stored on the run so the
  // denial is explainable. Ignored on approve.
  const [denyReason, setDenyReason] = React.useState("");

  // fetch (or re-fetch) the run
  const load = React.useCallback(
    (signal?: AbortSignal) => {
      if (!id) return;
      setState({ kind: "loading" });
      api
        .getRun(id, signal)
        .then((detail) => {
          if (signal?.aborted) return;
          setState({ kind: "ready", detail });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          if (err instanceof ApiError && err.status === 404) {
            setState({ kind: "not-found" });
            return;
          }
          const forbidden = err instanceof ApiError && err.isForbidden;
          setState({
            kind: "error",
            message:
              err instanceof Error ? err.message : "couldn't load the run",
            forbidden,
          });
        });
    },
    [id],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // Approve / Deny — mirror playground-page.tsx onApprove/onDeny. The resume
  // endpoint accepts { decision: "approve" | "deny" }; the key is an internal
  // resume token handled by the backend — never surfaced to the user.
  async function handleDecision(decision: "approve" | "deny") {
    if (!id) return;
    setApproval({ kind: "submitting", decision });
    try {
      // The reason is only meaningful on a deny; approve ignores it.
      await api.resumeRun(id, decision, decision === "deny" ? denyReason : undefined);
      setApproval({ kind: "done", decision });
      // Re-fetch so the status tag updates (the run is now running or cancelled).
      load();
    } catch (err) {
      setApproval({
        kind: "error",
        message: err instanceof Error ? err.message : "resume failed",
      });
    }
  }

  // ── Loading (§7 A5: five timeline steps + a rail of kv bars) ────────────────
  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6">
        <PageHeader title="Run" loading />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          <Card className="min-w-0">
            <PanelHeader title="What happened" />
            <CardContent>
              <TimelineSkeleton />
            </CardContent>
          </Card>
          <Card className="min-w-0">
            <PanelHeader title="Cost so far" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label="Loading run facts">
                {[0, 1, 2, 3, 4].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  // ── Not found ───────────────────────────────────────────────────────────────
  if (state.kind === "not-found") {
    return (
      <div className="min-w-0 space-y-6" data-testid="run-detail-not-found">
        <PageHeader
          title="Run not found"
          lede="Nothing on this cluster answers to that id."
        />
        <QuietNote title="No run with this id was found.">
          The id <span className="break-all font-mono text-xs">{id}</span> is not
          in the run store. It may have been deleted, or the link may name a run
          from another cluster. Nothing is missing from this page — there is
          simply no run to show.
          <span className="mt-3 block">
            <NextStepLink label="Back to runs" to="/runs" />
          </span>
        </QuietNote>
      </div>
    );
  }

  // ── Error ───────────────────────────────────────────────────────────────────
  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader title="Run" />
          <ForbiddenInline
            title="You don't have permission to view this run."
            resource="runs"
            detail={state.message}
          />
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="run-detail-error">
        <PageHeader title="Run" />
        <ErrorState
          title="The run didn't load."
          description="Nothing has changed about the run itself — only this page failed to read it."
          detail={state.message}
          onRetry={() => load()}
        />
      </div>
    );
  }

  // ── Ready ───────────────────────────────────────────────────────────────────
  const { detail } = state;
  // Show the approve/deny panel for either approvable pause kind — the workflow PLAN gate
  // (plan_approval, the deep-link + approval-queue's primary case) OR the mid-run HITL step
  // gate (approval). consent_required is NOT approvable here (owner connects their account).
  const isApprovalPause =
    (detail.requiresAction?.kind === "approval" ||
      detail.requiresAction?.kind === "plan_approval") &&
    detail.status === "requires_action";
  // isApprovalRun is the DURABLE "this run is/was an approval" signal (any status) — requiresAction
  // persists across resolve (deny → cancelled, approve → running both keep it), so a run a colleague
  // already resolved still shows the "← Approvals" nav-out on a fresh deep-link (V16 F7a).
  const isApprovalRun =
    detail.requiresAction?.kind === "approval" ||
    detail.requiresAction?.kind === "plan_approval";
  const isSubmitting = approval.kind === "submitting";
  const descendants = detail.descendantsRequiringAction ?? [];
  const hasDescendants = descendants.length > 0;
  // The tree parent to navigate back to (V16 F3): the immediate parent, else the tree root — but never a
  // self-link on a root run (rootRunId can equal the run's own id).
  const parentTarget =
    detail.parentRunId ||
    (detail.rootRunId && detail.rootRunId !== detail.id ? detail.rootRunId : "");

  // The ask, in full — the context M112's review said a plan must never be
  // judged without. Gated on the pause, so a finished run does not re-litigate
  // a decision nobody is being asked to make.
  const firstUserMsg = detail.messages?.find((m) => m.role === "user")?.content;
  const rawInput =
    typeof detail.input === "string"
      ? detail.input
      : detail.input != null
        ? JSON.stringify(detail.input, null, 2)
        : undefined;
  const requestText = isApprovalPause ? (firstUserMsg ?? rawInput) : undefined;
  const showRequestPanel = !!requestText;

  const story = buildStory(detail, showRequestPanel && !!firstUserMsg);
  const closing = closingLine(detail, story.steps.length);
  const terminal = TERMINAL.has(detail.status);

  // A COMPLETED run shows what it produced — the composed final answer (m130).
  // Any leaked K1 spotlight delimiter (ADR 0059) is stripped defensively:
  // internal markers are never shown to a user.
  const answer =
    detail.status === "succeeded"
      ? [...(detail.messages ?? [])]
          .reverse()
          .find((m) => m.role === "assistant")
          ?.content?.replace(/⟦\/?tool-output:[^⟧]*⟧/g, "")
          .trim()
      : undefined;

  const waitingRel =
    isApprovalPause && detail.updatedAt
      ? formatRelativeTime(detail.updatedAt)
      : "";

  // Duration is a FACT only for a run that has stopped: `updatedAt` on a live
  // run is the last transition, not now, so "ran for 12s" would be a claim
  // about a run that is still going.
  const ranMs =
    terminal && detail.createdAt && detail.updatedAt
      ? Date.parse(detail.updatedAt) - Date.parse(detail.createdAt)
      : NaN;
  const metaLine = [
    detail.agent,
    detail.namespace,
    Number.isFinite(ranMs) && ranMs > 0 ? formatLatency(ranMs) : undefined,
  ]
    .filter(Boolean)
    .join(" · ");

  const facts: KeyValueItem[] = [
    { key: "Run id", value: detail.id, title: detail.id },
    { key: "Agent", value: detail.agent, absent: "not recorded" },
    { key: "Workspace", value: detail.namespace, absent: "not recorded" },
    {
      key: "Started",
      value: detail.createdAt ? formatDateTime(detail.createdAt) : undefined,
      absent: "not recorded",
    },
    {
      key: "Last change",
      value: detail.updatedAt ? formatDateTime(detail.updatedAt) : undefined,
      absent: "not recorded",
    },
    {
      key: "Trace",
      value: detail.traceId ? truncateId(detail.traceId) : undefined,
      absent: "not linked",
      title: detail.traceId ?? "No trace was recorded for this run.",
    },
  ];

  const spendTitle = terminal ? SPEND_TITLE_CLOSED : SPEND_TITLE_OPEN;
  const spendAbsent = terminal ? "not recorded here" : "not yet known";
  const spend: KeyValueItem[] = [
    { key: "Model spend", absent: spendAbsent, title: spendTitle },
    { key: "Tool spend", absent: spendAbsent, title: spendTitle },
    { key: "Tokens", absent: spendAbsent, title: spendTitle },
  ];

  return (
    <div className="min-w-0 space-y-6" data-testid="run-detail-page">
      <div data-testid="run-detail-header">
        <PageHeader
          breadcrumb={[{ label: "Runs", to: "/runs" }, { label: truncateId(detail.id) }]}
          title={`run / ${truncateId(detail.id)}`}
          titleMono
          status={
            <Badge variant={statusVariant(detail.status)}>
              {fmtStatus(detail.status)}
            </Badge>
          }
          meta={metaLine || undefined}
        />
      </div>

      {/* The reviewer's exits (V16 F7a/F3). "← Approvals" shows for ANY approval run — a run a
          colleague already resolved, reached by deep-link, still has a way back to the queue.
          "← Parent run" climbs a sub-run back to the tree root, where the nested holds are
          overviewed. These are BACK-links, not next steps, so they keep the plain pine link
          register rather than the underlined "Next step" treatment (§7.2 is verb-first user
          actions; "go back where you came from" is navigation). */}
      {(isApprovalRun || parentTarget) && (
        <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
          {isApprovalRun && (
            <Link
              to="/approvals"
              data-testid="run-back-approvals"
              className="text-sm font-medium text-primary hover:underline"
            >
              ← Approvals
            </Link>
          )}
          {parentTarget && (
            <Link
              to={`/runs/${encodeURIComponent(parentTarget)}`}
              data-testid="run-back-parent"
              className="text-sm font-medium text-primary hover:underline"
            >
              {/* Accurate label: the immediate parent when known, else the tree root. */}
              {detail.parentRunId ? "← Parent run" : "← Root run"}
            </Link>
          )}
        </div>
      )}

      {/* §4.7 g2: the story on the left, what governs and what it cost on the
          right — with EXPLICIT grid placement, because DOM order is also the
          stacked reading order below `lg`. The delegation tree is long and it is
          context, not an errand, so it is placed in a second left-column row:
          stacked, a reviewer reads the ask, then the story, then their decision,
          and only then who did what. In one flat column it sat between them and
          the Approve button. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
        {/* ── What happened ──────────────────────────────────────────────── */}
        <div className="min-w-0 space-y-5 lg:col-start-1 lg:row-start-1">
          {showRequestPanel && (
            <Card className="min-w-0" data-testid="run-original-request">
              <PanelHeader title="What was asked" />
              <CardContent>
                <p className="mb-3 text-sm text-secondary-foreground">
                  Judge the decision below against this, not against the summary
                  of it.
                </p>
                {/* A code well (§4.5): the raw ask keeps its own indentation and
                    scrolls inside its own frame rather than widening the page. */}
                <div className="max-h-56 overflow-auto rounded-md bg-surface-3 p-4 text-sm">
                  <pre className="whitespace-pre-wrap break-words font-mono text-xs">
                    {requestText}
                  </pre>
                </div>
              </CardContent>
            </Card>
          )}

          <Card className="min-w-0">
            <PanelHeader
              title="What happened"
              meta={
                story.steps.length === 0
                  ? undefined
                  : `${story.steps.length} step${story.steps.length === 1 ? "" : "s"}${
                      story.held > 0 ? ` · ${story.held} held` : ""
                    }`
              }
            >
              {detail.traceId ? (
                <NextStepLink
                  label="Open the trace"
                  to={`/traces/${encodeURIComponent(detail.traceId)}`}
                  ariaLabel={`Open the trace for run ${detail.id}`}
                />
              ) : null}
            </PanelHeader>
            <CardContent>
              {story.steps.length === 0 ? (
                <QuietNote title="This run recorded no steps.">
                  Nothing has been written against it yet — not a model call, not
                  a tool call, not a decision. Steps appear here in order as the
                  run makes them.
                </QuietNote>
              ) : (
                <>
                  <Timeline
                    steps={story.steps}
                    label={`Steps in run ${detail.id}`}
                  />
                  {closing && <ClosingNote>{closing}</ClosingNote>}
                  <QuietNote className="mt-4">{STORY_LIMIT}</QuietNote>
                </>
              )}
            </CardContent>
          </Card>

          {answer && (
            <Card className="min-w-0" data-testid="run-result">
              <PanelHeader title="What it produced" />
              <CardContent>
                <p className="whitespace-pre-wrap text-md">{answer}</p>
              </CardContent>
            </Card>
          )}
        </div>

        {/* ── The rail: the decision, the nested holds, the spend ─────────── */}
        <div className="min-w-0 space-y-5 lg:col-start-2 lg:row-start-1 lg:row-span-2">
          {isApprovalPause && (
            <Card
              // A 2px hold rule, not a hold fill: §2.2 allows exactly two
              // full-bleed semantic surfaces console-wide and this is not one.
              className="min-w-0 border-l-2 border-l-hold"
              data-testid="run-approval-panel"
            >
              <PanelHeader
                title={
                  detail.requiresAction?.kind === "plan_approval"
                    ? "Your decision on the plan"
                    : "Your decision"
                }
              />
              <CardContent className="space-y-4">
                <p className="text-sm text-secondary-foreground">
                  {detail.requiresAction?.kind === "plan_approval"
                    ? "This run is holding on the plan it proposed. Approving lets it run; denying stops it for good."
                    : "This run is holding on one step. Approving lets it continue; denying stops it for good."}{" "}
                  {HOLD_REASSURANCE}
                </p>

                {detail.requiresAction?.message && (
                  <p className="border-l-2 border-l-hold bg-surface-2 px-4 py-3 font-serif text-md italic">
                    {detail.requiresAction.message}
                  </p>
                )}

                {waitingRel && (
                  <p className="text-sm text-faint" data-testid="run-waiting-since">
                    Waiting since{" "}
                    <span
                      className="font-mono tabular-nums text-secondary-foreground"
                      title={detail.updatedAt ? formatDateTime(detail.updatedAt) : undefined}
                    >
                      {waitingRel}
                    </span>
                  </p>
                )}

                {/* Feedback from a submitted decision + the exit back to the queue. */}
                {approval.kind === "done" && (
                  <div className="space-y-2" role="status">
                    <p className="text-sm text-success">
                      Decision submitted:{" "}
                      {approval.decision === "approve" ? "Approved" : "Denied"}.
                      The run state will update momentarily.
                    </p>
                    <NextStepLink label="Back to approvals" to="/approvals" />
                  </div>
                )}
                {approval.kind === "error" && (
                  <p className="text-sm text-destructive" role="alert">
                    {approval.message}
                  </p>
                )}

                {/* Optional reason — recorded on the run when denying (V16, m115.4). */}
                {approval.kind !== "done" && (
                  <div className="space-y-1">
                    <label
                      htmlFor="deny-reason"
                      className="font-mono text-2xs uppercase tracking-wide text-faint"
                    >
                      Reason (optional — recorded if you deny)
                    </label>
                    <Textarea
                      id="deny-reason"
                      data-testid="run-deny-reason"
                      rows={2}
                      value={denyReason}
                      onChange={(e) => setDenyReason(e.target.value)}
                      placeholder="Why are you denying this? (shown on the run)"
                      disabled={isSubmitting}
                    />
                  </div>
                )}

                {/* Full-width stacked (§6.1 A5) — the two answers to one question
                    read as a pair, not as a toolbar. */}
                <div className="space-y-2">
                  <Button
                    className="w-full"
                    data-testid="run-approve-btn"
                    disabled={isSubmitting || approval.kind === "done"}
                    onClick={() => void handleDecision("approve")}
                  >
                    {isSubmitting && approval.kind === "submitting" && approval.decision === "approve"
                      ? "Approving…"
                      : "Approve"}
                  </Button>
                  <Button
                    variant="outline"
                    className="w-full"
                    data-testid="run-deny-btn"
                    disabled={isSubmitting || approval.kind === "done"}
                    onClick={() => void handleDecision("deny")}
                  >
                    {/* §6.1 A5 words this "Deny and fail the run". Deviation, with
                        its reason: a denial resolves the run to `cancelled`, not
                        to `failed`, and the tag two inches away will say
                        "Cancelled". A button that names a state the platform
                        never produces teaches the wrong word for the state. */}
                    {isSubmitting && approval.kind === "submitting" && approval.decision === "deny"
                      ? "Denying…"
                      : "Deny and stop the run"}
                  </Button>
                </div>
              </CardContent>
            </Card>
          )}

          {hasDescendants && (
            <Card className="min-w-0" data-testid="run-nested-approvals">
              <PanelHeader title="Also waiting on a person">
                <Badge variant="hold" data-testid="nested-approvals-count">
                  {descendants.length}
                </Badge>
              </PanelHeader>
              <CardContent>
                <p className="mb-2 text-sm text-secondary-foreground">
                  {descendants.length === 1
                    ? "One sub-run inside this one is holding on its own decision."
                    : `${descendants.length} sub-runs inside this one are holding on their own decisions.`}{" "}
                  Resolving this run does not resolve them.
                </p>
                <ul>
                  {descendants.map((d) => (
                    <li
                      key={d.runId}
                      className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-border-soft py-3 last:border-0"
                    >
                      <div className="min-w-0">
                        <p
                          className="truncate font-mono text-xs text-faint"
                          title={d.runId}
                        >
                          {truncateId(d.runId)}
                        </p>
                        {d.agent && (
                          <p className="truncate text-sm font-medium" title={d.agent}>
                            {d.agent}
                          </p>
                        )}
                        {d.message && (
                          <p className="mt-0.5 text-sm text-secondary-foreground">
                            {d.message}
                          </p>
                        )}
                      </div>
                      <NextStepLink
                        label={descendantStep(d.kind)}
                        to={`/runs/${encodeURIComponent(d.runId)}`}
                        ariaLabel={`${descendantStep(d.kind)} — run ${d.runId}`}
                        testId={`nested-run-${d.runId}`}
                      />
                    </li>
                  ))}
                </ul>
              </CardContent>
            </Card>
          )}

          <Card className="min-w-0">
            <PanelHeader title="Cost so far" />
            <CardContent>
              <KeyValueList items={spend} />
              <QuietNote
                className="mt-4"
                title={
                  terminal
                    ? "Spend isn't recorded on the run."
                    : "Spend isn't attributed yet."
                }
              >
                {terminal ? SPEND_CLOSED : SPEND_OPEN}
              </QuietNote>
              {detail.traceId && (
                <p className="mt-4">
                  {/* A different promise from the header's "Open the trace":
                      that one goes to the story, this one answers the question
                      the dashes above raise. Two links, two errands. */}
                  <NextStepLink
                    label="See per-step cost"
                    to={`/traces/${encodeURIComponent(detail.traceId)}`}
                    ariaLabel={`See the per-step cost of run ${detail.id} in its trace`}
                  />
                </p>
              )}
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <KeyValueList items={facts} />
            </CardContent>
          </Card>
        </div>

        {/* The delegation tree (M124) — who handed work to whom. It renders
            NOTHING for a single-agent run, so `empty:hidden` retires the grid
            row with it rather than leaving a gap where a panel isn't. */}
        <div className="min-w-0 empty:hidden lg:col-start-1 lg:row-start-2">
          <OrchestrationTree runId={detail.id} />
        </div>
      </div>
    </div>
  );
}

// truncateText keeps a node's task/result readable in the tree card.
function truncateText(s: string, n: number): string {
  const t = s.trim();
  return t.length > n ? t.slice(0, n).trimEnd() + "…" : t;
}

// OrchestrationTree (M124) renders the run-tree when this run delegated to specialists: the supervisor
// at the top, then each specialist it handed a sub-task to, with what each was asked and what it
// produced. Renders NOTHING for a plain single-agent run (a tree of one). Best-effort: a failed/absent
// tree simply doesn't render (never an error banner — this is an enrichment, not the page's core).
function OrchestrationTree({ runId }: { runId: string }) {
  const [tree, setTree] = React.useState<RunTree | null>(null);
  React.useEffect(() => {
    const controller = new AbortController();
    api
      .getRunTree(runId, controller.signal)
      .then(setTree)
      .catch(() => setTree(null));
    return () => controller.abort();
  }, [runId]);

  // Best-effort enrichment: never crash the page on an absent/malformed tree (a partial response
  // must render nothing, not throw on tree.nodes.length). Single-node tree ⇒ not orchestrated.
  if (!tree || !Array.isArray(tree.nodes) || tree.nodes.length <= 1) return null;

  // The LIVE lens of the delegation canvas (M144.10, ADR 0115): the run-tree drawn
  // as the actual delegation TREE (who handed work to whom), the filled counterpart
  // to the Team Sheet's declared, hollow structure. Children keyed by parentRunId.
  const byParent = new Map<string, RunTreeNode[]>();
  for (const n of tree.nodes) {
    const p = n.parentRunId ?? tree.rootId;
    if (n.id === tree.rootId) continue; // the root anchors the tree, not its own child
    if (!byParent.has(p)) byParent.set(p, []);
    byParent.get(p)!.push(n);
  }
  for (const arr of byParent.values()) {
    arr.sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  }
  const root = tree.nodes.find((n) => n.id === tree.rootId) ?? tree.nodes[0];
  // Orphans: any node whose parent isn't in the tree hangs off the root, so nothing
  // is silently dropped from the picture.
  const seen = new Set<string>([root.id]);
  const collect = (id: string) => {
    for (const c of byParent.get(id) ?? []) {
      seen.add(c.id);
      collect(c.id);
    }
  };
  collect(root.id);
  const orphans = tree.nodes.filter((n) => !seen.has(n.id));
  if (orphans.length > 0) byParent.set(root.id, [...(byParent.get(root.id) ?? []), ...orphans]);

  const total = tree.nodes.length;

  return (
    <Card className="min-w-0" data-testid="run-orchestration">
      <PanelHeader title="Who did the work" meta={`${total} agents`} />
      <CardContent>
        <p className="mb-4 text-sm text-secondary-foreground">
          The platform ran {total} agent{total === 1 ? "" : "s"} to complete this
          — here is who delegated to whom, and how the work flowed between them.
        </p>
        <RunTreeNodeView node={root} byParent={byParent} depth={0} />
      </CardContent>
    </Card>
  );
}

function RunTreeNodeView({
  node,
  byParent,
  depth,
}: {
  node: RunTreeNode;
  byParent: Map<string, RunTreeNode[]>;
  depth: number;
}) {
  const children = byParent.get(node.id) ?? [];
  return (
    <div
      className={
        depth > 0
          ? "relative pl-5 before:absolute before:left-[7px] before:top-0 before:h-full before:w-px before:bg-border-strong"
          : ""
      }
    >
      {/* the connector elbow into this node */}
      {depth > 0 && (
        <span
          className="absolute left-[7px] top-[14px] h-px w-3 bg-border-strong"
          aria-hidden
        />
      )}
      <div
        data-testid={`orchestration-node-${node.agent}`}
        className="min-w-0 rounded-md border border-border bg-surface-2 p-3"
      >
        <div className="flex flex-wrap items-center gap-2">
          {/* One encoding of state per row. The old row carried BOTH a coloured
              dot and this tag, and the dot's own palette said `bg-info` for a
              running node — which is now the hold violet, i.e. "a person must
              decide" painted on a run that needs nobody. The tag is the
              console's status vocabulary; the dot was a second, wrong one. */}
          <span className="min-w-0 truncate text-sm font-medium" title={node.agent}>
            {node.agent}
          </span>
          {depth === 0 && <Badge variant="muted">supervisor</Badge>}
          <Badge variant={statusVariant(node.status)} className="ml-auto">
            {fmtStatus(node.status)}
          </Badge>
        </div>
        {node.input && (
          <p className="mt-2 text-sm text-secondary-foreground">
            <span className="font-mono text-2xs uppercase tracking-wide text-faint">
              Task
            </span>{" "}
            {truncateText(node.input, 180)}
          </p>
        )}
        {node.output && (
          <p className="mt-1 text-sm text-secondary-foreground">
            <span className="font-mono text-2xs uppercase tracking-wide text-faint">
              Result
            </span>{" "}
            {truncateText(node.output, 240)}
          </p>
        )}
      </div>
      {children.length > 0 && (
        <div className="mt-2 space-y-2">
          {children.map((c) => (
            <RunTreeNodeView key={c.id} node={c} byParent={byParent} depth={depth + 1} />
          ))}
        </div>
      )}
    </div>
  );
}
