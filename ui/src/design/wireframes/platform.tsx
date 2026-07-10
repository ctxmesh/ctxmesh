import {
  Boxes,
  ListTree,
  Play,
  PlugZap,
  Plus,
  Terminal,
  Trash2,
  Users,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DataTable, type Column } from "@/components/kit";
import { ConsoleChrome } from "@/design/console-chrome";
import { Note, ScreenFrame, WireSection } from "@/design/scaffold";

// ── Registry editor (members / roles / allowlist) ──────────────────────────
export function RegistryEditorWireframe() {
  return (
    <div className="space-y-4">
      <Note>AgentRegistry editor (§31): members + roles + the tool allowlist — the multi-tenant boundary editor. Viewers see this read-only.</Note>
      <ScreenFrame>
        <ConsoleChrome active="registries" title="billing-team" headerActions={<Button size="sm" variant="outline">Save changes</Button>}>
          <div className="mx-auto max-w-3xl space-y-6">
            <div className="flex items-center gap-3">
              <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-accent text-accent-foreground"><Users className="h-5 w-5" /></div>
              <div><h2 className="text-lg font-semibold tracking-snug">billing-team</h2><p className="text-xs text-muted-foreground">namespace: prod · 64 agents</p></div>
            </div>

            <WireSection title="Members & roles" aside={<Button size="sm" variant="outline"><Plus className="h-4 w-4" />Add member</Button>}>
              <div className="space-y-2">
                {[["alex.dev", "admin"], ["sam.ops", "editor"], ["casey.viewer", "viewer"]].map(([u, role]) => (
                  <div key={u} className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-4 py-2.5">
                    <div className="flex h-7 w-7 items-center justify-center rounded-full bg-primary/15 text-[11px] font-semibold text-primary">{u.slice(0, 2).toUpperCase()}</div>
                    <span className="flex-1 text-sm">{u}</span>
                    <select className="h-8 rounded-md border bg-background px-2 text-xs" defaultValue={role}>
                      <option>admin</option><option>editor</option><option>viewer</option>
                    </select>
                    <Button variant="ghost" size="icon"><Trash2 className="h-4 w-4 text-muted-foreground" /></Button>
                  </div>
                ))}
              </div>
            </WireSection>

            <WireSection title="Tool allowlist" aside={<span className="text-xs text-muted-foreground">tools agents in this registry may bind</span>}>
              <div className="flex flex-wrap gap-2 rounded-md border bg-surface-2/40 p-3">
                {["get_invoice", "refund", "search_docs", "send_email"].map((t) => (
                  <Badge key={t} variant="secondary" className="gap-1 text-[11px]">{t}<button className="text-muted-foreground hover:text-foreground">×</button></Badge>
                ))}
                <Button variant="ghost" size="sm" className="h-6"><Plus className="h-3 w-3" />Add tool</Button>
              </div>
            </WireSection>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Routes + Secrets lists ─────────────────────────────────────────────────
interface RouteRow { name: string; model: string; provider: string; ns: string }
interface SecretRow { name: string; ref: string; ns: string; kind: string }

export function RoutesSecretsWireframe() {
  const routes: RouteRow[] = [
    { name: "claude-sonnet-4", model: "claude-sonnet-4", provider: "anthropic", ns: "prod" },
    { name: "claude-opus-4", model: "claude-opus-4", provider: "anthropic", ns: "prod" },
    { name: "gpt-4o", model: "gpt-4o", provider: "openai", ns: "prod" },
  ];
  const secrets: SecretRow[] = [
    { name: "anthropic-key", ref: "anthropic-secret", ns: "prod", kind: "provider" },
    { name: "openai-key", ref: "openai-secret", ns: "prod", kind: "provider" },
    { name: "acme-mcp-token", ref: "acme-mcp-secret", ns: "prod", kind: "mcp" },
  ];
  const routeCols: Column<RouteRow>[] = [
    { id: "name", header: "Route", sortable: true, cell: (r) => <span className="font-medium">{r.name}</span> },
    { id: "model", header: "Model", cell: (r) => <span className="font-mono text-xs">{r.model}</span> },
    { id: "provider", header: "Provider", cell: (r) => <Badge variant="secondary" className="text-[10px]">{r.provider}</Badge> },
    { id: "ns", header: "Namespace", hideOnMobile: true, cell: (r) => r.ns },
  ];
  const secretCols: Column<SecretRow>[] = [
    { id: "name", header: "Binding", sortable: true, cell: (r) => <span className="font-medium">{r.name}</span> },
    { id: "ref", header: "Secret ref", cell: (r) => <span className="font-mono text-xs">{r.ref}</span> },
    { id: "kind", header: "Kind", cell: (r) => <Badge variant="secondary" className="text-[10px]">{r.kind}</Badge> },
    { id: "ns", header: "Namespace", hideOnMobile: true, cell: (r) => r.ns },
  ];
  return (
    <div className="space-y-4">
      <Note>Model routes + secret bindings — both on the DataTable / list contract. Secret <em>values</em> never render; only the reference does.</Note>
      <ScreenFrame caption="Model routes">
        <ConsoleChrome active="routes" title="Model routes" headerActions={<Button size="sm"><Plus className="h-4 w-4" />New route</Button>}>
          <DataTable columns={routeCols} rows={routes} rowKey={(r) => `${r.ns}/${r.name}`} query="" onQueryChange={() => {}} queryPlaceholder="Filter routes…" hasNext={false} rangeLabel="3 routes" onRowClick={() => {}} rowActions={() => <Button variant="ghost" size="sm">Edit</Button>} />
        </ConsoleChrome>
      </ScreenFrame>
      <ScreenFrame caption="Secret bindings">
        <ConsoleChrome active="secrets" title="Secret bindings" headerActions={<Button size="sm"><Plus className="h-4 w-4" />New binding</Button>}>
          <DataTable columns={secretCols} rows={secrets} rowKey={(r) => `${r.ns}/${r.name}`} query="" onQueryChange={() => {}} queryPlaceholder="Filter bindings…" hasNext={false} rangeLabel="3 bindings" onRowClick={() => {}} rowActions={() => <Button variant="ghost" size="sm">Edit</Button>} />
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Settings / providers page ──────────────────────────────────────────────
export function SettingsProvidersWireframe() {
  const providers = [
    { name: "Anthropic", models: 3, status: "connected", icon: PlugZap },
    { name: "OpenAI", models: 2, status: "connected", icon: PlugZap },
  ];
  return (
    <div className="space-y-4">
      <Note>Settings / providers — connected providers with their route counts; "Connect a provider" launches the wizard. Kill-switch hides connect on hardened installs.</Note>
      <ScreenFrame>
        <ConsoleChrome active="settings" title="Settings">
          <div className="mx-auto max-w-3xl space-y-6">
            <div className="flex gap-1 border-b">
              {["Providers", "Members", "Cluster", "About"].map((t, i) => (
                <button key={t} className={`-mb-px border-b-2 px-3 py-2 text-sm font-medium ${i === 0 ? "border-primary text-foreground" : "border-transparent text-muted-foreground"}`}>{t}</button>
              ))}
            </div>
            <WireSection title="Connected providers" aside={<Button size="sm"><Plus className="h-4 w-4" />Connect a provider</Button>}>
              <div className="space-y-2">
                {providers.map((p) => (
                  <div key={p.name} className="flex items-center gap-3 rounded-lg border bg-card px-4 py-3 shadow-card">
                    <div className="flex h-9 w-9 items-center justify-center rounded-md bg-surface-2"><p.icon className="h-4 w-4 text-primary" /></div>
                    <div className="flex-1"><p className="text-sm font-medium">{p.name}</p><p className="text-xs text-muted-foreground">{p.models} model routes</p></div>
                    <Badge variant="success" className="text-[10px]">{p.status}</Badge>
                    <Button variant="ghost" size="sm">Manage</Button>
                  </div>
                ))}
              </div>
            </WireSection>
            <WireSection title="Danger zone">
              <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
                <div className="flex items-center justify-between">
                  <div><p className="text-sm font-medium">Disconnect a provider</p><p className="text-xs text-muted-foreground">Removes routes; agents using them will fail until re-bound.</p></div>
                  <Button variant="destructive" size="sm">Disconnect…</Button>
                </div>
              </div>
            </WireSection>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── Create-agent entrance (the fork) ───────────────────────────────────────
export function CreateEntranceWireframe() {
  return (
    <div className="space-y-4">
      <Note>The create-agent entrance — the fork between the prompt-first "Describe it" hero and the "Configure it" form. "Describe" is visually the primary path.</Note>
      <ScreenFrame>
        <ConsoleChrome active="agents" title="New agent">
          <div className="mx-auto grid max-w-3xl gap-4 pt-6 md:grid-cols-2">
            <button className="rounded-xl border-2 border-primary bg-accent/40 p-6 text-left shadow-card transition-shadow hover:shadow-elevated">
              <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground"><Boxes className="h-6 w-6" /></div>
              <p className="text-base font-semibold">Describe it</p>
              <p className="mt-1 text-sm text-muted-foreground">Say what it should do in a sentence. We generate a validated config you review before creating. <span className="font-medium text-primary">Recommended</span>.</p>
            </button>
            <button className="rounded-xl border p-6 text-left shadow-card transition-shadow hover:shadow-elevated">
              <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-xl bg-surface-2"><Terminal className="h-6 w-6 text-muted-foreground" /></div>
              <p className="text-base font-semibold">Configure it</p>
              <p className="mt-1 text-sm text-muted-foreground">A guided multi-step form — full control over runtime, model, prompt, and tools.</p>
            </button>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

// ── dev --ui chrome variant ────────────────────────────────────────────────
export function DevModeWireframe() {
  return (
    <div className="space-y-4">
      <Note>
        `agent-engine dev --ui` (§31, M18): the same console served locally against
        the Compose loop — a "dev" badge, reduced nav (Agents / Traces / Runs only),
        no cluster. This is how a developer gets define→run→trace on a laptop.
      </Note>
      <ScreenFrame caption="Local dev mode — reduced surface">
        <ConsoleChrome
          active="agents"
          devMode
          user={{ name: "localhost", groups: ["local dev loop"], persona: "dev" }}
          title="Agents (local)"
          banner={
            <div className="flex items-center gap-2 border-b border-info/30 bg-info/5 px-6 py-2 text-xs text-muted-foreground">
              <Terminal className="h-3.5 w-3.5 text-info" />
              Running against the local Compose dev loop — no cluster. Full CRUD and scale UX are cluster-only.
            </div>
          }
        >
          <div className="space-y-4">
            <div className="flex items-center justify-between">
              <h2 className="text-lg font-semibold tracking-snug">my-dev-agent</h2>
              <Badge variant="secondary" className="text-[10px]">running locally</Badge>
            </div>
            <div className="grid gap-4 lg:grid-cols-[1fr_18rem]">
              <div className="overflow-hidden rounded-lg border bg-surface-3">
                <div className="flex items-center gap-2 border-b bg-card/60 px-4 py-2 text-sm"><ListTree className="h-4 w-4" />Trace (last run)</div>
                <div className="p-4 text-xs text-muted-foreground">run → llm → tool: echo → llm · 620ms</div>
              </div>
              <div className="rounded-lg border bg-card p-4 shadow-card">
                <div className="mb-2 flex items-center gap-2"><Play className="h-4 w-4 text-primary" /><p className="text-sm font-medium">Run</p></div>
                <Input placeholder="prompt…" defaultValue="hello" className="text-sm" />
                <Button size="sm" className="mt-3 w-full"><Play className="h-4 w-4" />Run</Button>
              </div>
            </div>
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}
