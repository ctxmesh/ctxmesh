import * as React from "react";
import { Link, useParams } from "react-router-dom";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { ForbiddenInline, SkeletonCard } from "@/components/kit";
import { api, ApiError, type RunDetail } from "@/lib/api";
import { formatRelativeTime, formatDateTime } from "@/lib/format";

// RunDetailPage — per-run detail view (V5, M112). Route: /runs/:id
//
// Loads the run by its run.ID (path param `:id` — NOT a traceId) via api.getRun().
// When the run is paused awaiting an approval, renders an "Approval required"
// panel with Approve / Deny buttons that POST /api/runs/{id}/resume (mirroring
// playground-page.tsx's onApprove/onDeny, ~lines 299-351).
//
// When descendantsRequiringAction is non-empty, renders a "Nested approvals"
// section listing each sub-run as a navigable link to /runs/:runId so the
// operator can drill into the subtree (M108 L1-surfacing consumer).
//
// data-testid contract:
//   run-detail-page        — root container (ready state)
//   run-detail-header      — the summary block (id / agent / status)
//   run-approval-panel     — approval required card (when requires_action = approval)
//   run-approve-btn        — the Approve button
//   run-deny-btn           — the Deny button
//   run-nested-approvals   — the nested-approvals section
//   nested-run-{runId}     — each descendant link

// ── Helpers ──────────────────────────────────────────────────────────────────

function statusVariant(
  status: string,
): "default" | "success" | "warning" | "destructive" | "secondary" {
  switch (status) {
    case "succeeded":
    case "completed":
      return "success";
    case "failed":
    case "error":
      return "destructive";
    case "running":
      return "default";
    case "requires_action":
    case "paused":
      return "warning";
    case "cancelled":
      return "secondary";
    default:
      return "secondary";
  }
}

