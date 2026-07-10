import * as React from "react";
import {
  ChevronDown,
  ChevronRight,
  Coins,
  EyeOff,
  MessageSquare,
  Star,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable, type Column } from "@/components/kit";
import { ConsoleChrome, Breadcrumb } from "@/design/console-chrome";
import { KeyValue, Note, ScreenFrame } from "@/design/scaffold";

// ── Native trace explorer (span tree + waterfall + span detail) ────────────
interface Span {
  id: string;
  label: string;
  kind: "run" | "llm" | "tool" | "internal";
  depth: number;
  startPct: number;
  widthPct: number;
  ms: number;
  redacted?: boolean;
}

const SPANS: Span[] = [
  { id: "s0", label: "run: billing-support-agent", kind: "run", depth: 0, startPct: 0, widthPct: 100, ms: 1840 },
  { id: "s1", label: "llm: claude-sonnet-4 (plan)", kind: "llm", depth: 1, startPct: 2, widthPct: 28, ms: 520 },
  { id: "s2", label: "tool: get_invoice", kind: "tool", depth: 1, startPct: 31, widthPct: 12, ms: 180 },
  { id: "s3", label: "tool: search_docs (redacted)", kind: "tool", depth: 1, startPct: 44, widthPct: 18, ms: 320, redacted: true },
  { id: "s4", label: "llm: claude-sonnet-4 (answer)", kind: "llm", depth: 1, startPct: 63, widthPct: 35, ms: 640 },
];

const KIND_COLOR: Record<Span["kind"], string> = {
  run: "bg-primary",
  llm: "bg-info",
  tool: "bg-success",
  internal: "bg-muted-foreground",
};

