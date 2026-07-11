import * as React from "react";
import { CheckCircle, Clock, XCircle } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useToast,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { api, ApiError, type McpApproval } from "@/lib/api";

// McpApprovalsPage — the operator-only view of pending MCP servers awaiting
// approval. On a hardened install, user-submitted MCP servers queue here;
// an operator approves (binds the server to the catalog) or rejects (removes
// the pending entry).
//
// RBAC: approve/reject are OPERATOR-ONLY. The UI gates display on `can(RES_REGISTRIES,
// "update")` — operators have update; a viewer does NOT and sees neither button.
// The REAL gate is the API: a forced attempt gets a 403 surfaced honestly.
//
// Empty state: an empty approval queue is NORMAL (no MCPs pending). The page
// shows a calm empty state, not an error.
//
// 501 from GET /api/mcp/approvals = approval queue not enabled on this install
// (rendered as the "disabled" empty state, not an error).

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; approvals: McpApproval[] }
  | { kind: "empty" }
  | { kind: "disabled" } // 501 = feature not enabled
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

interface ActionState {
  kind: "idle" | "approving" | "rejecting" | "confirm-reject";
  ns: string;
  name: string;
  error?: string;
}

const ACTION_IDLE: ActionState = { kind: "idle", ns: "", name: "" };

export function McpApprovalsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [action, setAction] = React.useState<ActionState>(ACTION_IDLE);

  const { can, reprobe } = useCapabilities();
  // Operator gate: only update permission holders see approve/reject actions.
  // A viewer sees the queue (list) but not the action buttons — the real gate
  // is the API 403 if they somehow invoke the action.
  const canApprove = can(RES_REGISTRIES, "update");

  const { toast } = useToast();

  const load = React.useCallback((signal?: AbortSignal) => {
    setPage({ kind: "loading" });
    api
      .mcpApprovals(signal)
      .then((res) => {
        if (signal?.aborted) return;
        // The BFF returns the pending servers under `items` (list-contract key);
        // fall back to `servers`, and default to [] so an unexpected shape can
        // never crash on `.length` (the integration-shape bug this fixes).
        const rows = res.items ?? res.servers ?? [];
        if (rows.length === 0) {
          setPage({ kind: "empty" });
        } else {
          setPage({ kind: "ready", approvals: rows });
        }
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError) {
          if (err.status === 501) { setPage({ kind: "disabled" }); return; }
          if (err.isForbidden) { setPage({ kind: "forbidden", message: err.message }); return; }
          setPage({ kind: "error", message: err.message });
          return;
        }
        setPage({ kind: "error", message: err instanceof Error ? err.message : "request failed" });
      });
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  async function onApprove(ns: string, name: string) {
    setAction({ kind: "approving", ns, name });
    try {
      await api.approveMcp(ns, name);
      toast({ variant: "success", title: `Approved ${name}`, description: "The MCP server is now in the catalog." });
      load();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setAction({ kind: "idle", ns: "", name: "", error: `Not allowed to approve: ${err.message}` });
          return;
        }
        setAction({ kind: "idle", ns: "", name: "", error: err.message });
        return;
      }
      setAction({ kind: "idle", ns: "", name: "", error: err instanceof Error ? err.message : "approve failed" });
    }
  }

  async function onReject(ns: string, name: string) {
    setAction({ kind: "rejecting", ns, name });
    try {
      await api.rejectMcp(ns, name);
      toast({ variant: "success", title: `Rejected ${name}`, description: "The pending MCP server has been removed." });
      load();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setAction({ kind: "idle", ns: "", name: "", error: `Not allowed to reject: ${err.message}` });
          return;
        }
        setAction({ kind: "idle", ns: "", name: "", error: err.message });
        return;
      }
      setAction({ kind: "idle", ns: "", name: "", error: err instanceof Error ? err.message : "reject failed" });
    }
  }

  const busy = action.kind === "approving" || action.kind === "rejecting";

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="mcp-approvals">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">MCP approval queue</h2>
        <p className="text-sm text-muted-foreground">
          Pending MCP servers submitted by users — approve to add them to the
          catalog, or reject to remove them.
        </p>
      </div>

      {action.error && (
        <p
          className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
          role="alert"
          data-testid="action-error"
        >
          {action.error}
        </p>
      )}

      {page.kind === "loading" && <SkeletonTable rows={4} />}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to view the approval queue"
          description="Reading the MCP approval queue requires operator-level permissions on this cluster."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load the approval queue"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "disabled" && (
        <EmptyState
          title="Approval queue not enabled"
          description="This install doesn't have the MCP approval queue feature enabled. Contact your operator."
        />
      )}

      {page.kind === "empty" && (
        <EmptyState
          title="No pending approvals"
          description="The approval queue is empty — no MCP servers are waiting for operator review."
        />
      )}

      {page.kind === "ready" && (
        <div className="rounded-lg border bg-card shadow-card divide-y">
          {page.approvals.map((a) => (
            <ApprovalRow
              key={`${a.namespace}/${a.name}`}
              approval={a}
              canApprove={canApprove}
              busy={busy && action.ns === a.namespace && action.name === a.name}
              onApprove={() => void onApprove(a.namespace, a.name)}
              onRejectRequest={() =>
                setAction({ kind: "confirm-reject", ns: a.namespace, name: a.name })
              }
            />
          ))}
        </div>
      )}

      {/* Reject confirmation dialog — prevent accidental rejection. */}
      <ConfirmDialog
        open={action.kind === "confirm-reject"}
        onCancel={() => setAction(ACTION_IDLE)}
        onConfirm={() => {
          const { ns, name } = action;
          void onReject(ns, name);
        }}
        title="Reject this MCP server?"
        description={
          action.kind === "confirm-reject"
            ? `Rejecting "${action.name}" removes it from the approval queue permanently.`
            : undefined
        }
        confirmLabel="Reject"
        destructive
      />
    </div>
  );
}

interface ApprovalRowProps {
  approval: McpApproval;
  canApprove: boolean;
  busy: boolean;
  onApprove: () => void;
  onRejectRequest: () => void;
}

function ApprovalRow({
  approval,
  canApprove,
  busy,
  onApprove,
  onRejectRequest,
}: ApprovalRowProps) {
  const { namespace: ns, name } = approval;
  const testBase = `${ns}-${name}`;

  return (
    <div
      className="flex items-start gap-4 px-4 py-3"
      data-testid={`mcp-approval-row-${testBase}`}
    >
      <Clock className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm font-medium">{name}</span>
          <Badge variant="secondary" className="text-[10px]">
            {ns}
          </Badge>
          <Badge variant="warning" className="text-[10px]">
            pending
          </Badge>
        </div>
        {approval.url && (
          <p className="font-mono text-xs text-muted-foreground">{approval.url}</p>
        )}
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          {approval.submittedBy && <span>by {approval.submittedBy}</span>}
          {approval.submittedAt && (
            <span>{new Date(approval.submittedAt).toLocaleString()}</span>
          )}
          {approval.toolCount !== undefined && (
            <span>{approval.toolCount} tool{approval.toolCount === 1 ? "" : "s"} discovered</span>
          )}
        </div>
      </div>
      {canApprove && (
        <div className="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={onApprove}
            data-testid={`mcp-approve-${testBase}`}
          >
            <CheckCircle className="mr-1.5 h-3.5 w-3.5 text-success" />
            Approve
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={onRejectRequest}
            data-testid={`mcp-reject-${testBase}`}
          >
            <XCircle className="mr-1.5 h-3.5 w-3.5 text-destructive" />
            Reject
          </Button>
        </div>
      )}
    </div>
  );
}
