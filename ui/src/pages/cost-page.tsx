import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Coins, Download, Filter } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  ErrorState,
  FilterChipRow,
  Meter,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  UNKNOWN,
  nextStepRank,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type Quantity,
} from "@/components/kit";
import {
  api,
  ApiError,
  type AgentCostItem,
  type CostForecastResponse,
  type CostSummary,
  type TenantSummary,
} from "@/lib/api";

// CostPage — the money page (m16.10; re-housed on the editorial system in M151,
// spec §6.2 "stat-band variant": stat band + budget Meter above the table, the
// §4.4 Cost column budget in it, and the recent-window caveat as a QuietNote).
//
// ── THE PAGE'S ONE IDEA: MONEY IS WHAT THIS CONSOLE IS LEAST ALLOWED TO LIE
//    ABOUT (§4.5) ───────────────────────────────────────────────────────────
// Every figure here is either measured or it says it is not. Money is never
// truncated, never wrapped, never elided; it is right-aligned mono tabular; a
// sub-dollar amount keeps four decimals so $0.0330 does not round away to
// nothing, and a dollar amount keeps two. An unmeasured figure renders the
// honest dash with its reason on hover — NEVER `$0.0000`, because a zero we
// measured and a figure we never measured must not share a glyph (§7.1).
//
// ── WHAT EACH FIGURE ON THIS PAGE ACTUALLY MEASURES ─────────────────────────
// Read `internal/bff/handlers.go:handleCostBreakdown` before touching this:
// the response's `total` means two DIFFERENT things depending on the scope,
// and the page must not paper over the difference.
//
//   • WITH a tenant (the normal path): the BFF filters the cluster-wide
//     breakdown down to the tenant's namespaces and RECOMPUTES `total` as the
//     sum of the kept rows. So the totals and the rows are the same window and
//     the rows do add up to the total — but `observations` is never computed
//     on that path and arrives as 0. That 0 is NOT "no traced calls"; it is a
//     figure this path does not answer, so it renders as unknown, not as zero.
//   • WITHOUT a tenant (only reachable on a cluster with genuinely zero
//     tenants, M99 B1): `total` is the trace backend's 30-day daily-metrics
//     rollup while the rows are the last 100 traces. Two different windows —
//     the rows therefore do NOT sum to the total, and the QuietNote says so
//     rather than letting a reader do arithmetic that cannot work.
//
// The Share column divides by the spend of the ROWS IN HAND, never by that
// cross-window total, and it renders only when the loaded window is the whole
// list — a share computed from a partial page would be confidently wrong.
//
// ── THE BUDGET METER (§5.24) ────────────────────────────────────────────────
// Month-to-date spend comes from the durable cost-rollup ledger (the forecast
// endpoint) and the cap comes from the selected Tenant's `model.budgetUSD` —
// both are monthly, which is the only reason they may share a bar. No cap ⇒ no
// bar; no ledger ⇒ no fill. The Meter enforces both.
//
// ── 501-CALM / 502-ERROR DISCIPLINE (ADR 0012) ─────────────────────────────
//   • 501 (no trace backend): `costBreakdown` returns null → a QuietNote.
//     Nothing broke; this install simply cannot answer. Never an error.
//   • 200 + notice: the trace store is transiently down → a retryable error
//     state, NOT the "no cost data yet" empty (which would claim zero spend).
//   • 502: a real error, surfaced with Retry.
//   • The forecast returns null on 501 too — calm, and the card is absent.
//
// ── TENANT SCOPE (ADR 0077) ─────────────────────────────────────────────────
// /api/cost and /api/cost/breakdown are tenant-scoped and 400 without ?tenant=,
// so the page lists tenants FIRST and picks one. Consequences the page must
// state rather than hide: with tenants present it defaults to the first (never
// a dead end); with genuinely zero tenants the BFF serves the cluster-wide
// breakdown (M99 B1); and when the tenant list cannot be read at all — a 403 or
// a failed list — the page has nothing to scope by, so it says exactly that
// instead of rendering an empty table that would read as "no spend".
//
// data-testid contract:
//   cost-page             — root container
//   cost-tenant-picker    — the in-page tenant <select>
//   cost-summary-card     — the window stat band
//   cost-forecast-card    — the month-to-date band (Meter + projection + CSV)
//   cost-window-note      — the recent-window QuietNote
//   cost-breakdown-table  — the DataTable (aria-label="Cost breakdown")
//   cost-row-{ns}-{name}  — each table row's entity cell
//   cost-no-tenant        — the "nothing to scope by" state
//   cost-unavailable      — the 501 calm state
//   cost-degraded         — the transient trace-store outage state

