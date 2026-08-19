import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Boxes, Check, Copy, MessagesSquare } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, ApiError, type RunSummary, type RunsFilteredParams } from "@/lib/api";
import { formatTokens } from "@/lib/format";

// RunsPage — the global paginated + filterable runs browser (m16.8).
//
// Backend: GET /api/runs?agent=&from=&to=&q=&limit=&cursor=
//   • agent (ns/name), from (ISO8601), to (ISO8601): SERVER-SIDE filters
//   • q: page-windowed CLIENT-SIDE substring filter (the BFF filters the loaded
//     page window, NOT the whole cluster — K8s has no server-side substring
//     search). Because q is page-windowed, a page can return SHORT and nextCursor
//     is derived from the unfiltered count, so:
//       – Never infer "no results" from a short page when q is set + nextCursor present.
//       – The DataTable already handles this: it shows "No matches in this page —
//         more pages exist. Load next page or clear filter." and keeps Next live.
//   • NO status filter — status was rejected server-side in m16.3 (the Langfuse
//     trace list has no per-trace status). Do NOT add one.
//   • cursor: opaque pagination token from the prior page's nextCursor.
//
// 501-calm / 502-error discipline (mirrors agentRuns + feedback):
//   • 501 (Langfuse not configured): runsFiltered() returns null → calm
//     "unavailable" empty state, NEVER an error toast or error state.
//   • 502 (Langfuse configured, upstream fetch FAILED): runsFiltered() throws
//     ApiError → surfaced as a visible error state (retryable), never hidden.
//
// RBAC: the page is READ-only (viewer can read runs). There are no write
// affordances — no actions column, no edit/delete. The API is the real gate
// (ADR 0011); the page simply shows what the caller's token can reach.
//
// Row-click: navigates to /traces/:traceId — the full single-trace view
// (TracePage, m16.7).
//
// data-testid contract:
//   runs-page           — root container
//   runs-table          — the DataTable (via aria-label="Runs")
//   runs-filter-bar     — the filter inputs container
//   run-row-{traceId}   — each table row (via rowKey)

const PAGE_LIMIT = 50;

