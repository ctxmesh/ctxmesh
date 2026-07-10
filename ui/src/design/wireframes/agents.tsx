import * as React from "react";
import {
  Boxes,
  MoreHorizontal,
  Play,
  Plus,
  Sparkles,
  Terminal,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { DataTable, type Column } from "@/components/kit";
import { ConsoleChrome, Breadcrumb } from "@/design/console-chrome";
import {
  KeyValue,
  Note,
  ScreenFrame,
  ViewerBanner,
  WireTabs,
} from "@/design/scaffold";

interface AgentRow {
  name: string;
  namespace: string;
  runtime: string;
  model: string;
  phase: "Ready" | "Pending" | "Failed";
  runs24h: number;
}

const AGENTS: AgentRow[] = [
  { name: "billing-support-agent", namespace: "prod", runtime: "managed", model: "claude-sonnet-4", phase: "Ready", runs24h: 412 },
  { name: "docs-rag-agent", namespace: "prod", runtime: "custom", model: "claude-opus-4", phase: "Ready", runs24h: 188 },
  { name: "triage-bot", namespace: "prod", runtime: "managed", model: "gpt-4o", phase: "Pending", runs24h: 0 },
  { name: "invoice-parser", namespace: "staging", runtime: "custom", model: "claude-haiku-4", phase: "Failed", runs24h: 3 },
  { name: "onboarding-agent", namespace: "staging", runtime: "managed", model: "claude-sonnet-4", phase: "Ready", runs24h: 57 },
];

function phaseBadge(p: AgentRow["phase"]) {
  const v = p === "Ready" ? "success" : p === "Pending" ? "warning" : "destructive";
  return <Badge variant={v} className="text-[10px]">{p}</Badge>;
}

function agentColumns(): Column<AgentRow>[] {
  return [
    {
      id: "name",
      header: "Agent",
      sortable: true,
      cell: (r) => (
        <div className="flex items-center gap-2.5">
          <div className="flex h-7 w-7 items-center justify-center rounded-md bg-surface-2">
            <Boxes className="h-4 w-4 text-primary" />
          </div>
          <div>
            <p className="font-medium">{r.name}</p>
            <p className="text-xs text-muted-foreground">{r.namespace}</p>
          </div>
        </div>
      ),
    },
    { id: "runtime", header: "Runtime", hideOnMobile: true, cell: (r) => <Badge variant="secondary" className="text-[10px]">{r.runtime}</Badge> },
    { id: "model", header: "Model", hideOnMobile: true, cell: (r) => <span className="font-mono text-xs">{r.model}</span> },
    { id: "phase", header: "Status", sortable: true, cell: (r) => phaseBadge(r.phase) },
    { id: "runs", header: "Runs 24h", sortable: true, className: "text-right", cell: (r) => <span className="tabular-nums">{r.runs24h}</span> },
  ];
}

export function AgentsListWireframe() {
  const [q, setQ] = React.useState("");
  return (
    <div className="space-y-4">
      <Note>
        The first DataTable consumer: filter bar (windowed `q`), sortable columns,
        cursor pagination (always shown — scale-first), row → detail. "New agent"
        offers both entrances.
      </Note>
      <ScreenFrame>
        <ConsoleChrome
          active="agents"
          title="Agents"
          headerActions={
            <div className="flex gap-2">
              <Button size="sm" variant="outline"><Plus className="h-4 w-4" />Configure</Button>
              <Button size="sm"><Sparkles className="h-4 w-4" />Describe an agent</Button>
            </div>
          }
        >
          <DataTable
            columns={agentColumns()}
            rows={AGENTS}
            rowKey={(r) => `${r.namespace}/${r.name}`}
            query={q}
            onQueryChange={setQ}
            queryPlaceholder="Filter agents (name, model)…"
            sort={{ columnId: "runs", dir: "desc" }}
            onSortChange={() => {}}
            hasPrev={false}
            hasNext
            onNext={() => {}}
            rangeLabel="1–5 of 18"
            onRowClick={() => {}}
            rowActions={() => (
              <Button variant="ghost" size="icon"><MoreHorizontal className="h-4 w-4" /></Button>
            )}
            toolbar={
              <div className="flex h-9 items-center rounded-md border bg-background px-3 text-sm text-muted-foreground">
                namespace: <span className="ml-1 font-medium text-foreground">all</span>
              </div>
            }
          />
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

export function AgentsListViewerWireframe() {
  return (
    <div className="space-y-4">
      <Note>
        RBAC-aware chrome (spec §3): the SAME list for a viewer — no "New agent"
        header buttons, no per-row action menu, a read-only banner. Enforcement
        still lives with the K8s API server; the UI only hides what you can't do.
      </Note>
      <ScreenFrame caption="Viewer variant">
        <ConsoleChrome active="agents" viewer title="Agents" banner={<ViewerBanner />}>
          <DataTable
            columns={agentColumns()}
            rows={AGENTS}
            rowKey={(r) => `${r.namespace}/${r.name}`}
            query=""
            onQueryChange={() => {}}
            queryPlaceholder="Filter agents…"
            hasPrev={false}
            hasNext
            rangeLabel="1–5 of 18"
            onRowClick={() => {}}
          />
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Agent detail (tabs + run panel) ────────────────────────────────────────
const DETAIL_TABS = ["Overview", "Logs", "Runs", "Bindings"] as const;

function OverviewTab() {
  return (
    <div className="grid gap-6 lg:grid-cols-[1fr_18rem]">
      <div className="space-y-6">
        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Configuration</p>
          <KeyValue
            rows={[
              { k: "Runtime", v: <Badge variant="secondary" className="text-[10px]">managed</Badge> },
              { k: "Model route", v: <span className="font-mono text-xs">claude-sonnet-4</span> },
              { k: "System prompt", v: <span className="text-muted-foreground">"You are a billing support agent…"</span> },
              { k: "Tools", v: "get_invoice, refund, search_docs" },
              { k: "Registry", v: "billing-team" },
            ]}
          />
        </div>
        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Status timeline</p>
          <ol className="space-y-3">
            {[
              ["Ready", "Rollout complete · 2 replicas", "success"],
              ["Scaling", "KEDA scaled 1→2 on queue depth", "info"],
              ["Deployed", "Image pulled, container started", "muted"],
              ["Created", "AgentDeployment applied", "muted"],
            ].map(([t, d, tone], i) => (
              <li key={i} className="flex gap-3">
                <div className="flex flex-col items-center">
                  <span className={`h-2.5 w-2.5 rounded-full ${tone === "success" ? "bg-success" : tone === "info" ? "bg-info" : "bg-border-strong"}`} />
                  {i < 3 && <span className="mt-1 h-8 w-px bg-border" />}
                </div>
                <div>
                  <p className="text-sm font-medium">{t}</p>
                  <p className="text-xs text-muted-foreground">{d}</p>
                </div>
              </li>
            ))}
          </ol>
        </div>
      </div>
      <RunPanel />
    </div>
  );
}

function RunPanel({ readOnly }: { readOnly?: boolean }) {
  return (
    <div className="rounded-lg border bg-card p-4 shadow-card">
      <div className="mb-3 flex items-center gap-2">
        <Play className="h-4 w-4 text-primary" />
        <p className="text-sm font-medium">Run</p>
      </div>
      {readOnly ? (
        <p className="text-xs text-muted-foreground">
          Invoking requires create on runs — hidden for viewers.
        </p>
      ) : (
        <>
          <Textarea rows={4} defaultValue="What's the status of order #4021?" className="text-sm" />
          <Button className="mt-3 w-full" size="sm"><Play className="h-4 w-4" />Run agent</Button>
          <div className="mt-4 rounded-md bg-surface-3 p-3 text-xs">
            <p className="mb-1 text-muted-foreground">Last response</p>
            <p>Order #4021 shipped on Jul 2 and is out for delivery.</p>
            <p className="mt-2 text-muted-foreground">240 tok · $0.001 · 1.2s · trace →</p>
          </div>
        </>
      )}
    </div>
  );
}

function LogsTab() {
  const lines = [
    ["12:04:11", "INFO", "run start id=r-88a2 user=alex.dev"],
    ["12:04:11", "INFO", "model call claude-sonnet-4 tools=3"],
    ["12:04:12", "DEBUG", "tool get_invoice args={id:4021}"],
    ["12:04:12", "INFO", "tool get_invoice ok 180ms"],
    ["12:04:13", "INFO", "run complete 240 tok $0.001"],
  ];
  return (
    <div className="overflow-hidden rounded-lg border bg-surface-3">
      <div className="flex items-center justify-between border-b bg-card/60 px-4 py-2">
        <div className="flex items-center gap-2 text-sm">
          <Terminal className="h-4 w-4" /> Live tail
          <span className="flex h-2 w-2 items-center"><span className="h-2 w-2 animate-pulse rounded-full bg-success" /></span>
        </div>
        <Input placeholder="filter…" className="h-7 w-40 text-xs" />
      </div>
      <pre className="max-h-72 overflow-y-auto p-4 font-mono text-xs leading-relaxed">
        {lines.map(([ts, lvl, msg], i) => (
          <div key={i} className="flex gap-3">
            <span className="text-muted-foreground">{ts}</span>
            <span className={lvl === "DEBUG" ? "text-muted-foreground" : lvl === "INFO" ? "text-info" : "text-warning"}>{lvl}</span>
            <span>{msg}</span>
          </div>
        ))}
      </pre>
    </div>
  );
}

export function AgentDetailWireframe() {
  const [tab, setTab] = React.useState<string>("Overview");
  return (
    <div className="space-y-4">
      <Note>
        Agent detail: tabbed (overview / status timeline · logs tail · runs ·
        bindings) with a Run panel. Click a tab to switch. Logs is a live tail;
        Runs reuses the DataTable.
      </Note>
      <div className="flex justify-end gap-2">
        {DETAIL_TABS.map((t) => (
          <Button key={t} size="sm" variant={tab === t ? "default" : "outline"} onClick={() => setTab(t)}>{t}</Button>
        ))}
      </div>
      <ScreenFrame>
        <ConsoleChrome
          active="agents"
          title="billing-support-agent"
          headerActions={
            <div className="flex gap-2">
              <Button size="sm" variant="outline">Edit</Button>
              <Button size="sm" variant="outline">Delete</Button>
            </div>
          }
        >
          <Breadcrumb parts={["Agents", "prod", "billing-support-agent"]} />
          <div className="mb-4 flex items-center gap-3">
            <h2 className="text-xl font-semibold tracking-snug">billing-support-agent</h2>
            <Badge variant="success" className="text-[10px]">Ready</Badge>
          </div>
          <WireTabs tabs={[...DETAIL_TABS]} active={tab} onSelect={setTab} />
          <div className="pt-5">
            {tab === "Overview" && <OverviewTab />}
            {tab === "Logs" && <LogsTab />}
            {tab === "Runs" && <RunsMiniTable />}
            {tab === "Bindings" && <BindingsTab />}
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

export function AgentDetailViewerWireframe() {
  return (
    <div className="space-y-4">
      <Note>
        Viewer variant of the detail page: Edit/Delete gone from the header, the
        Run panel explains why it's hidden — never a blank or a surprise 403.
      </Note>
      <ScreenFrame caption="Viewer variant">
        <ConsoleChrome active="agents" viewer title="billing-support-agent" banner={<ViewerBanner />}>
          <Breadcrumb parts={["Agents", "prod", "billing-support-agent"]} />
          <div className="mb-4 flex items-center gap-3">
            <h2 className="text-xl font-semibold tracking-snug">billing-support-agent</h2>
            <Badge variant="success" className="text-[10px]">Ready</Badge>
          </div>
          <WireTabs tabs={[...DETAIL_TABS]} active="Overview" onSelect={() => {}} />
          <div className="grid gap-6 pt-5 lg:grid-cols-[1fr_18rem]">
            <div className="rounded-lg border bg-card p-5 shadow-card">
              <p className="mb-3 text-sm font-medium">Configuration</p>
              <KeyValue rows={[{ k: "Runtime", v: "managed" }, { k: "Model", v: <span className="font-mono text-xs">claude-sonnet-4</span> }, { k: "Tools", v: "get_invoice, refund" }]} />
            </div>
            <RunPanel readOnly />
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

function RunsMiniTable() {
  interface Run { id: string; when: string; status: string; tokens: number; cost: string }
  const runs: Run[] = [
    { id: "r-88a2", when: "2m ago", status: "ok", tokens: 240, cost: "$0.001" },
    { id: "r-7f10", when: "9m ago", status: "ok", tokens: 512, cost: "$0.003" },
    { id: "r-6c9d", when: "22m ago", status: "error", tokens: 90, cost: "$0.000" },
  ];
  const cols: Column<Run>[] = [
    { id: "id", header: "Run", cell: (r) => <span className="font-mono text-xs">{r.id}</span> },
    { id: "when", header: "When", cell: (r) => r.when },
    { id: "status", header: "Status", cell: (r) => <Badge variant={r.status === "ok" ? "success" : "destructive"} className="text-[10px]">{r.status}</Badge> },
    { id: "tokens", header: "Tokens", className: "text-right", cell: (r) => <span className="tabular-nums">{r.tokens}</span> },
    { id: "cost", header: "Cost", className: "text-right", cell: (r) => r.cost },
  ];
  return <DataTable columns={cols} rows={runs} rowKey={(r) => r.id} onRowClick={() => {}} rangeLabel="Last 3 runs" />;
}

function BindingsTab() {
  return (
    <div className="space-y-2">
      {[
        ["ModelRoute", "claude-sonnet-4", "bound"],
        ["SecretBinding", "anthropic-key", "bound"],
        ["MCPToolBinding", "acme-mcp (3 tools)", "bound"],
        ["MemoryBinding", "—", "none"],
      ].map(([k, v, s]) => (
        <div key={k} className="flex items-center justify-between rounded-md border bg-surface-2/40 px-4 py-3 text-sm">
          <span className="text-muted-foreground">{k}</span>
          <span className="font-mono text-xs">{v}</span>
          <Badge variant={s === "bound" ? "success" : "secondary"} className="text-[10px]">{s}</Badge>
        </div>
      ))}
    </div>
  );
}