const PAGE_LIMIT = 50;

// UNTAGGED is the sentinel agentName for traces that carry no agent tag.
const UNTAGGED = "(untagged)";

// The bounded windows the BFF actually reads, named once so the copy can never
// drift from the backend (internal/bff/langfuse.go: costBreakdownWindowLimit =
// maxRunLimit = 100; costWindowDays = 30).
const TRACE_WINDOW = 100;
const METRICS_WINDOW_DAYS = 30;

// The fraction of a cap at which this console calls a tenant "near cap" — the
// same 0.8 the tenants list flags at (m54.5), so one bound means one thing in
// both places. It is drawn as the Meter's tick, and amber is exactly §2.2's
// "a bound is near or crossed".
const NEAR_CAP_RATIO = 0.8;

/**
 * Money, in the §4.5 register: `< $1` keeps 4 decimals, `≥ $1` keeps 2.
 *
 * A MEASURED zero renders `$0.00` rather than `$0.0000`. The four decimals
 * exist to preserve significant digits a sub-dollar amount would otherwise lose
 * — zero has none to preserve, and `$0.0000` is the one string §7.1 names as
 * the thing an unknown must never be mistaken for. Keeping them visually apart
 * is worth more than mechanical consistency.
 *
 * Lives here rather than in `lib/format.ts` because that module's `formatUSD`
 * is the older 3/6-decimal register shared with surfaces outside this page's
 * scope; converging them is a separate change, not a silent side effect.
 */