// RunsFilterBar holds the three server-side filter inputs: agent (ns/name),
// date-from, date-to. The q (free-text) filter is owned by the DataTable
// itself (via onQueryChange). NO status filter — m16.3 explicitly rejected it.
function RunsFilterBar({
  agent,
  from,
  to,
  onAgent,
  onFrom,
  onTo,
}: {
  agent: string;
  from: string;
  to: string;
  onAgent: (v: string) => void;
  onFrom: (v: string) => void;
  onTo: (v: string) => void;
}) {
  return (
    <div
      className="flex flex-wrap items-end gap-3"
      data-testid="runs-filter-bar"
    >
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-agent"
          className="text-xs font-medium text-muted-foreground"
        >
          Agent (ns/name)
        </label>
        <Input
          id="runs-filter-agent"
          value={agent}
          onChange={(e) => onAgent(e.target.value)}
          placeholder="namespace/agent-name"
          className="h-8 w-52 text-sm"
          aria-label="Filter by agent"
        />
      </div>
      <div className="flex flex-col gap-1">
        <span className="text-xs font-medium text-muted-foreground">Range</span>
        <div className="flex gap-1" data-testid="runs-range-presets">
          {RANGE_PRESETS.map((p) => (
            <Button
              key={p.label}
              type="button"
              variant="outline"
              size="sm"
              className="h-8 px-2 text-xs"
              onClick={() => {
                onFrom(datetimeLocalAgo(p.ms));
                onTo("");
              }}
              data-testid={`runs-range-${p.label}`}
            >
              {p.label}
            </Button>
          ))}
        </div>
      </div>
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-from"
          className="text-xs font-medium text-muted-foreground"
        >
          From
        </label>
        <Input
          id="runs-filter-from"
          type="datetime-local"
          value={from}
          onChange={(e) => onFrom(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter from date"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-to"
          className="text-xs font-medium text-muted-foreground"
        >
          To
        </label>
        <Input
          id="runs-filter-to"
          type="datetime-local"
          value={to}
          onChange={(e) => onTo(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter to date"
        />
      </div>
    </div>
  );
}

// TraceIdCell shows a SHORT trace id (32-char hex is unreadable in a column) with a copy button for the
// full id (M99 B3). stopPropagation so copying doesn't also trigger the row's navigate-to-trace.
function TraceIdCell({ traceId }: { traceId: string }) {
  const [copied, setCopied] = useState(false);
  const short =
    traceId.length > 12 ? `${traceId.slice(0, 8)}…${traceId.slice(-4)}` : traceId;
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono text-xs text-muted-foreground" title={traceId}>
        {short}
      </span>
      <button
        type="button"
        aria-label="Copy trace ID"
        data-testid={`copy-trace-${traceId}`}
        onClick={(e) => {
          e.stopPropagation();
          navigator.clipboard?.writeText(traceId).then(
            () => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            },
            () => {},
          );
        }}
        className="text-muted-foreground/70 transition-colors hover:text-foreground"
      >
        {copied ? (
          <Check className="h-3 w-3 text-success" />
        ) : (
          <Copy className="h-3 w-3" />
        )}
      </button>
    </span>
  );
}

// fmtCost keeps ONE consistent precision down the column (M99 B3): a true zero is "$0.00", a non-zero
// amount too small to show at 3 decimals collapses to "<$0.001" (rather than a jarring "$0.00037" next
// to "$0.008"), and everything else is 3 decimals. No mixed 3-vs-5-decimal rows.
function fmtCost(usd: number): string {
  if (usd === 0) return "$0.00";
  if (usd < 0.001) return "<$0.001";
  return `$${usd.toFixed(3)}`;
}

function fmtTimestamp(ts: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

// RunStatusCell renders the enriched run outcome (ADR 0081): "ok" → a calm success chip, "error" →
// a destructive chip, and an ABSENT status (an unenriched row, or a trace whose /detail we couldn't
// fetch) → a muted "—" — an honest "unknown", never a fabricated pass/fail.
function RunStatusCell({ status }: { status?: string }) {
  if (status === "ok")
    return (
      <Badge variant="success" className="capitalize">
        OK
      </Badge>
    );
  if (status === "error")
    return <Badge variant="destructive">Error</Badge>;
  return <span className="text-sm text-muted-foreground">—</span>;
}

// runNameDistinctFromAgent returns the run's own name ONLY when it adds information beyond the Agent
// column — i.e. it is NOT just the agent-identity display ("ns/name" or bare "name") the launcher
// stamps as the default trace name (M100 UI99-runstable, the NAME≈AGENT dedup). When the name is
// merely the agent identity (the common case), it returns "" so the Name column collapses to "—"
// and the eye goes to the single Agent column instead of reading the same text twice.
function runNameDistinctFromAgent(r: RunSummary): string {
  const name = (r.name ?? "").trim();
  if (!name) return "";
  if (!r.agentName) return name; // ambient/untagged run — the name is all we have
  const identity = r.agentNs ? `${r.agentNs}/${r.agentName}` : r.agentName;
  return name === identity ? "" : name;
}

// RANGE_PRESETS are the quick time-range shortcuts every observability console offers (M99 B3) — a
// one-click "last N" that beats hand-entering two datetimes. Each sets `from = now − ms` and clears `to`
// (open-ended = up to now).
const RANGE_PRESETS: { label: string; ms: number }[] = [
  { label: "15m", ms: 15 * 60_000 },
  { label: "1h", ms: 60 * 60_000 },
  { label: "24h", ms: 24 * 60 * 60_000 },
  { label: "7d", ms: 7 * 24 * 60 * 60_000 },
];

// datetimeLocalAgo formats (now − ms) as a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time), the shape the From/To inputs + toRFC3339 already expect.
function datetimeLocalAgo(ms: number): string {
  const d = new Date(Date.now() - ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// toRFC3339 converts a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time, no seconds/timezone) into a UTC RFC3339 string the BFF's runs
// filter accepts. Returns "" for empty/unparseable input (so the caller omits the
// param rather than sending a malformed one — the m24 fix for the Runs 400).
function toRFC3339(dtLocal: string): string {
  if (!dtLocal) return "";
  const d = new Date(dtLocal);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString();
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; runs: RunSummary[]; nextCursor: string }
  | { kind: "unavailable" } // 501 — Langfuse not configured
  | { kind: "degraded"; message: string } // 200 + notice — trace store transiently down
  | { kind: "error"; message: string; forbidden: boolean };

export function RunsPage() {
  const navigate = useNavigate();

  // Server-side filter state
  const [agentFilter, setAgentFilter] = useState("");
  const [fromFilter, setFromFilter] = useState("");
  const [toFilter, setToFilter] = useState("");

  // Client-side q filter (page-windowed substring)
  const [query, setQuery] = useState("");

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

    // The <input type="datetime-local"> value is "YYYY-MM-DDTHH:MM" in LOCAL time
    // (no seconds, no timezone) — the BFF's runs filter requires RFC3339, so
    // sending it raw 400s ("from must be RFC3339"). Convert local -> UTC RFC3339.
    const fromRfc = toRFC3339(fromFilter);
    const toRfc = toRFC3339(toFilter);

    const params: RunsFilteredParams = {
      limit: PAGE_LIMIT,
      // The Runs browser opts into per-trace enrichment (ADR 0081) so each row shows REAL
      // tokens + an ok/error status the list API can't carry. The dashboard's recent-runs
      // peek deliberately does NOT (it only needs cost/latency — the cheap path).
      enrich: true,
      ...(cursor ? { cursor } : {}),
      ...(agentFilter ? { agent: agentFilter } : {}),
      ...(fromRfc ? { from: fromRfc } : {}),
      ...(toRfc ? { to: toRfc } : {}),
      ...(query ? { q: query } : {}),
    };

    api
      .runsFiltered(params, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (Langfuse not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        // A `notice` means the trace store is transiently down (200 + empty, m23.6):
        // show "temporarily unavailable — retry", NOT the misleading "No runs yet".
        if (res.notice) {
          setLoadState({ kind: "degraded", message: res.notice });
          return;
        }
        setLoadState({
          kind: "ready",
          runs: res.runs,
          nextCursor: res.nextCursor ?? "",
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
  }, [cursor, agentFilter, fromFilter, toFilter, query]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Reset to page 0 whenever any server-side filter changes or q changes.
  const resetPaging = useCallback(() => setPageStack([""]), []);

  function onAgentChange(v: string) {
    setAgentFilter(v);
    resetPaging();
  }
  function onFromChange(v: string) {
    setFromFilter(v);
    resetPaging();
  }
  function onToChange(v: string) {
    setToFilter(v);
    resetPaging();
  }
  function onQueryChange(q: string) {
    setQuery(q);
    resetPaging();
  }

  const runs = loadState.kind === "ready" ? loadState.runs : [];
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR (BFF), NEVER off runs.length — a page-windowed
  // q filter can empty the current page while later pages still have matches.
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

  const columns: Column<RunSummary>[] = [
    {
      id: "name",
      header: "Name",
      // Deduped against the Agent column (M100): show the run's own name only when it differs from
      // the agent identity the Agent column already renders; otherwise "—" (no repeated text).
      cell: (r) => {
        const name = runNameDistinctFromAgent(r);
        return name ? (
          <span className="font-medium">{name}</span>
        ) : (
          <span className="text-sm text-muted-foreground">—</span>
        );
      },
    },
    {
      id: "agent",
      header: "Agent",
      hideOnMobile: true,
      // Per-row link straight to the originating agent (m54.2, M49 UX review A1) —
      // no trace→agent hop. stopPropagation so the link doesn't also trigger the
      // row-click's navigate-to-trace. "—" when the run carries no agent identity.
      cell: (r) =>
        r.agentNs && r.agentName ? (
          <Link
            to={`/agents/${encodeURIComponent(r.agentNs)}/${encodeURIComponent(r.agentName)}`}
            data-testid={`run-agent-link-${r.traceId}`}
            onClick={(e) => e.stopPropagation()}
            className="inline-flex items-center gap-1 text-sm font-medium text-primary hover:underline"
          >
            <Boxes className="h-3.5 w-3.5" />
            {r.agentNs}/{r.agentName}
          </Link>
        ) : (
          <span className="text-sm text-muted-foreground">—</span>
        ),
    },
    {
      id: "traceId",
      header: "Trace ID",
      hideOnMobile: true,
      cell: (r) => <TraceIdCell traceId={r.traceId} />,
    },
    {
      id: "timestamp",
      header: "When",
      hideOnMobile: true,
      cell: (r) => (
        <span className="text-sm text-muted-foreground">
          {fmtTimestamp(r.timestamp)}
        </span>
      ),
    },
    {
      id: "status",
      header: "Status",
      cell: (r) => <RunStatusCell status={r.status} />,
    },
    {
      id: "tokens",
      header: "Tokens",
      className: "text-right",
      cell: (r) => (
        <span className="tabular-nums">{formatTokens(r.tokens)}</span>
      ),
    },
    {
      id: "cost",
      header: "Cost",
      className: "text-right",
      cell: (r) => (
        <span className="tabular-nums">{fmtCost(r.costUSD)}</span>
      ),
    },
    {
      id: "latency",
      header: "Latency",
      className: "text-right",
      hideOnMobile: true,
      cell: (r) => (
        <span className="tabular-nums">{Math.round(r.latencyMs)}ms</span>
      ),
    },
    // NOTE: NO actions column — runs are READ-only. A viewer can read runs;
    // there are no write affordances here (RBAC enforced at the API, ADR 0011).
  ];

  // 501 calm state — Langfuse not configured.
  if (loadState.kind === "unavailable") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="runs-page">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Runs</h2>
          <p className="text-sm text-muted-foreground">
            Global run history — all traces across all agents.
          </p>
        </div>
        <div
          className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
          data-testid="runs-unavailable"
        >
          Runs unavailable — tracing not configured (Langfuse not wired).
        </div>
      </div>
    );
  }

  // 200 + notice — the trace store is transiently unavailable (slow/circuit-broken).
  // Honest degrade: distinct from "No runs yet" so the user knows to retry, not that
  // there is zero activity (m24 — the notice was previously dropped).
  if (loadState.kind === "degraded") {
    return (
      <div className="mx-auto max-w-5xl space-y-6" data-testid="runs-page">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Runs</h2>
          <p className="text-sm text-muted-foreground">
            Global run history — all traces across all agents.
          </p>
        </div>
        <div
          className="flex h-40 flex-col items-center justify-center gap-3 rounded-lg border bg-card px-6 text-center text-sm text-muted-foreground"
          data-testid="runs-degraded"
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
    <div className="mx-auto max-w-5xl space-y-6" data-testid="runs-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Runs</h2>
        <p className="text-sm text-muted-foreground">
          Global run history — all traces across all agents. The text filter is
          windowed to the loaded page; use the agent + date filters to narrow
          server-side.
        </p>
      </div>

      <DataTable<RunSummary>
        columns={columns}
        rows={runs}
        rowKey={(r) => r.traceId}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter runs on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Runs"
        // Row-click → the native trace page (m16.7).
        onRowClick={(r) => navigate(`/traces/${encodeURIComponent(r.traceId)}`)}
        // The filter bar (agent + from + to) is the DataTable's toolbar slot so
        // it renders flush with the table (not above the q input).
        toolbar={
          <RunsFilterBar
            agent={agentFilter}
            from={fromFilter}
            to={toFilter}
            onAgent={onAgentChange}
            onFrom={onFromChange}
            onTo={onToChange}
          />
        }
        empty={{
          icon: MessagesSquare,
          title: "No runs yet",
          description: agentFilter
            ? `No runs found for agent "${agentFilter}". Try a different agent filter or date range.`
            : "No runs visible. Run an agent from the Playground to see its traced runs here.",
        }}
      />
    </div>
  );
}
