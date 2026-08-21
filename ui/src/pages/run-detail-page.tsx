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
  const isSubmitting = approval.kind === "submitting";
  const hasDescendants =
    (detail.descendantsRequiringAction?.length ?? 0) > 0;

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
      </div>

      {/* ── Approval panel — shown only when the run is paused for approval ─── */}
      {isApprovalPause && (
        <Card data-testid="run-approval-panel">
          <CardHeader>
            <CardTitle className="text-base">Approval required</CardTitle>
            <CardDescription>
              This run is paused awaiting your decision before it can continue.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {detail.requiresAction?.message && (
              <div className="rounded-md border bg-muted/40 p-4 text-sm">
                {detail.requiresAction.message}
              </div>
            )}

            {/* Feedback from a previous approval attempt */}
            {approval.kind === "done" && (
              <p className="text-sm text-success" role="status">
                Decision submitted: {approval.decision === "approve" ? "Approved" : "Denied"}.
                The run state will update momentarily.
              </p>
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
            <CardTitle className="text-base">Nested approvals</CardTitle>
            <CardDescription>
              One or more sub-runs within this run are also paused awaiting approval.
              Click a run to review and act on it.
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
