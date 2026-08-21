import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { CheckSquare } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type ApprovalQueueItem } from "@/lib/api";

// ApprovalsPage — the V5 "Plan approvals" queue (M112).
//
// Backend: GET /api/approvals?namespace=<ns>
//   • namespace is REQUIRED — the backend returns 400 without it.
//   • The queue is namespace-scoped and persona-gated: a caller without
//     `list workflows` in the namespace gets a 403, never an empty list.
//   • Namespace comes from the GLOBAL namespace selector (same as alerts-page).
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
//   approvals-table — the DataTable (via aria-label="Plan approvals")

type LoadState =
  | { kind: "loading" }
  | { kind: "no-namespace" }
  | { kind: "ready"; items: ApprovalQueueItem[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function ApprovalsPage() {
  const { namespace } = useNamespace();
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    // The queue is namespace-scoped and the backend REQUIRES a namespace (unlike the
    // cluster-wide alerts feed). When none is selected (the default "all" scope), prompt
    // the user to pick one rather than firing a request that 400s — a select-a-namespace
    // state is the honest UX, not an error.
    if (!namespace) {
      setLoadState({ kind: "no-namespace" });
      return;
    }
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    api
      .listApprovals(namespace, controller.signal)
      .then((items) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [namespace]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

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
      header: "Tree context",
      hideOnMobile: true,
      cell: (item) => {
        if (!item.rootRunId) {
          return <span className="text-sm text-muted-foreground">—</span>;
        }
        return (
          <span className="text-xs text-muted-foreground">
            in tree{" "}
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
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Plan approvals</h2>
        <p className="text-sm text-muted-foreground">
          Runs paused awaiting plan approval in this namespace — click a run to
          review and approve or deny. Switch the namespace scope with the global
          namespace selector.
        </p>
      </div>

      <DataTable<ApprovalQueueItem>
        columns={columns}
        rows={items}
        rowKey={(item) => item.runId}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="Plan approvals"
        empty={
          loadState.kind === "no-namespace"
            ? {
                icon: CheckSquare,
                title: "Select a namespace",
                description:
                  "Choose a namespace from the selector above to see the runs awaiting plan approval there.",
              }
            : {
                icon: CheckSquare,
                title: "No runs are awaiting approval.",
                description:
                  "When a workflow run pauses for plan approval it will appear here. Approve or deny from the run's detail page.",
              }
        }
      />
    </div>
  );
}
