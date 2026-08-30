import * as React from "react";
import {
  Check,
  ChevronRight,
  KeyRound,
  Loader2,
  PlugZap,
  Search,
  Sparkles,
  Wrench,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Wizard, type WizardStep } from "@/components/kit";
import { ConsoleChrome } from "@/design/console-chrome";
import { KeyValue, Note, ScreenFrame } from "@/design/scaffold";

// A framed wizard host so each wizard wireframe reads as a screen; the step
// index is local so the reviewer can walk the flow.
function WizardScreen({
  active,
  title,
  steps,
  finishLabel,
}: {
  active: string;
  title: string;
  steps: WizardStep[];
  finishLabel: string;
}) {
  const [current, setCurrent] = React.useState(0);
  return (
    <ScreenFrame>
      <ConsoleChrome active={active} title={title}>
        <div className="mx-auto max-w-3xl rounded-lg border bg-card p-6 shadow-card">
          <Wizard
            steps={steps}
            current={current}
            onStepChange={setCurrent}
            finishLabel={finishLabel}
          />
        </div>
      </ConsoleChrome>
    </ScreenFrame>
  );
}

// ── Connect-provider wizard ────────────────────────────────────────────────
export function ConnectProviderWireframe() {
  const providers = [
    { id: "anthropic", name: "Anthropic", models: "Claude" },
    { id: "openai", name: "OpenAI", models: "GPT" },
    { id: "google", name: "Google", models: "Gemini" },
    { id: "custom", name: "Custom / OpenAI-compatible", models: "base URL" },
  ];
  const steps: WizardStep[] = [
    {
      id: "provider",
      title: "Provider",
      description: "Pick who serves the model",
      content: (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Which provider are you connecting?
          </p>
          <div className="grid grid-cols-2 gap-3">
            {providers.map((p, i) => (
              <button
                key={p.id}
                type="button"
                className={`flex items-center gap-3 rounded-lg border p-3 text-left transition-colors hover:bg-surface-2 ${i === 0 ? "border-primary ring-1 ring-primary" : ""}`}
              >
                <div className="flex h-9 w-9 items-center justify-center rounded-md bg-surface-2">
                  <PlugZap className="h-4 w-4 text-primary" />
                </div>
                <div>
                  <p className="text-sm font-medium">{p.name}</p>
                  <p className="text-xs text-muted-foreground">{p.models}</p>
                </div>
              </button>
            ))}
          </div>
        </div>
      ),
    },
    {
      id: "key",
      title: "API key",
      description: "Pasted once, stored server-side",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="pk">Anthropic API key</Label>
            <div className="relative">
              <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input id="pk" type="password" defaultValue="sk-ant-xxxxxxxx" className="pl-9 font-mono text-xs" />
            </div>
          </div>
          <div className="rounded-md border border-info/30 bg-info/5 p-3 text-xs text-muted-foreground">
            The key goes straight to the BFF, which validates it against the
            provider and stores it as a Kubernetes Secret under <em>your</em>{" "}
            identity. It never returns to the browser and is never logged.
          </div>
        </div>
      ),
    },
    {
      id: "models",
      title: "Models",
      description: "Live from the provider",
      content: (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Key validated. Choose which models to expose as ModelRoutes:
          </p>
          <div className="space-y-2">
            {["claude-opus-4", "claude-sonnet-4", "claude-haiku-4"].map((m, i) => (
              <label
                key={m}
                className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-3 py-2 text-sm"
              >
                <input type="checkbox" defaultChecked={i < 2} className="accent-primary" />
                <span className="font-mono">{m}</span>
                <Badge variant="secondary" className="ml-auto text-[10px]">
                  chat
                </Badge>
              </label>
            ))}
          </div>
        </div>
      ),
    },
    {
      id: "review",
      title: "Done",
      review: true,
      content: (
        <div className="space-y-4">
          <div className="flex items-center gap-2 text-success">
            <Check className="h-5 w-5" />
            <p className="text-sm font-medium text-foreground">
              Ready to create these resources
            </p>
          </div>
          <KeyValue
            rows={[
              { k: "Provider", v: "Anthropic" },
              { k: "Secret", v: <span className="font-mono text-xs">anthropic-key (ns: default)</span> },
              { k: "SecretBinding", v: <span className="font-mono text-xs">anthropic-binding</span> },
              { k: "ModelRoutes", v: "claude-opus-4, claude-sonnet-4" },
            ]}
          />
          <Note>Created server-side as you (ADR 0011). Kill-switch: hardened installs hide this flow.</Note>
        </div>
      ),
    },
  ];
  return (
    <div className="space-y-4">
      <Note>
        The first-run flow (§28.1): provider → paste key once → live model list →
        review. Replaces the M12 "reference an existing Secret" friction.
      </Note>
      <WizardScreen active="providers" title="Connect a provider" steps={steps} finishLabel="Create provider" />
    </div>
  );
}