function fmtStatus(status: string): string {
  if (!status) return "Unknown";
  // requires_action → "Awaiting action"
  if (status === "requires_action") return "Awaiting action";
  return status.charAt(0).toUpperCase() + status.slice(1);
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

  // Approve / Deny — mirror playground-page.tsx onApprove/onDeny (~lines 299–351).
  // The resume endpoint accepts { decision: "approve" | "deny" }; the key is an
  // internal resume token handled by the backend — never surfaced to the user.
  async function handleDecision(decision: "approve" | "deny") {
    if (!id) return;
    setApproval({ kind: "submitting", decision });
    try {
      await api.resumeRun(id, decision);
      setApproval({ kind: "done", decision });
      // Re-fetch so the status badge updates (run is now running or cancelled).
      load();
    } catch (err) {
      setApproval({
        kind: "error",
        message: err instanceof Error ? err.message : "resume failed",
      });
    }
  }

  // ── Loading ─────────────────────────────────────────────────────────────────
  if (state.kind === "loading") {
    return (
      <div className="mx-auto max-w-3xl space-y-6 px-4 py-6">
        <SkeletonCard />
        <SkeletonCard />
      </div>
    );
  }

  // ── Not found ───────────────────────────────────────────────────────────────
  if (state.kind === "not-found") {
    return (
      <div
        className="mx-auto max-w-3xl px-4 py-6"
        data-testid="run-detail-not-found"
      >
        <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground shadow-card">
          <p className="font-medium text-foreground">Run not found</p>
          <p className="mt-1">
            No run with ID <span className="font-mono">{id}</span> was found.
            It may have been deleted or the link may be incorrect.
          </p>
        </div>
      </div>
    );
  }

  // ── Error ───────────────────────────────────────────────────────────────────
  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="mx-auto max-w-3xl px-4 py-6">
          <ForbiddenInline
            title="Not allowed to read this run"
            description="Your account can't read this run."
            detail={state.message}
          />
        </div>
      );
    }
    return (
      <div
        className="mx-auto max-w-3xl px-4 py-6"
        role="alert"
        data-testid="run-detail-error"
      >
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-6 text-sm text-destructive">
          Couldn't load the run: {state.message}
        </div>
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
  // already resolved still shows the "← Approvals" nav-out on a fresh deep-link (V16 F7a: the old gate
  // was isApprovalPause, leaving a resolved run with no way back to the queue).
  const isApprovalRun =
    detail.requiresAction?.kind === "approval" ||
    detail.requiresAction?.kind === "plan_approval";
  const isSubmitting = approval.kind === "submitting";
  const hasDescendants =
    (detail.descendantsRequiringAction?.length ?? 0) > 0;
  const descendantCount = detail.descendantsRequiringAction?.length ?? 0;
  // The tree parent to navigate back to (V16 F3): the immediate parent, else the tree root — but never a
  // self-link on a root run (rootRunId can equal the run's own id).
  const parentTarget =
    detail.parentRunId ||
    (detail.rootRunId && detail.rootRunId !== detail.id ? detail.rootRunId : "");

  return (
    <div
      className="mx-auto max-w-3xl space-y-6 px-4 py-6"
      data-testid="run-detail-page"
    >
      {/* ── Header / summary ──────────────────────────────────────────────── */}
      <div
        className="rounded-lg border bg-card p-5 shadow-card"
        data-testid="run-detail-header"
      >
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 space-y-1">
            <h1 className="text-lg font-semibold tracking-snug">Run detail</h1>
            <p className="font-mono text-xs text-muted-foreground break-all">
              {detail.id}
            </p>
          </div>
          <Badge variant={statusVariant(detail.status)}>
            {fmtStatus(detail.status)}
          </Badge>
        </div>

        {(detail.agent || detail.namespace) && (
          <p className="mt-2 text-sm text-muted-foreground">
            {detail.agent && <span className="font-medium text-foreground">{detail.agent}</span>}
            {detail.agent && detail.namespace && " · "}
            {detail.namespace && <span>{detail.namespace}</span>}
          </p>
        )}

        {detail.error && (
          <p className="mt-3 text-sm text-destructive" role="alert">
            Error: {detail.error}
          </p>
        )}

        {detail.traceId && (
          <p className="mt-3 text-sm text-muted-foreground">
            Trace:{" "}
            <Link
              to={`/traces/${encodeURIComponent(detail.traceId)}`}
              className="font-mono text-xs text-primary underline hover:no-underline"
            >
              {detail.traceId}
            </Link>
          </p>
        )}

        {/* Waiting since — shown when the run is paused; updatedAt reflects the pause transition */}
        {isApprovalPause && detail.updatedAt && (() => {
          const rel = formatRelativeTime(detail.updatedAt);
          const abs = formatDateTime(detail.updatedAt);
          return rel ? (
            <p className="mt-3 text-sm text-muted-foreground" data-testid="run-waiting-since">
              Waiting since{" "}
              <span className="font-medium text-foreground" title={abs}>{rel}</span>
            </p>
          ) : null;
        })()}
      </div>

      {/* ── Nav-out row — the reviewer's exit (V16 F7a/F3) ─────────────────── */}
      {/* "← Approvals" shows for ANY approval run (not only while paused) so a run a colleague already   */}
      {/* resolved, reached by deep-link, still has a way back to the queue. "← Parent run" shows for a    */}
      {/* sub-run so the reviewer can climb back to the tree root (where nested approvals are overviewed). */}
      {(isApprovalRun || parentTarget) && (
        <div className="flex flex-wrap items-center gap-4">
          {isApprovalRun && (
            <Link
              to="/approvals"
              data-testid="run-back-approvals"
              className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
            >
              ← Approvals
            </Link>
          )}
          {parentTarget && (
            <Link
              to={`/runs/${encodeURIComponent(parentTarget)}`}
              data-testid="run-back-parent"
              className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
            >
              ← Parent run
            </Link>
          )}
        </div>
      )}

      {/* ── Original request — the context a reviewer needs to judge the plan ─── */}
      {/* M112-UX-review P2: a plan must not be judged with only the plan summary.        */}
      {/* Render the first user message from detail.messages (role==="user"), or          */}
      {/* detail.input as a fallback. Placed ABOVE the approval panel so it reads first. */}
      {isApprovalPause && (() => {
        const firstUserMsg = detail.messages?.find((m) => m.role === "user")?.content;
        const rawInput =
          typeof detail.input === "string"
            ? detail.input
            : detail.input != null
              ? JSON.stringify(detail.input, null, 2)
              : undefined;
        const requestText = firstUserMsg ?? rawInput;
        if (!requestText) return null;
        return (
          <Card data-testid="run-original-request">
            <CardHeader>
              <CardTitle className="text-base">Original request</CardTitle>
              <CardDescription>
                The original input that triggered this run — evaluate the plan against this ask.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div
                className="max-h-48 overflow-y-auto rounded-md border bg-muted/40 p-4 text-sm"
                style={{ whiteSpace: "pre-wrap", wordBreak: "break-word" }}
              >
                {requestText}
              </div>
            </CardContent>
          </Card>
        );
      })()}

      {/* ── Approval panel — shown only when the run is paused for approval ─── */}
      {isApprovalPause && (
        <Card data-testid="run-approval-panel">
          <CardHeader>
            <CardTitle className="text-base">
              {detail.requiresAction?.kind === "plan_approval"
                ? "Plan approval required"
                : "Action required"}
            </CardTitle>
            <CardDescription>
              {detail.requiresAction?.kind === "plan_approval"
                ? "This workflow run is paused on its proposed plan. Approving lets it proceed; denying cancels the run permanently."
                : "This run is paused on a mid-run step awaiting your decision. Approving lets it continue; denying cancels the run permanently."}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {detail.requiresAction?.message && (
              <div className="rounded-md border bg-muted/40 p-4 text-sm">
                {detail.requiresAction.message}
              </div>
            )}

            {/* Feedback from a previous approval attempt + the exit back to the queue */}
            {approval.kind === "done" && (
              <div className="space-y-2" role="status">
                <p className="text-sm text-success">
                  Decision submitted: {approval.decision === "approve" ? "Approved" : "Denied"}.
                  The run state will update momentarily.
                </p>
                <Link
                  to="/approvals"
                  className="inline-flex items-center gap-1 text-sm font-medium text-primary hover:underline"
                >
                  Return to Approvals →
                </Link>
              </div>
            )}
            {approval.kind === "error" && (
              <p className="text-sm text-destructive" role="alert">
                {approval.message}
              </p>
            )}

            <div className="flex gap-3">
              <Button
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
                data-testid="run-deny-btn"
                disabled={isSubmitting || approval.kind === "done"}
                onClick={() => void handleDecision("deny")}
              >
                {isSubmitting && approval.kind === "submitting" && approval.decision === "deny"
                  ? "Denying…"
                  : "Deny"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* ── Nested approvals — sub-runs paused awaiting action (M108) ─────── */}
      {hasDescendants && (
        <Card data-testid="run-nested-approvals">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              Nested approvals
              <Badge variant="secondary" data-testid="nested-approvals-count">
                {descendantCount}
              </Badge>
            </CardTitle>
            <CardDescription>
              {descendantCount} sub-run{descendantCount === 1 ? "" : "s"} within this run{" "}
              {descendantCount === 1 ? "is" : "are"} also paused awaiting approval. Click a run to
              review and act on it.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="space-y-2">
              {detail.descendantsRequiringAction!.map((d) => (
                <li
                  key={d.runId}
                  className="flex flex-col gap-0.5 rounded-md border bg-muted/30 px-4 py-3 text-sm"
                >
                  <Link
                    to={`/runs/${encodeURIComponent(d.runId)}`}
                    data-testid={`nested-run-${d.runId}`}
                    className="font-mono text-xs font-medium text-primary underline hover:no-underline"
                  >
                    {d.runId}
                  </Link>
                  {d.agent && (
                    <span className="text-xs text-muted-foreground">
                      Agent: {d.agent}
                    </span>
                  )}
                  {d.message && (
                    <span className="text-xs text-muted-foreground">
                      {d.message}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