export function TraceExplorerWireframe() {
  const [selected, setSelected] = React.useState<string>("s2");
  const active = SPANS.find((s) => s.id === selected)!;

  return (
    <div className="space-y-4">
      <Note>
        Native, on-theme trace explorer (§31) — Langfuse demoted to a link-out.
        Left: span tree + waterfall. Right: span detail with input/output,
        tokens, cost. Click "search_docs" to see the redacted-span state.
      </Note>
      <ScreenFrame caption="Span tree + waterfall + span detail">
        <ConsoleChrome
          active="traces"
          title="Trace"
          headerActions={<Button variant="outline" size="sm">Open in Langfuse ↗</Button>}
        >
          <Breadcrumb parts={["Traces", "trace-9f2a1c"]} />
          <div className="grid gap-4 lg:grid-cols-[1fr_22rem]">
            {/* Tree + waterfall */}
            <div className="overflow-hidden rounded-lg border bg-card shadow-card">
              <div className="flex items-center justify-between border-b bg-surface-2/60 px-4 py-2 text-xs text-muted-foreground">
                <span>Spans</span>
                <span>1.84s total · 1,240 tok · $0.006</span>
              </div>
              <div>
                {SPANS.map((s) => {
                  const isSel = s.id === selected;
                  return (
                    <button
                      key={s.id}
                      onClick={() => setSelected(s.id)}
                      className={`grid w-full grid-cols-[16rem_1fr] items-center gap-2 border-b border-border/50 px-3 py-2 text-left transition-colors last:border-0 ${isSel ? "bg-accent" : "hover:bg-surface-2/60"}`}
                    >
                      <span className="flex items-center gap-1.5 truncate text-sm" style={{ paddingLeft: `${s.depth * 14}px` }}>
                        {s.kind === "run" ? <ChevronDown className="h-3.5 w-3.5 text-muted-foreground" /> : <span className="w-3.5" />}
                        {s.redacted && <EyeOff className="h-3.5 w-3.5 text-warning" />}
                        <span className={`h-2 w-2 rounded-full ${KIND_COLOR[s.kind]}`} />
                        <span className="truncate">{s.label}</span>
                      </span>
                      <span className="relative h-4">
                        <span
                          className={`absolute top-1/2 h-2.5 -translate-y-1/2 rounded-sm ${KIND_COLOR[s.kind]} ${s.redacted ? "opacity-40" : ""}`}
                          style={{ left: `${s.startPct}%`, width: `${s.widthPct}%` }}
                        />
                        <span className="absolute right-1 top-1/2 -translate-y-1/2 text-[10px] text-muted-foreground">{s.ms}ms</span>
                      </span>
                    </button>
                  );
                })}
              </div>
            </div>

            {/* Span detail */}
            <div className="rounded-lg border bg-card p-4 shadow-card">
              <div className="mb-3 flex items-center gap-2">
                <span className={`h-2.5 w-2.5 rounded-full ${KIND_COLOR[active.kind]}`} />
                <p className="truncate text-sm font-medium">{active.label}</p>
              </div>
              {active.redacted ? (
                <div className="space-y-3">
                  <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground">
                    <EyeOff className="h-4 w-4 text-warning" />
                    <span className="text-muted-foreground">Input/output redacted by trace-governance policy (§13.3). Timing and cost are still shown.</span>
                  </div>
                  <KeyValue rows={[{ k: "Duration", v: `${active.ms}ms` }, { k: "Tokens", v: "—" }, { k: "Cost", v: "—" }]} />
                </div>
              ) : (
                <div className="space-y-4">
                  <KeyValue
                    rows={[
                      { k: "Kind", v: active.kind },
                      { k: "Duration", v: `${active.ms}ms` },
                      { k: "Tokens", v: active.kind === "llm" ? "620 in / 240 out" : "—" },
                      { k: "Cost", v: active.kind === "llm" ? "$0.003" : "$0.000" },
                    ]}
                  />
                  <div>
                    <p className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">Input</p>
                    <pre className="overflow-x-auto rounded-md bg-surface-3 p-3 text-xs">{`{ "id": 4021 }`}</pre>
                  </div>
                  <div>
                    <p className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">Output</p>
                    <pre className="overflow-x-auto rounded-md bg-surface-3 p-3 text-xs">{`{ "status": "shipped", "eta": "2026-07-05" }`}</pre>
                  </div>
                </div>
              )}
            </div>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Runs browser ───────────────────────────────────────────────────────────
interface RunRow { id: string; agent: string; when: string; status: string; tokens: number; cost: string; latency: string }
const RUNS: RunRow[] = [
  { id: "r-9f2a", agent: "billing-support-agent", when: "2m ago", status: "ok", tokens: 1240, cost: "$0.006", latency: "1.8s" },
  { id: "r-8c11", agent: "docs-rag-agent", when: "5m ago", status: "ok", tokens: 3120, cost: "$0.021", latency: "3.2s" },
  { id: "r-7b09", agent: "triage-bot", when: "8m ago", status: "error", tokens: 210, cost: "$0.001", latency: "0.6s" },
  { id: "r-6a02", agent: "billing-support-agent", when: "12m ago", status: "ok", tokens: 940, cost: "$0.004", latency: "1.4s" },
];

export function RunsBrowserWireframe() {
  const [q, setQ] = React.useState("");
  const cols: Column<RunRow>[] = [
    { id: "id", header: "Run", cell: (r) => <span className="font-mono text-xs">{r.id}</span> },
    { id: "agent", header: "Agent", sortable: true, cell: (r) => <span className="font-medium">{r.agent}</span> },
    { id: "when", header: "When", hideOnMobile: true, cell: (r) => r.when },
    { id: "status", header: "Status", sortable: true, cell: (r) => <Badge variant={r.status === "ok" ? "success" : "destructive"} className="text-[10px]">{r.status}</Badge> },
    { id: "tokens", header: "Tokens", sortable: true, className: "text-right", cell: (r) => <span className="tabular-nums">{r.tokens}</span> },
    { id: "cost", header: "Cost", className: "text-right", cell: (r) => r.cost },
    { id: "latency", header: "Latency", className: "text-right", hideOnMobile: true, cell: (r) => r.latency },
  ];
  return (
    <div className="space-y-4">
      <Note>Runs browser — every run on the paginated list contract; a row opens its native trace. Filters by agent/status.</Note>
      <ScreenFrame>
        <ConsoleChrome active="runs" title="Runs">
          <DataTable
            columns={cols}
            rows={RUNS}
            rowKey={(r) => r.id}
            query={q}
            onQueryChange={setQ}
            queryPlaceholder="Filter runs (agent, status)…"
            sort={{ columnId: "when", dir: "desc" }}
            onSortChange={() => {}}
            hasNext
            rangeLabel="1–4 of 1,204"
            onRowClick={() => {}}
            toolbar={<div className="flex h-9 items-center gap-2 rounded-md border bg-background px-3 text-sm text-muted-foreground">status: <span className="font-medium text-foreground">all</span></div>}
          />
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Feedback browser ───────────────────────────────────────────────────────
export function FeedbackBrowserWireframe() {
  const items = [
    { score: 5, agent: "billing-support-agent", comment: "Resolved my refund instantly.", trace: "r-9f2a", when: "3m ago" },
    { score: 2, agent: "triage-bot", comment: "Routed to the wrong queue.", trace: "r-7b09", when: "9m ago" },
    { score: 4, agent: "docs-rag-agent", comment: "Good answer, cited the right page.", trace: "r-8c11", when: "15m ago" },
  ];
  return (
    <div className="space-y-4">
      <Note>Feedback browser (§17.3) — scores/comments correlated to their trace. Click a row → the native trace explorer.</Note>
      <ScreenFrame>
        <ConsoleChrome active="feedback" title="Feedback">
          <div className="mb-4 grid grid-cols-3 gap-4">
            {[["Avg score", "4.1"], ["Responses (24h)", "312"], ["Thumbs-down", "18"]].map(([l, v]) => (
              <div key={l} className="rounded-lg border bg-card p-4 shadow-card">
                <p className="text-xs uppercase tracking-wide text-muted-foreground">{l}</p>
                <p className="mt-1 text-2xl font-semibold tracking-tight">{v}</p>
              </div>
            ))}
          </div>
          <div className="space-y-2">
            {items.map((it) => (
              <button key={it.trace} className="flex w-full items-start gap-3 rounded-lg border bg-card px-4 py-3 text-left shadow-card hover:bg-surface-2">
                <div className="flex items-center gap-0.5">
                  {Array.from({ length: 5 }).map((_, i) => (
                    <Star key={i} className={`h-3.5 w-3.5 ${i < it.score ? "fill-warning text-warning" : "text-border-strong"}`} />
                  ))}
                </div>
                <div className="min-w-0 flex-1">
                  <p className="flex items-center gap-2 text-sm">
                    <MessageSquare className="h-3.5 w-3.5 text-muted-foreground" />
                    {it.comment}
                  </p>
                  <p className="text-xs text-muted-foreground">{it.agent} · {it.when} · trace {it.trace}</p>
                </div>
                <ChevronRight className="mt-1 h-4 w-4 text-muted-foreground" />
              </button>
            ))}
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Cost v2 (per-agent drill-down) ─────────────────────────────────────────
export function CostWireframe() {
  const agents = [
    { name: "docs-rag-agent", cost: 18.4, pct: 100, tokens: "2.1M" },
    { name: "billing-support-agent", cost: 12.9, pct: 70, tokens: "1.6M" },
    { name: "onboarding-agent", cost: 6.2, pct: 34, tokens: "0.7M" },
    { name: "triage-bot", cost: 4.7, pct: 26, tokens: "0.5M" },
  ];
  return (
    <div className="space-y-4">
      <Note>Cost v2 (§31) — spend rollups with a per-agent drill-down. Native charts on-theme; no Langfuse embed.</Note>
      <ScreenFrame>
        <ConsoleChrome active="cost" title="Cost" headerActions={<Badge variant="secondary" className="text-[10px]">last 7 days</Badge>}>
          <div className="mb-6 grid grid-cols-3 gap-4">
            {[["Total spend", "$42.18"], ["Tokens", "4.9M"], ["Avg $/run", "$0.035"]].map(([l, v]) => (
              <div key={l} className="rounded-lg border bg-card p-4 shadow-card">
                <p className="flex items-center gap-1.5 text-xs uppercase tracking-wide text-muted-foreground"><Coins className="h-3.5 w-3.5" />{l}</p>
                <p className="mt-1 text-2xl font-semibold tracking-tight">{v}</p>
              </div>
            ))}
          </div>
          <div className="rounded-lg border bg-card p-5 shadow-card">
            <p className="mb-4 text-sm font-medium">Cost by agent</p>
            <div className="space-y-3">
              {agents.map((a) => (
                <button key={a.name} className="flex w-full items-center gap-3 text-left">
                  <span className="w-44 truncate text-sm">{a.name}</span>
                  <span className="h-6 flex-1 overflow-hidden rounded bg-surface-2">
                    <span className="flex h-full items-center justify-end rounded bg-primary/70 px-2 text-[10px] font-medium text-primary-foreground" style={{ width: `${a.pct}%` }}>
                      ${a.cost.toFixed(2)}
                    </span>
                  </span>
                  <span className="w-16 text-right text-xs text-muted-foreground">{a.tokens}</span>
                </button>
              ))}
            </div>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}
