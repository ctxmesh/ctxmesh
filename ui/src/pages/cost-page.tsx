import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Coins, Download } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select } from "@/components/ui/select";
import { DataTable, type Column, type DataTableError } from "@/components/kit";
import {
  api,
  ApiError,
  type AgentCostItem,
  type CostForecastResponse,
  type CostSummary,
  type TenantSummary,
} from "@/lib/api";

// CostPage — the cost drill-down surface (m16.10).
//
// Backend: GET /api/cost/breakdown?by=agent&tenant=&limit=&cursor=
//   Returns { agents: AgentCostItem[], total: CostSummary, nextCursor }.
//   The figures are a RECENT-WINDOW rollup (≤200 recent traces) — NOT
//   all-time spend. The UI makes this explicit via a caveat note.
//
// Tenant scoping (ADR 0077, m86): the breakdown + forecast are tenant-scoped —
//   ?tenant= is REQUIRED (a missing tenant is a 400). The page self-serves that
//   choice with an in-page tenant picker (below) driven by GET /api/tenants:
//     • on load with no ?tenant=, if tenants exist we default to the FIRST one
//       (the page is immediately useful, no dead-end);
//     • the picker writes the selection back into ?tenant= so the view is
//       linkable / refreshable, and re-fetches breakdown + forecast;
//     • ONLY when there are genuinely zero tenants do we show the calm empty
//       state — and even then we never fire a guaranteed-400 tenant-less call.
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
              Traced calls
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