export function formatMoney(usd: number): string {
  if (usd === 0) return "$0.00";
  return Math.abs(usd) < 1 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`;
}

/**
 * A share of the window's spend. One decimal is all the precision it earns —
 * except at the bottom, where rounding would turn real spend into `0.0%` and
 * make a measured share indistinguishable from nothing at all. Anything below
 * a tenth of a percent says so instead.
 */
function formatShare(pct: number): string {
  if (pct > 0 && pct < 0.05) return "<0.1%";
  return `${pct.toFixed(1)}%`;
}

/** Big counts read better compact in a stat band; the table keeps them exact. */
function formatCompact(n: number): string {
  return new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n);
}

// ── Triage: what a cost row asks of a person ────────────────────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Priced {
  item: AgentCostItem;
  /** Traces with no agent tag: real spend that belongs to nobody. */
  untagged: boolean;
  next: NextStep;
}

/**
 * One breakdown row → its next step.
 *
 * Exactly one thing on this page asks for a person: spend the trace backend
 * could not attribute to an agent. Everything else is a report — the row is
 * still clickable through to the agent, but inventing an errand for a healthy
 * spend line would make the column noise, and the column is the page's point.
 */
function triage(item: AgentCostItem): Priced {
  const untagged = item.agentName === UNTAGGED;
  return {
    item,
    untagged,
    next: untagged
      ? // The fix is a trace tag, which lives in the agent's instrumentation —
        // so the step is to go LOOK at the traces, which the runs list can do.
        { label: "Open the traces", tone: "default", to: "/runs" }
      : { tone: "none" },
  };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "all" | "attention" | "spending";

const VIEWS: { id: ViewId; label: string; match: (p: Priced) => boolean }[] = [
  { id: "all", label: "Everything", match: () => true },
  { id: "attention", label: "Needs a person", match: (p) => p.next.tone !== "none" },
  { id: "spending", label: "With spend", match: (p) => p.item.totalCostUSD > 0 },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  attention: {
    title: "Every dollar has a name",
    description:
      "All the spend in this window is attributed to a named agent — nothing here needs a person. Show everything to read the breakdown.",
  },
  spending: {
    title: "Nothing recorded spend",
    description:
      "No agent in this window recorded a cost. Show everything to see what was traced anyway.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every figure is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are not the whole list.
 */
export function closingLine(rows: Priced[], complete: boolean): string | null {
  if (rows.length === 0) return null;
  const named = rows.filter((p) => !p.untagged);
  const spend = rows.reduce((sum, p) => sum + p.item.totalCostUSD, 0);
  const loose = rows
    .filter((p) => p.untagged)
    .reduce((sum, p) => sum + p.item.totalCostUSD, 0);
  const where = complete ? "in this window" : "on this page";
  const head =
    named.length === 1
      ? `One agent drew on a model ${where}`
      : `${named.length} agents drew on a model ${where}`;
  if (loose > 0) {
    return `${head}, spending ${formatMoney(spend - loose)} between them. A further ${formatMoney(loose)} was traced without an agent tag and belongs to nobody.`;
  }
  if (named.length === 0) return null;
  return `${head}, spending ${formatMoney(spend)} between them — every dollar of it attributed to a name.`;
}

// ── The tenant picker ───────────────────────────────────────────────────────

// TenantPicker — the in-page dropdown that scopes the whole view. It reuses the
// shell's Select primitive (matching the Workspace switcher) rather than
// inventing a control. A tenant is a DIFFERENT concept from a namespace
// (cluster-scoped quota grouping vs one namespace), so this is its own picker.
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
        className="font-mono text-xs uppercase tracking-wide text-faint"
      >
        Tenant
      </label>
      <Select
        id="cost-tenant-picker"
        aria-label="Tenant"
        data-testid="cost-tenant-picker"
        value={tenant}
        onChange={(e) => onChange(e.target.value)}
        className="h-8 w-44 text-sm"
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

// ── The stat band ───────────────────────────────────────────────────────────

/** One figure in the band. The value is a Quantity, so an unknown cannot be drawn as a number. */
function Stat({
  label,
  value,
  format,
  title,
}: {
  label: string;
  value: Quantity;
  format: (n: number) => string;
  title?: string;
}) {
  return (
    <div className="min-w-0">
      <p className="font-mono text-xs uppercase tracking-wide text-faint">{label}</p>
      <QuantityValue
        value={value}
        format={format}
        title={title}
        className="mt-1 block text-2xl"
      />
    </div>
  );
}

// WindowBand renders the window-level rollup. `observations` is passed as a
// Quantity because the tenant-scoped path does not compute it (see the header
// note) — an unknown call count must not arrive here as a zero.
function WindowBand({
  total,
  observations,
}: {
  total: CostSummary;
  observations: Quantity;
}) {
  return (
    <section
      aria-label="Window summary"
      className="rounded-lg border border-border bg-card p-5"
      data-testid="cost-summary-card"
    >
      <div className="grid gap-6 sm:grid-cols-3">
        <Stat label="Total spend" value={total.totalCostUSD ?? UNKNOWN} format={formatMoney} />
        <Stat label="Total tokens" value={total.totalTokens ?? UNKNOWN} format={formatCompact} />
        <Stat
          label="Traced calls"
          value={observations}
          format={formatCompact}
          title="A tenant-scoped total is recomputed from the rows above and carries no call count — unknown, not zero."
        />
      </div>
    </section>
  );
}

// ForecastBand renders month-to-date spend against the tenant's budget (§5.24)
// plus the linear run-rate projection and the chargeback export.
//
// Both figures on the bar are monthly: month-to-date comes from the durable
// cost-rollup ledger, the cap from Tenant.spec.model.budgetUSD (the ceiling the
// gateway fails calls closed against). Nothing else on this page may share a
// bar with that cap — the table's figures are a trace window, not a month.
function ForecastBand({
  forecast,
  tenant,
  budgetUSD,
}: {
  forecast: CostForecastResponse;
  tenant: string;
  budgetUSD?: string;
}) {
  // Build the chargeback download URL for the current calendar month (YYYY-MM).
  const now = new Date();
  const period = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}`;
  const csvUrl = api.costChargebackCSVUrl(tenant, period);

  // A cap the CRD stores as a decimal string. Unparseable ⇒ no cap, which the
  // Meter renders as "no bar", never as a bar against a guessed bound.
  const cap = budgetUSD ? Number.parseFloat(budgetUSD) : Number.NaN;
  const hasCap = Number.isFinite(cap) && cap > 0;

  // A projection of 0 means the ledger could not project (an empty month, or
  // the first day of it) — not a forecast of nothing. It reads as unknown.
  const projected: Quantity =
    forecast.projectedMonthEndUSD > 0 ? forecast.projectedMonthEndUSD : UNKNOWN;

  return (
    <section
      aria-label="Month forecast"
      className="rounded-lg border border-border bg-card p-5"
      data-testid="cost-forecast-card"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
        <h2 className="font-serif text-lg font-medium">Month forecast</h2>
        <a
          href={csvUrl}
          download={`chargeback-${period}.csv`}
          data-testid="cost-chargeback-download"
          className="inline-flex items-center gap-1.5 text-sm text-primary border-b border-accent hover:border-primary"
        >
          <Download className="h-3.5 w-3.5" />
          Download chargeback CSV
        </a>
      </div>

      <div className="mt-4 grid gap-6 lg:grid-cols-[minmax(0,1fr)_16rem]">
        <Meter
          label={`${tenant} · month to date`}
          used={forecast.monthToDateUSD ?? UNKNOWN}
          cap={hasCap ? cap : UNKNOWN}
          threshold={hasCap ? cap * NEAR_CAP_RATIO : undefined}
          format={formatMoney}
          thing="tenant"
          foot={
            hasCap
              ? "The tick is 80% of the cap — where this console calls a tenant near cap. Over the cap, the next model call is refused."
              : null
          }
        />
        <div className="min-w-0">
          <p className="font-mono text-xs uppercase tracking-wide text-faint">
            Projected month end
          </p>
          <QuantityValue
            value={projected}
            format={formatMoney}
            title="Too little of the month has been recorded to project — unknown, not zero."
            className="mt-1 block text-2xl"
          />
          <p className="mt-1 text-sm text-faint">Linear run-rate on the days so far.</p>
        </div>
      </div>
    </section>
  );
}

