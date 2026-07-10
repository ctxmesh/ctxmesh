import * as React from "react";
import { Check, GitBranch, Play, Plus, Search, Wrench, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ConsoleChrome } from "@/design/console-chrome";
import { Note, ScreenFrame, WireSection } from "@/design/scaffold";

// ── Tool catalog (curated + user-added + pending-approval) ─────────────────
export function ToolCatalogWireframe() {
  const tools = [
    { name: "get_invoice", server: "acme-mcp", kind: "user-added", status: "ready", desc: "Fetch an invoice by id" },
    { name: "search_docs", server: "platform", kind: "curated", status: "ready", desc: "RAG search over the docs corpus" },
    { name: "refund", server: "acme-mcp", kind: "user-added", status: "pending", desc: "Issue a refund (write)" },
    { name: "web_search", server: "platform", kind: "curated", status: "ready", desc: "General web search" },
    { name: "send_slack", server: "acme-mcp", kind: "user-added", status: "pending", desc: "Post to a Slack channel" },
    { name: "sql_query", server: "platform", kind: "curated", status: "ready", desc: "Read-only warehouse queries" },
  ];
  return (
    <div className="space-y-4">
      <Note>
        One merged tool catalog (§28.2): operator-curated + user-added, with
        pending-approval badges for the hardened-mode approval queue. Filterable;
        each card → the tool detail drawer.
      </Note>
      <ScreenFrame>
        <ConsoleChrome active="tools" title="Tool catalog" headerActions={<Button size="sm"><Plus className="h-4 w-4" />Add MCP server</Button>}>
          <div className="mb-4 flex items-center gap-3">
            <div className="relative min-w-[18rem] flex-1">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input placeholder="Search tools…" className="pl-9" />
            </div>
            <div className="flex gap-1.5">
              {["All", "Curated", "User-added", "Pending"].map((f, i) => (
                <button key={f} className={`rounded-full border px-3 py-1.5 text-xs ${i === 0 ? "border-primary bg-accent text-accent-foreground" : "text-muted-foreground hover:bg-surface-2"}`}>{f}</button>
              ))}
            </div>
          </div>
          <div className="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
            {tools.map((t) => (
              <button key={t.name} className="rounded-lg border bg-card p-4 text-left shadow-card transition-shadow hover:shadow-elevated">
                <div className="mb-2 flex items-center gap-2">
                  <div className="flex h-8 w-8 items-center justify-center rounded-md bg-surface-2"><Wrench className="h-4 w-4 text-primary" /></div>
                  <span className="font-mono text-sm">{t.name}</span>
                  {t.status === "pending" && <Badge variant="warning" className="ml-auto text-[9px]">pending</Badge>}
                </div>
                <p className="mb-3 text-xs text-muted-foreground">{t.desc}</p>
                <div className="flex items-center gap-2">
                  <Badge variant="secondary" className="text-[9px]">{t.kind}</Badge>
                  <span className="text-[10px] text-muted-foreground">{t.server}</span>
                </div>
              </button>
            ))}
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Eval builder + results ─────────────────────────────────────────────────
export function EvalWireframe() {
  const [tab, setTab] = React.useState<"build" | "results">("build");
  return (
    <div className="space-y-4">
      <Note>EvalSuite builder + results (§17). Toggle build ↔ results.</Note>
      <div className="flex justify-end gap-2">
        <Button size="sm" variant={tab === "build" ? "default" : "outline"} onClick={() => setTab("build")}>Builder</Button>
        <Button size="sm" variant={tab === "results" ? "default" : "outline"} onClick={() => setTab("results")}>Results</Button>
      </div>
      <ScreenFrame>
        <ConsoleChrome active="evals" title={tab === "build" ? "New eval suite" : "Eval results"}>
          {tab === "build" ? (
            <div className="mx-auto max-w-2xl space-y-5">
              <div className="grid grid-cols-2 gap-4">
                <div className="space-y-1.5"><Label>Suite name</Label><Input defaultValue="billing-accuracy" /></div>
                <div className="space-y-1.5"><Label>Target agent</Label><div className="flex h-9 items-center rounded-md border bg-background px-3 text-sm">billing-support-agent</div></div>
              </div>
              <WireSection title="Cases">
                <div className="space-y-2">
                  {[
                    ["What's the status of #4021?", "contains: shipped"],
                    ["Refund my last order", "calls tool: refund"],
                  ].map(([inp, assert], i) => (
                    <div key={i} className="grid grid-cols-[1fr_1fr_auto] items-center gap-2 rounded-md border bg-surface-2/40 p-2">
                      <Input defaultValue={inp} className="text-xs" />
                      <Input defaultValue={assert} className="font-mono text-xs" />
                      <Button variant="ghost" size="icon"><X className="h-4 w-4" /></Button>
                    </div>
                  ))}
                  <Button variant="outline" size="sm"><Plus className="h-4 w-4" />Add case</Button>
                </div>
              </WireSection>
              <div className="flex justify-end gap-2 border-t pt-4">
                <Button variant="outline">Save</Button>
                <Button><Play className="h-4 w-4" />Save &amp; run</Button>
              </div>
            </div>
          ) : (
            <div className="space-y-4">
              <div className="grid grid-cols-4 gap-4">
                {[["Pass rate", "82%"], ["Cases", "50"], ["Passed", "41"], ["Cost", "$0.34"]].map(([l, v]) => (
                  <div key={l} className="rounded-lg border bg-card p-4 shadow-card"><p className="text-xs uppercase tracking-wide text-muted-foreground">{l}</p><p className="mt-1 text-2xl font-semibold tracking-tight">{v}</p></div>
                ))}
              </div>
              <div className="overflow-hidden rounded-lg border bg-card shadow-card">
                <table className="w-full text-left text-sm">
                  <thead><tr className="border-b bg-surface-2/60 text-xs uppercase tracking-wide text-muted-foreground"><th className="px-4 py-2.5">Case</th><th className="px-4 py-2.5">Assertion</th><th className="px-4 py-2.5">Result</th></tr></thead>
                  <tbody>
                    {[["What's the status of #4021?", "contains: shipped", true], ["Refund my last order", "calls tool: refund", true], ["Cancel subscription", "contains: cancelled", false]].map(([c, a, ok], i) => (
                      <tr key={i} className="border-b border-border/60 last:border-0">
                        <td className="px-4 py-2.5">{c as string}</td>
                        <td className="px-4 py-2.5 font-mono text-xs text-muted-foreground">{a as string}</td>
                        <td className="px-4 py-2.5">{ok ? <Badge variant="success" className="text-[10px]"><Check className="mr-1 h-3 w-3" />pass</Badge> : <Badge variant="destructive" className="text-[10px]">fail</Badge>}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Prompt version diff (two-pane) ─────────────────────────────────────────
export function PromptDiffWireframe() {
  const left = [
    ["You are a billing support agent.", "same"],
    ["Answer using the tools provided.", "same"],
    ["Be concise.", "removed"],
    ["", "empty"],
  ] as const;
  const right = [
    ["You are a billing support agent for Acme.", "changed"],
    ["Answer using the tools provided.", "same"],
    ["Always cite the order id you looked up.", "added"],
    ["Be concise and friendly.", "added"],
  ] as const;
  const tone = (t: string) =>
    t === "added" || t === "changed" ? "bg-success/10 border-l-2 border-success" : t === "removed" ? "bg-destructive/10 border-l-2 border-destructive" : "";
  return (
    <div className="space-y-4">
      <Note>Prompt version diff (§31) — two-pane, added/removed/changed lines highlighted. Restore an older version from the header.</Note>
      <ScreenFrame>
        <ConsoleChrome active="prompts" title="billing-prompt · diff" headerActions={<Button size="sm" variant="outline">Restore v3</Button>}>
          <div className="mb-3 flex items-center gap-2 text-sm">
            <GitBranch className="h-4 w-4 text-muted-foreground" />
            <Badge variant="secondary" className="text-[10px]">v3</Badge>
            <span className="text-muted-foreground">→</span>
            <Badge variant="secondary" className="text-[10px]">v4 (current)</Badge>
          </div>
          <div className="grid grid-cols-2 gap-4">
            {[["Version 3", left], ["Version 4 (current)", right]].map(([title, rows]) => (
              <div key={title as string} className="overflow-hidden rounded-lg border bg-card shadow-card">
                <div className="border-b bg-surface-2/60 px-4 py-2 text-xs font-medium text-muted-foreground">{title as string}</div>
                <div className="font-mono text-xs">
                  {(rows as readonly (readonly [string, string])[]).map(([line, t], i) => (
                    <div key={i} className={`px-4 py-1.5 ${tone(t)}`}>{line || " "}</div>
                  ))}
                </div>
              </div>
            ))}
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}