// ForecastCard renders the month-to-date spend + linear run-rate projected month-end
// from the durable cost-rollup ledger (M70, ADR 0063 D3).
// 501-calm discipline: null forecast ⇒ card is not rendered (store not enabled).
function ForecastCard({
  forecast,
  tenant,
}: {
  forecast: CostForecastResponse | null;
  tenant: string;
}) {
  if (!forecast) return null;

  // Build the chargeback download URL for the current calendar month (YYYY-MM).
  const now = new Date();
  const period = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}`;
  const csvUrl = api.costChargebackCSVUrl(tenant, period);

  return (
    <Card data-testid="cost-forecast-card">
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <CardTitle className="text-base">Month forecast</CardTitle>
          <a
            href={csvUrl}
            download={`chargeback-${period}.csv`}
            data-testid="cost-chargeback-download"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
          >
            <Download className="h-3 w-3" />
            Download chargeback CSV
          </a>
        </div>
      </CardHeader>
      <CardContent>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Month to date
            </p>
            <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {formatUSD(forecast.monthToDateUSD)}
            </p>
          </div>
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Projected month end
            </p>
            <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {forecast.projectedMonthEndUSD > 0
                ? formatUSD(forecast.projectedMonthEndUSD)
                : "—"}
            </p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Linear run-rate estimate
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
  | { kind: "no-tenant" } // ADR 0077 — breakdown requires ?tenant=; none selected yet
  | { kind: "unavailable" } // 501 — Langfuse not configured
  | { kind: "degraded"; message: string } // 200 + notice — trace store transiently down
  | { kind: "error"; message: string; forbidden: boolean };

// TenantsState is the tenant PICKER's own load state (ADR 0077, m86) — kept
// distinct from the breakdown's LoadState. `forbidden` is a first-class outcome:
// a viewer who can't list tenants (403) shouldn't see a broken-looking picker.
type TenantsState =
  | { kind: "loading" }
  | { kind: "ready"; tenants: TenantSummary[] }
  | { kind: "forbidden" }
  | { kind: "error"; message: string };

// TenantPicker — the in-page dropdown that drives the cost view's tenant. It
// reuses the shell's Select primitive (matching the Workspace switcher) rather
// than inventing a new control. A tenant is a DIFFERENT concept from a namespace
// (cluster-scoped quota grouping vs a single namespace), so this is its own
// picker, not the namespace one.
function TenantPicker({
  tenant,
  tenants,
  onChange,
}: {
  tenant: string;
  tenants: TenantSummary[];
  onChange: (name: string) => void;
}) {
  return (
    <div className="flex items-center gap-2">
      <label
        htmlFor="cost-tenant-picker"
        className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground"
      >
        Tenant
      </label>
      <Select
        id="cost-tenant-picker"
        aria-label="Tenant"
        data-testid="cost-tenant-picker"
        value={tenant}
        onChange={(e) => onChange(e.target.value)}
        className="h-8 w-44 text-xs"
      >
        {tenants.map((t) => (
          <option key={t.name} value={t.name}>
            {t.name}
          </option>
        ))}
      </Select>
    </div>
  );
}

// emptyCostSummary is the zero-value CostSummary used while loading.
const emptyCostSummary: CostSummary = {
  totalCostUSD: 0,
  totalTokens: 0,
  observations: 0,
  byModel: [],
};

export function CostPage() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();

  // ?tenant= drives the whole tenant-scoped view (breakdown + forecast). The
  // in-page picker writes it back here so the view stays linkable/refreshable.
  const tenant = searchParams.get("tenant") ?? "";

  // The tenant picker's options (ADR 0077, m86). Fetched once via GET /api/tenants.
  const [tenantsState, setTenantsState] = useState<TenantsState>({ kind: "loading" });

  // Forecast state: null = 501 (store not enabled) or no tenant given; undefined = not yet loaded.
  const [forecast, setForecast] = useState<CostForecastResponse | null | undefined>(
    undefined,
  );

  // setTenant drives the view by writing the selection into ?tenant= (replace, not
  // push — flipping the tenant isn't a distinct history step). Pagination resets to
  // page 0 for the new tenant.
  const setTenant = useCallback(
    (name: string) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (name) next.set("tenant", name);
          else next.delete("tenant");
          return next;
        },
        { replace: true },
      );
    },
    [setSearchParams],
  );

  // Load the tenant list once for the picker. Best-effort: a 403 (viewer can't list
  // tenants) is an honest "forbidden", distinct from an authentically empty list.
  useEffect(() => {
    const controller = new AbortController();
    setTenantsState({ kind: "loading" });
    api
      .listTenants(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setTenantsState({ kind: "ready", tenants: res.items ?? [] });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setTenantsState({ kind: "forbidden" });
          return;
        }
        setTenantsState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load tenants",
        });
      });
    return () => controller.abort();
  }, []);

  // Default-to-first: when the page lands with no ?tenant= and tenants exist, pick
  // the first so the page is immediately useful instead of dead-ending. Genuinely
  // zero tenants keeps the calm empty state (handled in render, below).
  useEffect(() => {
    if (tenant) return;
    if (tenantsState.kind === "ready" && tenantsState.tenants.length > 0) {
      setTenant(tenantsState.tenants[0].name);
    }
  }, [tenant, tenantsState, setTenant]);

  // Cursor pagination: stack of cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  // Switching tenant resets pagination — a cursor from one tenant is meaningless
  // for another. Reset to page 0 whenever the tenant changes.
  useEffect(() => {
    setPageStack([""]);
  }, [tenant]);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    // ADR 0077: the breakdown is tenant-scoped and REQUIRES ?tenant= when tenants exist. M99 B1
    // zero-tenant fallback: on a cluster with genuinely NO tenants the BFF serves the cluster-wide
    // per-agent breakdown for an empty tenant, so we FETCH it (not a dead-end empty state). While the
    // tenant list is still loading, forbidden, or non-empty (the picker's default-to-first will select
    // one), keep the calm no-tenant state.
    if (!tenant) {
      const zeroTenants =
        tenantsState.kind === "ready" && tenantsState.tenants.length === 0;
      if (!zeroTenants) {
        setLoadState({ kind: "no-tenant" });
        return;
      }
      // else: fall through and fetch with an empty tenant → the cluster-wide breakdown.
    }

    setLoadState({ kind: "loading" });

    api
      .costBreakdown(
        tenant,
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
        // A `notice` means the trace store is transiently down (200 + empty, m23.6):
        // show a "temporarily unavailable — retry" state, NOT the misleading "no cost
        // data yet" empty (which implies zero activity when the source just timed out).
        if (res.notice) {
          setLoadState({ kind: "degraded", message: res.notice });
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
  }, [cursor, tenant, tenantsState]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Forecast: fetch when ?tenant= is present. 501 → null (calm). Errors are swallowed
  // (the breakdown table is the primary surface; forecast is secondary and optional).
  useEffect(() => {
    if (!tenant) {
      setForecast(null);
      return;
    }
    const ctrl = new AbortController();
    api
      .costForecast(tenant, ctrl.signal)
      .then((res) => {
        if (!ctrl.signal.aborted) setForecast(res);
      })
      .catch(() => {
        // Forecast errors are non-fatal: the page still works without the card.
        if (!ctrl.signal.aborted) setForecast(null);
      });
    return () => ctrl.abort();
  }, [tenant]);

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

  const tenants = tenantsState.kind === "ready" ? tenantsState.tenants : [];

  // The page header — title + subtitle + (when there is a tenant to switch
  // between) the in-page tenant picker. Shared across every state so the picker
  // is always reachable, not just on the happy path.
  const header = (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Cost</h2>
        <p className="text-sm text-muted-foreground">
          Per-agent spend within a tenant, from recent activity.
        </p>
      </div>
      {tenants.length > 0 && (
        <TenantPicker tenant={tenant} tenants={tenants} onChange={setTenant} />
      )}
    </div>
  );

  // No-tenant calm state — ONLY reached when there are genuinely zero tenants (a
  // fresh cluster). With tenants present the default-to-first effect selects one,
  // so this is never a dead-end. A can't-list-tenants 403 also lands here, since
  // without the list we cannot self-serve a selection.
  if (loadState.kind === "no-tenant") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
        {header}
        <div
          className="flex h-40 flex-col items-center justify-center gap-3 rounded-lg border bg-card px-6 text-center text-sm text-muted-foreground"
          data-testid="cost-no-tenant"
        >
          {tenantsState.kind === "forbidden" ? (
            <p>
              Cost is grouped by tenant. You don&apos;t have permission to list
              tenants — ask an operator for access.
            </p>
          ) : (
            <>
              <p>
                Cost is grouped by tenant, and this cluster has no tenants yet.
                Create a tenant to start tracking per-agent spend.
              </p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => navigate("/tenants")}
                data-testid="cost-create-tenant"
              >
                Create a tenant
              </Button>
            </>
          )}
        </div>
      </div>
    );
  }

  // 501 calm state — Langfuse not configured.
  if (loadState.kind === "unavailable") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
        {header}
        <div
          className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
          data-testid="cost-unavailable"
        >
          Cost data unavailable — tracing not configured (Langfuse not wired).
        </div>
      </div>
    );
  }

  // 200 + notice — the trace store is transiently unavailable (slow/circuit-broken).
  // Honest degrade: distinct from "no data" so the user knows to retry, not that
  // there is zero activity (m24 — the notice was previously dropped).
  if (loadState.kind === "degraded") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
        {header}
        <div
          className="flex h-40 flex-col items-center justify-center gap-3 rounded-lg border bg-card px-6 text-center text-sm text-muted-foreground"
          data-testid="cost-degraded"
        >
          <span>{loadState.message}</span>
          <Button variant="outline" size="sm" onClick={load}>
            Retry
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="cost-page">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Cost</h2>
          <p className="text-sm text-muted-foreground">
            Per-agent cost breakdown.{" "}
            <span className="font-medium text-foreground">
              Costs reflect a recent window of activity, not all-time spend.
            </span>
          </p>
        </div>
        {tenants.length > 0 && (
          <TenantPicker tenant={tenant} tenants={tenants} onChange={setTenant} />
        )}
      </div>

      {/* M99 B1: when the cluster has no tenants we show the cluster-wide per-agent breakdown; make
          that explicit + offer to create a tenant for per-tenant tracking. */}
      {loadState.kind === "ready" && !tenant && (
        <div
          className="flex flex-wrap items-center justify-between gap-2 rounded-lg border bg-muted/30 px-4 py-2 text-xs text-muted-foreground"
          data-testid="cost-all-agents-note"
        >
          <span>
            No tenants yet — showing spend across <strong>all agents</strong>.
            Create a tenant for per-tenant cost + budgets.
          </span>
          <Button
            variant="outline"
            size="sm"
            onClick={() => navigate("/tenants")}
            data-testid="cost-create-tenant"
          >
            Create a tenant
          </Button>
        </div>
      )}

      {loadState.kind === "ready" && (
        <CostSummaryCard total={total} />
      )}

      {/* Forecast card: shown when ?tenant= is present and the store is enabled.
          null = 501 or no tenant → card hidden (calm). undefined = still loading → hidden. */}
      {forecast != null && tenant && (
        <ForecastCard forecast={forecast} tenant={tenant} />
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