// ── Add-MCP wizard ─────────────────────────────────────────────────────────
export function AddMcpWireframe() {
  const steps: WizardStep[] = [
    {
      id: "url",
      title: "Server",
      description: "URL or image",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="mcp">MCP server URL</Label>
            <Input id="mcp" defaultValue="https://mcp.acme.dev/sse" className="font-mono text-xs" />
          </div>
          <p className="text-xs text-muted-foreground">
            Or add by image for a sidecar-hosted server. Egress opens per approved
            server only (§20).
          </p>
        </div>
      ),
    },
    {
      id: "probe",
      title: "Probe",
      description: "Handshake + auth check",
      content: (
        <div className="space-y-3">
          <div className="flex items-center gap-2 rounded-md border bg-surface-2/40 px-3 py-2 text-sm">
            <Check className="h-4 w-4 text-success" /> Reachable — MCP handshake ok
          </div>
          <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm">
            <KeyRound className="h-4 w-4 text-warning" /> Server requires bearer auth
          </div>
        </div>
      ),
    },
    {
      id: "auth",
      title: "Auth",
      description: "Bearer key (v1)",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="bt">Bearer token</Label>
            <Input id="bt" type="password" defaultValue="mcp-tok-xxxx" className="font-mono text-xs" />
          </div>
          <Note>
            Stored as a Secret via the SecretBinding pattern; attached at the
            egress hop — never by the browser, never inside the agent container.
            OAuth 2.1 (per-user grants) is M17.
          </Note>
        </div>
      ),
    },
    {
      id: "tools",
      title: "Tools",
      review: true,
      content: (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            <span className="font-medium text-foreground">tools/list</span>{" "}
            discovered 4 tools. They'll be added to the merged catalog:
          </p>
          <div className="space-y-2">
            {[
              ["search_docs", "Full-text search over docs"],
              ["create_ticket", "Open a support ticket"],
              ["get_order", "Fetch an order by id"],
              ["refund", "Issue a refund (write)"],
            ].map(([n, d]) => (
              <div key={n} className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-3 py-2">
                <Wrench className="h-4 w-4 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="font-mono text-sm">{n}</p>
                  <p className="text-xs text-muted-foreground">{d}</p>
                </div>
              </div>
            ))}
          </div>
          <Note>
            On hardened clusters this queues for operator approval into the
            ToolRegistry before binding (§28.2).
          </Note>
        </div>
      ),
    },
  ];
  return (
    <div className="space-y-4">
      <Note>
        BYO-MCP (§28.2): URL → probe → key if auth'd → discovered tools. Self-serve
        by default; policy-gated to operator approval on hardened installs.
      </Note>
      <WizardScreen active="tools" title="Add an MCP server" steps={steps} finishLabel="Add server" />
    </div>
  );
}

