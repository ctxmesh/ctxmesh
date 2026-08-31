import * as React from "react";
import {
  ArrowRight,
  Boxes,
  Command,
  KeyRound,
  PlugZap,
  Sparkles,
  TerminalSquare,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { CommandPalette } from "@/components/kit";
import { EmptyState } from "@/components/kit";
import { ConsoleChrome } from "@/design/console-chrome";
import { Note, ScreenFrame } from "@/design/scaffold";

// ── Login (+ wrong-token error) ────────────────────────────────────────────
function LoginCard({ error }: { error?: boolean }) {
  return (
    <div className="mx-auto w-full max-w-md rounded-xl border bg-card p-8 shadow-elevated">
      <div className="mb-6 flex flex-col items-center text-center">
        <div className="mb-3 flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-sm">
          <Boxes className="h-6 w-6" />
        </div>
        <h1 className="text-xl font-semibold tracking-snug">
          Sign in to ctxmesh
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Paste a Kubernetes bearer token. It's held for this session only and
          sent as your identity on every request.
        </p>
      </div>

      <div className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="token">Bearer token</Label>
          <div className="relative">
            <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              id="token"
              type="password"
              placeholder="eyJhbGciOiJSUzI1NiIsImtpZC…"
              defaultValue={error ? "expired-token-xxxxx" : ""}
              className="pl-9 font-mono text-xs"
              aria-invalid={error}
            />
          </div>
          {error && (
            <p className="text-xs text-destructive">
              That token was rejected (401). It may be expired or for another
              cluster. Run <span className="font-mono">kubectl create token</span>{" "}
              and paste a fresh one.
            </p>
          )}
        </div>

        <Button className="w-full">
          Continue
          <ArrowRight className="h-4 w-4" />
        </Button>

        <div className="rounded-md border bg-surface-2/50 p-3 text-xs text-muted-foreground">
          <p className="mb-1 flex items-center gap-1.5 font-medium text-foreground">
            <TerminalSquare className="h-3.5 w-3.5" /> First time?
          </p>
          Get a token with{" "}
          <span className="font-mono">
            kubectl create token &lt;sa&gt; -n &lt;ns&gt;
          </span>
          . OIDC single-sign-on arrives in M18.
        </div>
      </div>
    </div>
  );
}

export function LoginWireframe() {
  return (
    <div className="space-y-6">
      <Note>
        Token login (ADR 0012). K8s RBAC stays the only authorization — the token
        is the identity. The right panel is a branded value-prop so the first
        screen doesn't feel like a raw form.
      </Note>
      <div className="grid gap-6 lg:grid-cols-2">
        <ScreenFrame caption="Default">
          <div className="grid min-h-[30rem] lg:grid-cols-[1fr]">
            <div className="flex items-center justify-center bg-background p-8">
              <LoginCard />
            </div>
          </div>
        </ScreenFrame>
        <ScreenFrame caption="Wrong / expired token → honest 401">
          <div className="flex min-h-[30rem] items-center justify-center bg-background p-8">
            <LoginCard error />
          </div>
        </ScreenFrame>
      </div>
    </div>
  );
}

// ── App shell + cmd-K ──────────────────────────────────────────────────────
export function ShellWireframe() {
  const [paletteOpen, setPaletteOpen] = React.useState(true);

  const commands = [
    { id: "n-dash", group: "Navigate", label: "Dashboard", icon: Boxes, onRun: () => {}, hint: "G D" },
    { id: "n-agents", group: "Navigate", label: "Agents", icon: Boxes, onRun: () => {}, hint: "G A" },
    { id: "n-traces", group: "Navigate", label: "Traces", icon: Boxes, onRun: () => {} },
    { id: "a-describe", group: "Actions", label: "Describe an agent…", icon: Sparkles, onRun: () => {} },
    { id: "a-provider", group: "Actions", label: "Connect a provider…", icon: PlugZap, onRun: () => {} },
    { id: "r-1", group: "Recent agents", label: "billing-support-agent", icon: Boxes, onRun: () => {} },
    { id: "r-2", group: "Recent agents", label: "docs-rag-agent", icon: Boxes, onRun: () => {} },
  ];

  return (
    <div className="space-y-6">
      <Note>
        The persistent shell every surface renders inside: grouped sidebar IA
        (Overview / Build / Observe / Platform / Settings), a who-am-I header
        with persona badge, and the ⌘K palette open over it. Toggle the palette
        to inspect it.
      </Note>
      <div className="flex justify-end">
        <Button variant="outline" size="sm" onClick={() => setPaletteOpen((v) => !v)}>
          <Command className="h-4 w-4" />
          {paletteOpen ? "Hide" : "Show"} ⌘K palette
        </Button>
      </div>
      <ScreenFrame caption="Shell + who-am-I header + ⌘K overlay">
        <div className="relative">
          <ConsoleChrome active="dashboard" title="Dashboard">
            <EmptyState
              icon={Boxes}
              title="This is the app shell"
              description="Every surface mounts into this outlet. The sidebar, header identity, and ⌘K palette are constant; only this content region changes per route."
            />
          </ConsoleChrome>
          {paletteOpen && (
            <div className="absolute inset-0">
              <CommandPalette
                open
                onClose={() => setPaletteOpen(false)}
                commands={commands}
              />
            </div>
          )}
        </div>
      </ScreenFrame>
    </div>
  );
}

// ── Dashboard v2 (populated + teaching empty state) ────────────────────────
function StatCard({
  label,
  value,
  sub,
}: {
  label: string;
  value: string;
  sub: string;
}) {
  return (
    <div className="rounded-lg border bg-card p-4 shadow-card">
      <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </p>
      <p className="mt-1 text-2xl font-semibold tracking-tight">{value}</p>
      <p className="mt-0.5 text-xs text-muted-foreground">{sub}</p>
    </div>
  );
}

export function DashboardWireframe() {
  const [empty, setEmpty] = React.useState(false);

  return (
    <div className="space-y-6">
      <Note>
        Dashboard v2 as the operator landing. Toggle to the teaching empty state
        — a fresh install has no providers, so the primary CTA is "Connect a
        provider", not an empty chart grid.
      </Note>
      <div className="flex justify-end">
        <Button variant="outline" size="sm" onClick={() => setEmpty((v) => !v)}>
          {empty ? "Show populated" : "Show teaching empty state"}
        </Button>
      </div>
      <ScreenFrame caption={empty ? "Fresh install — teaching empty state" : "Populated"}>
        <ConsoleChrome
          active="dashboard"
          title="Dashboard"
          headerActions={
            !empty && (
              <Button size="sm">
                <Sparkles className="h-4 w-4" />
                Describe an agent
              </Button>
            )
          }
        >
          {empty ? (
            <EmptyState
              icon={PlugZap}
              title="Connect a provider to get started"
              description="ctxmesh needs a model provider (Anthropic, OpenAI, …) before you can create an agent. Paste a key once — we create the Secret, binding, and route for you."
              action={{ label: "Connect a provider", icon: PlugZap }}
              secondaryAction={{ label: "What is a provider?" }}
            />
          ) : (
            <div className="space-y-6">
              <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
                <StatCard label="Agents" value="18" sub="16 ready · 2 pending" />
                <StatCard label="Runs (24h)" value="1,204" sub="98.2% success" />
                <StatCard label="Spend (24h)" value="$42.18" sub="↓ 6% vs prev" />
                <StatCard label="p95 latency" value="1.8s" sub="tool-calling loop" />
              </div>
              <div className="grid gap-4 lg:grid-cols-3">
                <div className="rounded-lg border bg-card p-4 shadow-card lg:col-span-2">
                  <div className="mb-3 flex items-center justify-between">
                    <p className="text-sm font-medium">Cost by model (7d)</p>
                    <Badge variant="secondary" className="text-[10px]">
                      native
                    </Badge>
                  </div>
                  <div className="flex h-40 items-end gap-2">
                    {[60, 45, 72, 38, 90, 55, 68].map((h, i) => (
                      <div
                        key={i}
                        className="flex-1 rounded-t bg-primary/70"
                        style={{ height: `${h}%` }}
                      />
                    ))}
                  </div>
                </div>
                <div className="rounded-lg border bg-card p-4 shadow-card">
                  <p className="mb-3 text-sm font-medium">Recent runs</p>
                  <div className="space-y-2">
                    {["checkout-flow", "docs-lookup", "triage-ticket"].map((r) => (
                      <div
                        key={r}
                        className="flex items-center justify-between rounded-md border bg-surface-2/40 px-3 py-2 text-sm"
                      >
                        <span className="truncate">{r}</span>
                        <Badge variant="success" className="text-[10px]">
                          ok
                        </Badge>
                      </div>
                    ))}
                  </div>
                </div>
              </div>
            </div>
          )}
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}
