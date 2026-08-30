import { useCallback, useEffect, useRef, useState } from "react";
import { Bell, Plus } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NewAlertPolicyDialog } from "@/components/dashboard/new-alert-policy-dialog";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type AlertSummary } from "@/lib/api";

// AlertsPage — the fired-alert console feed (M70, ADR 0063 D2).
//
// Backend: GET /api/alerts?namespace=&limit=
//   • namespace comes from the GLOBAL namespace selector.
//   • The feed is newest-first, limit-bounded (server-side). No keyset cursor for now
//     (the alertstore.List contract is a simple bounded slice). A cursor is a
//     follow-up if the table grows large.
//
// 501-calm / 403-forbidden / 5xx-error discipline (mirrors audit-page):
//   • 501 (alert store not configured): listAlerts() returns null → calm
//     "not enabled" empty state, NEVER an error toast.
//   • 403 (caller lacks `list alertpolicies`): ApiError.isForbidden →
//     the DataTable's forbidden variant, never a fake empty list.
//   • 500 + other non-2xx: a visible, retryable error state.
//
// The page is READ-only. The controller auto-resolves alerts on condition true→false
// transitions. A manual ack write path is deferred (follow-up task).
//
// data-testid contract:
//   alerts-page         — root container
//   alerts-table        — the DataTable (via aria-label="Fired alerts")
//   alerts-unavailable  — the 501 "not enabled" state

const PAGE_LIMIT = 50;

function fmtTimestamp(ts: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

// firingVariant maps alert status to a Badge variant.
function firingVariant(firing: boolean): "destructive" | "success" {
  return firing ? "destructive" : "success";
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: AlertSummary[] }
  | { kind: "unavailable" } // 501 — alert store not configured
  | { kind: "error"; message: string; forbidden: boolean };

export function AlertsPage() {
  const { namespace } = useNamespace();
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const [newPolicyOpen, setNewPolicyOpen] = useState(false);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    api
      .listAlerts(
        {
          limit: PAGE_LIMIT,
          ...(namespace ? { namespace } : {}),
        },
        controller.signal,
      )
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (alert store not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        setLoadState({ kind: "ready", items: res.items });
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
          resource: "alerts",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<AlertSummary>[] = [
    {
      id: "firedAt",
      header: "Fired",
      cell: (a) => (
        <span className="text-sm text-muted-foreground">{fmtTimestamp(a.firedAt)}</span>
      ),
    },
    {
      id: "status",
      header: "Status",
      cell: (a) => (
        <Badge variant={firingVariant(a.firing)}>{a.firing ? "Firing" : "Resolved"}</Badge>
      ),
    },
    {
      id: "policy",
      header: "Policy",
      cell: (a) => <span className="font-medium">{a.policy}</span>,
    },
    {
      id: "condition",
      header: "Condition",
      cell: (a) => <span className="font-mono text-xs">{a.condition}</span>,
    },
    {
      id: "type",
      header: "Type",
      cell: (a) => <span className="text-sm text-muted-foreground">{a.type}</span>,
    },
    {
      id: "value",
      header: "Value",
      hideOnMobile: true,
      cell: (a) => (
        <span className="font-mono text-xs">{a.value || "—"}</span>
      ),
    },
    {
      id: "agent",
      header: "Agent",
      hideOnMobile: true,
      cell: (a) => (
        <span className="text-sm text-muted-foreground">{a.agent || "—"}</span>
      ),
    },
    {
      id: "namespace",
      header: "Namespace",
      hideOnMobile: true,
      cell: (a) => (
        <span className="text-sm text-muted-foreground">{a.namespace || "—"}</span>
      ),
    },
    {
      id: "resolvedAt",
      header: "Resolved",
      hideOnMobile: true,
      cell: (a) => (
        <span className="text-sm text-muted-foreground">
          {a.resolvedAt ? fmtTimestamp(a.resolvedAt) : "—"}
        </span>
      ),
    },
  ];

  // 501 calm state — the alert store is not configured (no control-plane DSN).
  if (loadState.kind === "unavailable") {
    return (
      <div className="mx-auto max-w-6xl space-y-6" data-testid="alerts-page">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Alerts</h2>
          <p className="text-sm text-muted-foreground">
            Fired alert conditions from your AlertPolicy rules.
          </p>
        </div>
        <div
          className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
          data-testid="alerts-unavailable"
        >
          Alerts unavailable — the alert store is not configured (control-plane database not wired).
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="alerts-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Alerts</h2>
          <p className="text-sm text-muted-foreground">
            Fired alert conditions from your AlertPolicy rules — newest first. Switch the namespace
            scope with the global namespace selector.
          </p>
        </div>
        <Button
          size="sm"
          onClick={() => setNewPolicyOpen(true)}
          data-testid="new-alert-policy-open"
          className="shrink-0"
        >
          <Plus className="mr-2 h-4 w-4" />
          New alert policy
        </Button>
      </div>

      <DataTable<AlertSummary>
        columns={columns}
        rows={items}
        rowKey={(a) => String(a.id)}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="Fired alerts"
        empty={{
          icon: Bell,
          title: "No alerts",
          description:
            "No alert conditions have fired in this namespace yet. Define an AlertPolicy so a condition can fire here — start with New alert policy above.",
        }}
      />

      <NewAlertPolicyDialog
        open={newPolicyOpen}
        onClose={() => setNewPolicyOpen(false)}
        namespace={namespace}
      />
    </div>
  );
}