// ── Create-agent: prompt-first ("Describe it") ─────────────────────────────
export function CreateAgentDescribeWireframe() {
  const [step, setStep] = React.useState(0);
  const [advanced, setAdvanced] = React.useState(false);

  return (
    <div className="space-y-4">
      <Note>
        The HERO of the arc (§30). Prompt-first entrance → generation review with
        a friendly summary + an Advanced CRD view → converges on the shared review
        step with a tool picker. "Configure it" (the form path) is the sibling
        wireframe; both land on the SAME review.
      </Note>
      <div className="flex justify-end gap-2">
        <Button variant={step === 0 ? "default" : "outline"} size="sm" onClick={() => setStep(0)}>1 · Describe</Button>
        <Button variant={step === 1 ? "default" : "outline"} size="sm" onClick={() => setStep(1)}>2 · Review generated</Button>
        <Button variant={step === 2 ? "default" : "outline"} size="sm" onClick={() => setStep(2)}>3 · Tools + create</Button>
      </div>

      {step === 0 && (
        <ScreenFrame caption="Prompt-first hero">
          <ConsoleChrome active="agents" title="New agent">
            <div className="mx-auto max-w-2xl py-10 text-center">
              <div className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-elevated">
                <Sparkles className="h-7 w-7" />
              </div>
              <h1 className="text-3xl font-semibold tracking-tight">
                Describe your agent
              </h1>
              <p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground">
                Say what it should do in a sentence. We generate a validated
                config you review before anything is created.
              </p>
              <div className="mx-auto mt-6 max-w-xl text-left">
                <Textarea
                  rows={4}
                  className="text-sm"
                  defaultValue="A support agent that looks up orders, answers from our docs, and can open a ticket when it can't resolve the issue."
                />
                <div className="mt-3 flex items-center justify-between">
                  <div className="flex flex-wrap gap-2">
                    {["Summarizer", "RAG over docs", "Triage bot"].map((t) => (
                      <button key={t} className="rounded-full border bg-surface-2/60 px-3 py-1 text-xs text-muted-foreground hover:bg-surface-2">
                        {t}
                      </button>
                    ))}
                  </div>
                  <Button onClick={() => setStep(1)}>
                    <Sparkles className="h-4 w-4" />
                    Generate
                  </Button>
                </div>
                <p className="mt-3 text-center text-xs text-muted-foreground">
                  Prefer full control?{" "}
                  <span className="font-medium text-primary">Configure it manually →</span>
                </p>
              </div>
            </div>
          </ConsoleChrome>
        </ScreenFrame>
      )}

      {step === 1 && (
        <ScreenFrame caption="Generation review — friendly summary + Advanced CRD">
          <ConsoleChrome
            active="agents"
            title="New agent"
            headerActions={
              <Badge variant="secondary" className="text-[10px]">
                generation: $0.004 · 1,240 tok
              </Badge>
            }
          >
            <div className="mx-auto max-w-2xl space-y-4">
              <div className="rounded-lg border bg-card p-5 shadow-card">
                <div className="mb-3 flex items-center justify-between">
                  <p className="text-sm font-medium">Here's what we'll build</p>
                  <Button variant="outline" size="sm">
                    <Sparkles className="h-4 w-4" /> Regenerate
                  </Button>
                </div>
                <KeyValue
                  rows={[
                    { k: "Name", v: "support-agent" },
                    { k: "Runtime", v: <Badge variant="secondary" className="text-[10px]">managed</Badge> },
                    { k: "Model", v: <span className="font-mono text-xs">claude-sonnet-4</span> },
                    { k: "System prompt", v: <span className="text-muted-foreground">"You are a support agent for Acme…"</span> },
                    { k: "Tools", v: "get_order, search_docs, create_ticket" },
                  ]}
                />
              </div>
              <button
                onClick={() => setAdvanced((v) => !v)}
                className="flex w-full items-center gap-2 text-sm font-medium text-muted-foreground hover:text-foreground"
              >
                <ChevronRight className={`h-4 w-4 transition-transform ${advanced ? "rotate-90" : ""}`} />
                Advanced — view the generated agent.yaml / CRDs
              </button>
              {advanced && (
                <pre className="overflow-x-auto rounded-md bg-surface-3 p-4 text-xs">{`apiVersion: agentry.dev/v1
kind: AgentDeployment
metadata:
  name: support-agent
spec:
  runtime: managed
  model: claude-sonnet-4
  systemPrompt: |
    You are a support agent for Acme…
  tools:
    - get_order
    - search_docs
    - create_ticket`}</pre>
              )}
              <div className="flex justify-between">
                <Button variant="ghost" onClick={() => setStep(0)}>Back</Button>
                <Button onClick={() => setStep(2)}>Looks good → tools</Button>
              </div>
            </div>
          </ConsoleChrome>
        </ScreenFrame>
      )}

      {step === 2 && <SharedReviewScreen />}
    </div>
  );
}

