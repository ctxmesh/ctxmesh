import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Coins } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type AgentCostItem, type CostSummary } from "@/lib/api";

// CostPage — the cost drill-down surface (m16.10).
//
// Backend: GET /api/cost/breakdown?by=agent&limit=&cursor=
//   Returns { agents: AgentCostItem[], total: CostSummary, nextCursor }.
//   The figures are a RECENT-WINDOW rollup (≤200 recent traces) — NOT
//   all-time spend. The UI makes this explicit via a caveat note.
//
// 501-calm / 502-error discipline:
//   • 501 (Langfuse not configured): costBreakdown() returns null → calm
//     "unavailable" state, NEVER an error.
//   • 502 (Langfuse configured, upstream fetch FAILED): throws ApiError →
//     surfaced as a visible error state (retryable), never hidden.
//
// (untagged) bucket:
//   Traces with no agent tag arrive as agentName="(untagged)". The row is
//   rendered normally but is NON-NAVIGABLE (no agent detail page exists for
//   untagged traces). A normal agent row click → /agents/:ns/:name.
//
// RBAC: read-only page — the API is the gate (ADR 0011). No write affordances.
//
// data-testid contract:
//   cost-page             — root container
//   cost-summary-card     — the total cost summary card
//   cost-breakdown-table  — the DataTable (via aria-label="Cost breakdown")
//   cost-row-{ns}-{name}  — each table row (via rowKey)

const PAGE_LIMIT = 50;

// UNTAGGED is the sentinel agentName for traces with no agent tag.
const UNTAGGED = "(untagged)";

function formatUSD(usd: number): string {
  if (usd === 0) return "$0.00";
  if (usd < 0.001) return `$${usd.toFixed(5)}`;
  return `$${usd.toFixed(3)}`;
}

function formatCompact(n: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact" }).format(n);
}

// CostSummaryCard renders the window-level rollup — total cost + total tokens.
// The recent-window caveat is surfaced here and in the page subtitle.
function CostSummaryCard({ total }: { total: CostSummary }) {
  return (
    <Card data-testid="cost-summary-card">
      <CardHeader>
        <CardTitle className="text-base">Window summary</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="grid gap-4 sm:grid-cols-3">
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Total cost
            </p>
            <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {formatUSD(total.totalCostUSD)}
            </p>
          </div>
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Total tokens
            </p>
            <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {formatCompact(total.totalTokens)}
            </p>
          </div>
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Observations
            </p>
            <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {formatCompact(total.observations)}
            </p>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; agents: AgentCostItem[]; total: CostSummary; nextCursor: string }
  | { kind: "unavailable" } // 501 — Langfuse not configured
  | { kind: "error"; message: string; forbidden: boolean };

// emptyCostSummary is the zero-value CostSummary used while loading.
const emptyCostSummary: CostSummary = {
  totalCostUSD: 0,
  totalTokens: 0,
  observations: 0,
  byModel: [],
};

export function CostPage() {
  const navigate = useNavigate();

  // Cursor pagination: stack of cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    api
      .costBreakdown(
        { limit: PAGE_LIMIT, ...(cursor ? { cursor } : {}) },
        controller.signal,
      )
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (Langfuse not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        setLoadState({
          kind: "ready",
          agents: res.agents,
          total: res.total,
          nextCursor: res.nextCursor,
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 502 and other non-501 errors surface as a real error (retryable).
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [cursor]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const agents = loadState.kind === "ready" ? loadState.agents : [];
  const total = loadState.kind === "ready" ? loadState.total : emptyCostSummary;
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR (BFF), NEVER off agents.length.
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;

  function onNext() {
    if (!hasNext) return;
    setPageStack((s) => [...s, nextCursor]);
  }
  function onPrev() {
    if (!hasPrev) return;
    setPageStack((s) => s.slice(0, -1));
  }

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<AgentCostItem>[] = [
    {
      id: "agent",
      header: "Agent",
      cell: (item) => {
        const isUntagged = item.agentName === UNTAGGED;
        // The testid uses the raw ns/name for lookup in tests; sanitize "/" in
        // the untagged sentinel to avoid weird attribute values.
        const testNs = item.agentNs.replace(/[^a-z0-9-]/gi, "_") || "_";
        const testName = item.agentName.replace(/[^a-z0-9-]/gi, "_") || "_";
        return (
          <span
            className={isUntagged ? "italic text-muted-foreground" : "font-medium"}
            data-testid={`cost-row-${testNs}-${testName}`}
          >
            {isUntagged ? "(untagged)" : `${item.agentNs}/${item.agentName}`}
          </span>
        );
      },
    },
    {
      id: "totalCostUSD",
      header: "Cost",
      className: "text-right",
      cell: (item) => (
        <span className="tabular-nums">{formatUSD(item.totalCostUSD)}</span>
      ),
    },
    {
      id: "totalTokens",
      header: "Tokens",
      className: "text-right",
      cell: (item) => (
        <span className="tabular-nums">{item.totalTokens.toLocaleString()}</span>
      ),
    },
    {
      id: "runCount",
      header: "Runs",
      className: "text-right",
      cell: (item) => (
        <span className="tabular-nums">{item.runCount.toLocaleString()}</span>
      ),
    },
  ];

  // 501 calm state — Langfuse not configured.
  if (loadState.kind === "unavailable") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Cost</h2>
          <p className="text-sm text-muted-foreground">
            Per-agent cost breakdown from recent activity.
          </p>
        </div>
        <div
          className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
          data-testid="cost-unavailable"
        >
          Cost data unavailable — tracing not configured (Langfuse not wired).
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Cost</h2>
        <p className="text-sm text-muted-foreground">
          Per-agent cost breakdown.{" "}
          <span className="font-medium text-foreground">
            Costs reflect a recent window of activity, not all-time spend.
          </span>
        </p>
      </div>

      {loadState.kind === "ready" && (
        <CostSummaryCard total={total} />
      )}

      <div data-testid="cost-breakdown-table">
        <DataTable<AgentCostItem>
          columns={columns}
          rows={agents}
          rowKey={(item) => `${item.agentNs}-${item.agentName}`}
          loading={loadState.kind === "loading"}
          error={error}
          hasPrev={hasPrev}
          hasNext={hasNext}
          onPrev={onPrev}
          onNext={onNext}
          rangeLabel={`Page ${pageNumber}`}
          ariaLabel="Cost breakdown"
          // Row click: navigate to the agent detail page — but NOT for the (untagged)
          // bucket (no agent to navigate to; it's a no-op row).
          onRowClick={(item) => {
            if (item.agentName === UNTAGGED) return;
            navigate(
              `/agents/${encodeURIComponent(item.agentNs)}/${encodeURIComponent(item.agentName)}`,
            );
          }}
          empty={{
            icon: Coins,
            title: "No cost data yet",
            description:
              "No cost data in the recent activity window. Run agents via the Playground to see cost breakdown here.",
          }}
        />
      </div>
    </div>
  );
}