// ── Load state ──────────────────────────────────────────────────────────────

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; agents: AgentCostItem[]; total: CostSummary; nextCursor: string }
  // ADR 0077 — the breakdown must be scoped by a tenant and we have no list to
  // scope by (403 / failed list). NOT "no spend".
  | { kind: "no-tenant" }
  | { kind: "unavailable" } // 501 — no trace backend on this install
  | { kind: "degraded"; message: string } // 200 + notice — trace store transiently down
  | { kind: "error"; message: string; forbidden: boolean };

// TenantsState is the PICKER's own load state, kept distinct from the
// breakdown's. `forbidden` is a first-class outcome: a viewer who cannot list
// tenants must not see a broken-looking picker or a blank table.
type TenantsState =
  | { kind: "loading" }
  | { kind: "ready"; tenants: TenantSummary[] }
  | { kind: "forbidden" }
  | { kind: "error"; message: string };

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

  const [tenantsState, setTenantsState] = useState<TenantsState>({ kind: "loading" });
  const [view, setView] = useState<ViewId>("all");
  const [query, setQuery] = useState("");
  // Bumped by the tenant-list retry so the fetch below can be re-run without a
  // page reload (a reload would be a bigger hammer than the failure deserves).
  const [tenantsAttempt, setTenantsAttempt] = useState(0);

  // Forecast: null = 501 (no ledger) or no tenant; undefined = not yet loaded.
  const [forecast, setForecast] = useState<CostForecastResponse | null | undefined>(
    undefined,
  );

  // setTenant drives the view by writing the selection into ?tenant= (replace,
  // not push — flipping the tenant is not a distinct history step).
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

  // Load the tenant list once for the picker. Best-effort: a 403 (a viewer who
  // cannot list tenants) is an honest "forbidden", distinct from an empty list.
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
  }, [tenantsAttempt]);

  // Default-to-first: landing with no ?tenant= while tenants exist picks the
  // first, so the page is immediately useful instead of dead-ending.
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

  // Switching tenant resets pagination — a cursor from one tenant is
  // meaningless for another.
  useEffect(() => {
    setPageStack([""]);
  }, [tenant]);

  // zeroTenants is the ONLY thing the breakdown fetch needs from the tenant
  // list: the M99 B1 "genuinely no tenants → fetch the cluster-wide breakdown"
  // fallback. Depending on the whole tenantsState instead re-created `load`
  // when the list merely finished loading, which fired a SECOND identical
  // breakdown fetch and flashed already-rendered rows away (m52.G8). As a
  // boolean it only changes when the answer actually changes.
  const zeroTenants =
    tenantsState.kind === "ready" && tenantsState.tenants.length === 0;

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    // ADR 0077: the breakdown REQUIRES ?tenant= when tenants exist. M99 B1: on
    // a cluster with genuinely NO tenants there is no boundary to leak across,
    // so the BFF serves the cluster-wide breakdown for an empty tenant and we
    // fetch it rather than dead-ending. While the list is still loading,
    // forbidden, or non-empty (default-to-first will select one), hold the calm
    // no-tenant state.
    if (!tenant) {
      if (!zeroTenants) {
        setLoadState({ kind: "no-tenant" });
        return;
      }
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
        // null = 501 (no trace backend) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        // A `notice` means the trace store is transiently down (200 + empty,
        // m23.6): show a retryable outage, NOT the "no cost data yet" empty
        // (which would claim zero activity when the source merely timed out).
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
        // 502 and other non-501 errors surface as a real, retryable error.
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [cursor, tenant, zeroTenants]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Forecast: fetched when ?tenant= is present. 501 → null (calm). Errors are
  // absorbed — the breakdown is the primary surface and the band is secondary.
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
        if (!ctrl.signal.aborted) setForecast(null);
      });
    return () => ctrl.abort();
  }, [tenant]);

  const agents = useMemo(
    () => (loadState.kind === "ready" ? loadState.agents : []),
    [loadState],
  );
  const total = loadState.kind === "ready" ? loadState.total : emptyCostSummary;
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR (BFF), NEVER off agents.length.
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;
  // The loaded window IS the whole list only when no cursor precedes or follows
  // it — the one condition under which a figure counted from the rows in hand
  // is a FACT rather than a windowed guess.
  const listComplete = loadState.kind === "ready" && !hasNext && !hasPrev;

  // Triage once, sort once. Attention-first (§6.1): nextStepRank is the primary
  // key so "Nothing needed" always sinks; spend descending breaks the tie, which
  // is the order the BFF already returns rows in.
  const sorted = useMemo(() => {
    const rows = agents.map(triage);
    rows.sort(
      (a, b) =>
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        b.item.totalCostUSD - a.item.totalCostUSD ||
        a.item.agentName.localeCompare(b.item.agentName),
    );
    return rows;
  }, [agents]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[0];
  const q = query.trim().toLowerCase();
  const visible = useMemo(() => {
    const byView = sorted.filter(activeView.match);
    return q
      ? byView.filter(
          (p) =>
            p.item.agentName.toLowerCase().includes(q) ||
            p.item.agentNs.toLowerCase().includes(q),
        )
      : byView;
  }, [sorted, activeView, q]);

  // Chip counts are facts or they are absent (kit FilterChipRow contract): the
  // agent list is cursor-paged, so a count of the rows in hand would describe
  // one page while looking like a total.
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: listComplete ? sorted.filter(v.match).length : undefined,
  }));

  // The Share denominator: the spend of the rows in hand. NEVER `total`, which
  // on the cluster-wide path is a different window entirely (see the header).
  const rowSpend = useMemo(
    () => sorted.reduce((sum, p) => sum + p.item.totalCostUSD, 0),
    [sorted],
  );
  const shareKnown = listComplete && rowSpend > 0;

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
          resource: "cost data",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // Columns are the §4.4 "Cost" budget in visual order — Agent(1) · Traces(3) ·
  // Tokens(3) · Cost(1) · Share(4) — plus the Next step column, which §4.4 says
  // is never dropped on any archetype. Cost survives every width: it is the
  // reason the page exists, and money is never truncated (§4.5).
  const columns: Column<Priced>[] = [
    {
      id: "agent",
      header: "Agent",
      priority: 1,
      className: "max-w-[22rem]",
      cell: ({ item, untagged }) => {
        // The testid uses the raw ns/name for lookup in tests; sanitize "/" in
        // the untagged sentinel so the attribute stays well-formed.
        const testNs = item.agentNs.replace(/[^a-z0-9-]/gi, "_") || "_";
        const testName = item.agentName.replace(/[^a-z0-9-]/gi, "_") || "_";
        return (
          <div data-testid={`cost-row-${testNs}-${testName}`}>
            <CellEntity
              name={
                untagged ? (
                  <span title="Traces with no agent tag — this spend cannot be attributed to a named agent.">
                    {UNTAGGED}
                  </span>
                ) : (
                  item.agentName
                )
              }
              title={item.agentName}
              namespace={untagged ? undefined : item.agentNs}
            />
          </div>
        );
      },
    },
    {
      id: "traces",
      header: "Traces",
      priority: 3,
      numeric: true,
      cell: ({ item }) => <QuantityValue value={item.runCount ?? UNKNOWN} />,
    },
    {
      id: "tokens",
      header: "Tokens",
      priority: 3,
      numeric: true,
      cell: ({ item }) => <QuantityValue value={item.totalTokens ?? UNKNOWN} />,
    },
    {
      id: "cost",
      header: "Cost",
      // Never dropped: this column is the page.
      priority: 1,
      numeric: true,
      cell: ({ item }) => (
        <QuantityValue
          value={item.totalCostUSD ?? UNKNOWN}
          format={formatMoney}
          title="No cost was recorded for these traces — unknown, not zero."
          className="text-foreground"
        />
      ),
    },
    {
      id: "share",
      header: "Share",
      priority: 4,
      numeric: true,
      cell: ({ item }) => (
        <QuantityValue
          // A share of a partial page is not a share of anything a reader
          // means, so it is absent rather than wrong.
          value={shareKnown ? (item.totalCostUSD / rowSpend) * 100 : UNKNOWN}
          format={formatShare}
          title="A share can only be computed when the whole breakdown is loaded — unknown, not zero."
        />
      ),
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      className: "w-[10rem]",
      cell: ({ item, next }) => (
        <NextStepLink
          label={next.label}
          to={next.to}
          tone={next.tone}
          ariaLabel={next.label ? `${next.label} behind ${item.agentName}` : undefined}
          testId={`cost-next-${item.agentName.replace(/[^a-z0-9-]/gi, "_")}`}
        />
      ),
    },
  ];

  const tenants = tenantsState.kind === "ready" ? tenantsState.tenants : [];
  const budgetUSD = tenants.find((t) => t.name === tenant)?.model?.budgetUSD;

  // The header is shared across every state so the picker is always reachable,
  // not just on the happy path.
  const header = (
    <PageHeader
      title="Cost"
      meta={tenant || (zeroTenants ? "all agents" : undefined)}
      lede="What the models actually cost, per agent, over a recent window of traced calls. Every figure here is measured — where one is not, it says so."
      actionsSlot={
        tenants.length > 0 ? (
          <TenantPicker tenant={tenant} tenants={tenants} onChange={setTenant} />
        ) : undefined
      }
    />
  );

  // Nothing to scope by (ADR 0077). Reached when the tenant list cannot be read
  // — a 403, or a list that failed — because the breakdown is 400 without a
  // tenant. It is NOT "no spend", and the copy must never let it read that way.
  if (loadState.kind === "no-tenant") {
    // The list is still in flight: this is LOADING, not an answer. Claiming
    // "no tenants" here would be a claim made a beat before the data arrives.
    if (tenantsState.kind === "loading") {
      return (
        <div className="min-w-0 space-y-6" data-testid="cost-page">
          {header}
          <div data-testid="cost-breakdown-table">
            <DataTable<Priced>
              columns={columns}
              rows={[]}
              rowKey={({ item }) => `${item.agentNs}-${item.agentName}`}
              loading
              ariaLabel="Cost breakdown"
            />
          </div>
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="cost-page">
        {header}
        <div data-testid="cost-no-tenant">
          {tenantsState.kind === "forbidden" ? (
            <QuietNote title="Cost is grouped by tenant, and you can’t read the tenant list.">
              Spend is scoped to one tenant at a time, so without that list there
              is nothing to scope by — this is a permission boundary, not an
              empty ledger. Ask an operator for a role that can read tenants.
              Nothing here is estimated and no spend is being claimed either way.
            </QuietNote>
          ) : tenantsState.kind === "error" ? (
            <QuietNote title="Cost is grouped by tenant, and the tenant list didn’t load.">
              Spend is scoped to one tenant at a time, so the breakdown cannot be
              fetched until that list answers. This is not a report of zero
              spend — it is a report of nothing asked.
              <Button
                variant="outline"
                size="sm"
                className="mt-3"
                onClick={() => setTenantsAttempt((n) => n + 1)}
                data-testid="cost-retry-tenants"
              >
                Try again
              </Button>
            </QuietNote>
          ) : (
            <QuietNote title="Cost is grouped by tenant, and this cluster has none yet.">
              A tenant groups namespaces and caps their model spend. Create one
              to track spend and budgets per team.
              <Button
                variant="outline"
                size="sm"
                className="mt-3"
                onClick={() => navigate("/tenants")}
                data-testid="cost-create-tenant"
              >
                Create a tenant
              </Button>
            </QuietNote>
          )}
        </div>
      </div>
    );
  }

  // 501 — this install has no trace backend. Calm (§7.1), never an error.
  if (loadState.kind === "unavailable") {
    return (
      <div className="min-w-0 space-y-6" data-testid="cost-page">
        {header}
        <div data-testid="cost-unavailable">
          <QuietNote title="Cost tracing isn’t configured on this install.">
            Per-agent spend is read from the trace backend, and this platform has
            none wired up — connecting one (Langfuse) enables it. Nothing here is
            estimated and nothing is lost: the figures are simply absent, not
            zero.
          </QuietNote>
        </div>
      </div>
    );
  }

  // 200 + notice — the trace store is transiently unavailable (slow /
  // circuit-broken). Distinct from "no data" so a reader knows to retry rather
  // than concluding there was no activity (m24 — the notice was dropped once).
  if (loadState.kind === "degraded") {
    return (
      <div className="min-w-0 space-y-6" data-testid="cost-page">
        {header}
        <div data-testid="cost-degraded">
          <ErrorState
            title="The trace store didn’t answer"
            description={loadState.message}
            onRetry={load}
          />
        </div>
      </div>
    );
  }

  // The chip views + filter box narrow the LOADED window, so an emptied view is
  // the "empty-filtered" truth (§7), not the first-run one: it offers a way
  // back out instead of teaching a reader with 12 rows how to get their first.
  const viewEmptied =
    agents.length > 0 && visible.length === 0 && view !== "all" && q === "";
  const empty: EmptyStateProps = viewEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: listComplete ? agents.length : undefined,
        countNoun: "rows",
      }
    : {
        icon: Coins,
        title: "No cost data yet",
        description:
          "Nothing was traced in this window, so there is no spend to break down. Run an agent from the Playground and its cost appears here.",
      };

  const ready = loadState.kind === "ready";
  // The tenant-scoped total is recomputed from the rows and never carries a call
  // count (handlers.go) — so on that path the figure is unknown, not zero.
  const observations: Quantity = tenant
    ? UNKNOWN
    : (total.observations ?? UNKNOWN);
  const closing = ready ? closingLine(sorted, listComplete) : null;

  return (
    <div className="min-w-0 space-y-6" data-testid="cost-page">
      {header}

      {/* M99 B1: with no tenants the cluster-wide per-agent breakdown is what
          is on screen. Say so, and offer the tenant that would scope it. */}
      {ready && !tenant && (
        <div data-testid="cost-all-agents-note">
          <QuietNote title="No tenants yet — this is every agent in the cluster.">
            Spend is normally scoped to one tenant, which is also where a budget
            lives. Create a tenant to get per-team spend and a cap to measure it
            against.
            <Button
              variant="outline"
              size="sm"
              className="mt-3"
              onClick={() => navigate("/tenants")}
              data-testid="cost-create-tenant"
            >
              Create a tenant
            </Button>
          </QuietNote>
        </div>
      )}

      {ready && <WindowBand total={total} observations={observations} />}

      {/* The forecast band needs a tenant (the ledger is tenant-scoped) and a
          ledger. null = 501 or no tenant → absent, calmly. undefined = still
          loading → absent, rather than a bar that guesses. */}
      {ready && tenant && forecast != null && (
        <ForecastBand forecast={forecast} tenant={tenant} budgetUSD={budgetUSD} />
      )}

      {ready && agents.length > 0 && (
        <div data-testid="cost-window-note">
          <QuietNote title="These figures cover a recent window — not all time.">
            {tenant ? (
              <>
                The rows are the last {TRACE_WINDOW} traces the trace backend
                recorded for <span className="font-mono">{tenant}</span>’s
                namespaces, and the totals above are those same rows added up, so
                they agree by construction. Spend from before that window is not
                counted here — and is not estimated either.
              </>
            ) : (
              <>
                The totals above are the trace backend’s{" "}
                {METRICS_WINDOW_DAYS}-day rollup, while the rows below read the
                last {TRACE_WINDOW} traces. Two different windows, so the rows do
                not add up to the total — neither figure is estimated, they
                simply measure different things.
              </>
            )}
          </QuietNote>
        </div>
      )}

      {/* §7 A1: the chips render while the table is still a skeleton — with no
          counts, because there is nothing yet to count. */}
      {(loadState.kind === "loading" || agents.length > 0) && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter the breakdown"
          className="min-w-0"
        />
      )}

      <div data-testid="cost-breakdown-table">
        <DataTable<Priced>
          columns={columns}
          rows={visible}
          rowKey={({ item }) => `${item.agentNs}-${item.agentName}`}
          loading={loadState.kind === "loading"}
          error={error}
          query={query}
          onQueryChange={setQuery}
          queryPlaceholder="Filter agents on this page…"
          hasPrev={hasPrev}
          hasNext={hasNext}
          onPrev={onPrev}
          onNext={onNext}
          rangeLabel={`Page ${pageNumber}`}
          ariaLabel="Cost breakdown"
          // Row click → the agent detail — but NOT for the (untagged) bucket,
          // which is not an agent and has nowhere to go.
          onRowClick={({ item }) => {
            if (item.agentName === UNTAGGED) return;
            navigate(
              `/agents/${encodeURIComponent(item.agentNs)}/${encodeURIComponent(item.agentName)}`,
            );
          }}
          empty={empty}
        />
      </div>

      {closing && <ClosingNote>{closing}</ClosingNote>}
    </div>
  );
}
