import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { CheckSquare, RefreshCw } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type ApprovalQueueItem } from "@/lib/api";
import { formatRelativeTime, formatDateTime } from "@/lib/format";

// ApprovalsPage — the V15 unified "Approvals" inbox (M113).
//
// Backend: GET /api/approvals?namespace=<ns>
//   • namespace is REQUIRED — the backend returns 400 without it.
//   • The queue is namespace-scoped and persona-gated: a caller without
//     `list workflows` in the namespace gets a 403, never an empty list.
//   • Namespace comes from the GLOBAL namespace selector (same as alerts-page).
//   • Items carry kind ("plan_approval" | "approval") for badge display.
//
// 403-forbidden discipline (mirrors alerts-page):
//   • 403 (caller lacks `list workflows`): ApiError.isForbidden →
//     the DataTable's forbidden variant, never a fake empty list.
//   • 500 + other non-2xx: a visible, retryable error state.
//
// The page is READ-only — it deep-links each row to /runs/:id (the run detail
// page) where the reviewer approves or denies the plan.
//
// data-testid contract:
//   approvals-page  — root container
//   approvals-table — the DataTable (via aria-label="Approvals")

type LoadState =
  | { kind: "loading" }
  | { kind: "no-namespace" }
  | { kind: "ready"; items: ApprovalQueueItem[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function ApprovalsPage() {
  const { namespace } = useNamespace();
  // Initialise from the current namespace so the first render already shows the right state — no
  // loading-skeleton flash before the "select a namespace" prompt when none is selected.
  const [loadState, setLoadState] = useState<LoadState>(
    namespace ? { kind: "loading" } : { kind: "no-namespace" },
  );
  // refreshing drives the manual Refresh button's spinner. The manual refresh is SILENT (it does not blank
  // the table with a skeleton — matching the background poll); the spinner is the only feedback (V16 close-
  // gate UX finding: a non-silent manual refresh flashed a skeleton over already-visible rows).
  const [refreshing, setRefreshing] = useState(false);
  const abortRef = useRef<AbortController | null>(null);

  // load fetches the queue. `silent` (used by the poll) refreshes the rows IN PLACE — it does not flip to
  // the loading skeleton, and a failed silent refresh keeps the current rows rather than blowing the table
  // away with an error (a background poll must never disrupt what the reviewer is looking at). A manual load
  // (the Refresh button / initial mount) is non-silent: it shows loading + surfaces errors normally.
  const load = useCallback(
    (silent = false) => {
      abortRef.current?.abort();
      // The queue is namespace-scoped and the backend REQUIRES a namespace (unlike the
      // cluster-wide alerts feed). When none is selected (the default "all" scope), prompt
      // the user to pick one rather than firing a request that 400s — a select-a-namespace
      // state is the honest UX, not an error.
      if (!namespace) {
        setLoadState({ kind: "no-namespace" });
        setRefreshing(false);
        return;
      }
      const controller = new AbortController();
      abortRef.current = controller;
      if (!silent) setLoadState({ kind: "loading" });

      api
        .listApprovals(namespace, controller.signal)
        .then((items) => {
          if (controller.signal.aborted) return;
          setLoadState({ kind: "ready", items });
        })
        .catch((err: unknown) => {
          if (controller.signal.aborted) return;
          // A background poll must not disrupt the view — keep the current rows on a silent failure.
          if (silent) return;
          setLoadState({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
            forbidden: err instanceof ApiError && err.isForbidden,
          });
        })
        .finally(() => {
          if (silent) setRefreshing(false);
        });
    },
    [namespace],
  );

  // Manual refresh: refresh IN PLACE (silent — no skeleton flash) with a button spinner for feedback.
  const manualRefresh = useCallback(() => {
    setRefreshing(true);
    load(true);
  }, [load]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // A ~30s background poll so a row a colleague already resolved isn't clicked blind (V16). Silent (no
  // skeleton flash), and only while a namespace is selected — the interval resets when the namespace changes
  // (load's identity changes) and is torn down on unmount.
  useEffect(() => {
    if (!namespace) return;
    const id = window.setInterval(() => load(true), 30_000);
    return () => window.clearInterval(id);
  }, [load, namespace]);

  const items = loadState.kind === "ready" ? loadState.items : [];

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "approvals",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<ApprovalQueueItem>[] = [
    {
      id: "runId",
      header: "Run",
      cell: (item) => (
        <Link
          to={`/runs/${encodeURIComponent(item.runId)}`}
          className="font-mono text-xs text-primary underline-offset-2 hover:underline"
        >
          {item.runId}
        </Link>
      ),
    },
    {
      id: "agent",
      header: "Agent",
      cell: (item) => <span className="text-sm font-medium">{item.agent}</span>,
    },
    {
      id: "namespace",
      header: "Namespace",
      hideOnMobile: true,
      cell: (item) => (
        <span className="font-mono text-xs text-muted-foreground">{item.namespace}</span>
      ),
    },
    {
      // kind badge: "Plan gate" for plan_approval, "Step approval" for approval.
      // Orients the reviewer: a plan gate blocks execution start; a step gate
      // blocks a privileged action mid-flight.
      id: "kind",
      header: "Kind",
      cell: (item) =>
        item.kind === "plan_approval" ? (
          <Badge variant="default">Plan gate</Badge>
        ) : (
          <Badge variant="secondary">Step approval</Badge>
        ),
    },
    {
      id: "waitingSince",
      header: "Waiting since",
      hideOnMobile: true,
      cell: (item) => {
        if (!item.waitingSince) {
          return <span className="text-sm text-muted-foreground">—</span>;
        }
        const rel = formatRelativeTime(item.waitingSince);
        const abs = formatDateTime(item.waitingSince);
        return (
          <span className="text-sm text-muted-foreground" title={abs}>
            {rel || "—"}
          </span>
        );
      },
    },
    {
      id: "message",
      header: "Message",
      cell: (item) => {
        if (!item.message) {
          return <span className="text-sm text-muted-foreground">—</span>;
        }
        // Truncate long plan summaries — the full plan is on the detail page.
        const truncated =
          item.message.length > 120
            ? `${item.message.slice(0, 120)}…`
            : item.message;
        return (
          <span className="text-sm text-muted-foreground" title={item.message}>
            {truncated}
          </span>
        );
      },
    },
    {
      id: "rootRunId",
      header: "Part of",
      hideOnMobile: true,
      cell: (item) => {
        if (!item.rootRunId) {
          return <span className="text-sm text-muted-foreground">—</span>;
        }
        return (
          <span className="text-xs text-muted-foreground">
            part of{" "}
            <Link
              to={`/runs/${encodeURIComponent(item.rootRunId)}`}
              className="font-mono text-primary underline-offset-2 hover:underline"
            >
              {item.rootRunId}
            </Link>
          </span>
        );
      },
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="approvals-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Approvals</h2>
          <p className="text-sm text-muted-foreground">
            Runs paused awaiting approval in this namespace — click a run to review and approve or
            deny. Includes both workflow plan gates and mid-run step approvals. Switch the namespace
            scope with the global namespace selector. Auto-refreshes every 30s.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={manualRefresh}
          disabled={loadState.kind === "loading" || loadState.kind === "no-namespace" || refreshing}
          data-testid="approvals-refresh"
          className="shrink-0"
        >
          <RefreshCw className={`mr-2 h-4 w-4${refreshing ? " animate-spin" : ""}`} />
          {refreshing ? "Refreshing…" : "Refresh"}
        </Button>
      </div>

      <DataTable<ApprovalQueueItem>
        columns={columns}
        rows={items}
        rowKey={(item) => item.runId}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="Approvals"
        empty={
          loadState.kind === "no-namespace"
            ? {
                icon: CheckSquare,
                title: "Select a namespace",
                description:
                  "Choose a namespace from the selector above to see the runs awaiting approval there.",
              }
            : {
                icon: CheckSquare,
                title: "No runs are awaiting approval.",
                description:
                  "When a run pauses for plan approval or a mid-run step approval it will appear here. Approve or deny from the run's detail page.",
              }
        }
      />
    </div>
  );
}