// ── Create-agent: "Configure it" (form path) ───────────────────────────────
export function CreateAgentConfigureWireframe() {
  const steps: WizardStep[] = [
    {
      id: "basics",
      title: "Basics",
      description: "Name, runtime, model",
      content: (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="an">Name</Label>
              <Input id="an" defaultValue="support-agent" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ans">Namespace</Label>
              <Input id="ans" defaultValue="default" />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label>Runtime</Label>
            <div className="grid grid-cols-2 gap-3">
              <button className="rounded-lg border border-primary p-3 text-left ring-1 ring-primary">
                <p className="text-sm font-medium">Managed</p>
                <p className="text-xs text-muted-foreground">Stock tool-calling loop — no Docker build</p>
              </button>
              <button className="rounded-lg border p-3 text-left hover:bg-surface-2">
                <p className="text-sm font-medium">Custom image</p>
                <p className="text-xs text-muted-foreground">Bring your own container</p>
              </button>
            </div>
          </div>
        </div>
      ),
    },
    {
      id: "behavior",
      title: "Behavior",
      description: "Prompt + model route",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="sp">System prompt</Label>
            <Textarea id="sp" rows={4} defaultValue="You are a support agent for Acme…" />
          </div>
          <div className="space-y-1.5">
            <Label>Model route</Label>
            <div className="flex h-9 items-center rounded-md border bg-background px-3 text-sm">
              <span className="font-mono text-xs">claude-sonnet-4</span>
            </div>
          </div>
        </div>
      ),
    },
    {
      id: "tools",
      title: "Tools",
      description: "Bind from the catalog",
      content: <ToolPicker />,
    },
    {
      id: "review",
      title: "Review",
      review: true,
      content: (
        <div className="space-y-4">
          <p className="text-sm font-medium">Review before creating</p>
          <KeyValue
            rows={[
              { k: "Name", v: "support-agent" },
              { k: "Runtime", v: "managed" },
              { k: "Model", v: <span className="font-mono text-xs">claude-sonnet-4</span> },
              { k: "Tools", v: "get_order, search_docs, create_ticket" },
            ]}
          />
          <Note>Same review step as the "Describe it" path — one convergence point, one create action.</Note>
        </div>
      ),
    },
  ];
  const [current, setCurrent] = React.useState(0);
  return (
    <div className="space-y-4">
      <Note>
        The "Configure it" entrance — a multi-step form (basics → behavior →
        tools → review) for users who want full control. It converges on the SAME
        review + tool picker as the prompt path.
      </Note>
      <ScreenFrame>
        <ConsoleChrome active="agents" title="New agent · Configure">
          <div className="mx-auto max-w-3xl rounded-lg border bg-card p-6 shadow-card">
            <Wizard steps={steps} current={current} onStepChange={setCurrent} finishLabel="Create agent" />
          </div>
        </ConsoleChrome>
      </ScreenFrame>
    </div>
  );
}

function ToolPicker() {
  const tools = [
    ["get_order", "curated", true],
    ["search_docs", "curated", true],
    ["create_ticket", "user-added", true],
    ["refund", "pending approval", false],
    ["send_email", "curated", false],
  ] as const;
  return (
    <div className="space-y-3">
      <div className="relative">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
        <Input placeholder="Search tools…" className="pl-9" />
      </div>
      <div className="space-y-2">
        {tools.map(([name, kind, on]) => (
          <label key={name} className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-3 py-2">
            <input type="checkbox" defaultChecked={on} className="accent-primary" />
            <Wrench className="h-4 w-4 text-muted-foreground" />
            <span className="flex-1 font-mono text-sm">{name}</span>
            <Badge
              variant={kind === "pending approval" ? "warning" : "secondary"}
              className="text-[10px]"
            >
              {kind}
            </Badge>
          </label>
        ))}
      </div>
    </div>
  );
}

function SharedReviewScreen() {
  return (
    <ScreenFrame caption="Shared review + tool picker (both entrances converge here)">
      <ConsoleChrome active="agents" title="New agent · Review">
        <div className="mx-auto max-w-2xl space-y-4">
          <div className="rounded-lg border bg-card p-5 shadow-card">
            <p className="mb-3 text-sm font-medium">Attach tools</p>
            <ToolPicker />
          </div>
          <div className="flex items-center justify-between rounded-lg border bg-surface-2/40 px-4 py-3">
            <div className="text-sm">
              <p className="font-medium">support-agent</p>
              <p className="text-xs text-muted-foreground">managed · claude-sonnet-4 · 3 tools</p>
            </div>
            <Button>
              <Check className="h-4 w-4" /> Create agent
            </Button>
          </div>
        </div>
      </ConsoleChrome>
    </ScreenFrame>
  );
}

// Loading-state helper reused if needed (keeps Loader2 import meaningful in the
// generation flow when async).
export function GeneratingRow() {
  return (
    <div className="flex items-center gap-2 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" /> Generating config…
    </div>
  );
}
