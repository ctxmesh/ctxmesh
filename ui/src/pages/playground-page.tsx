import { useState } from "react";
import { AlertTriangle, CheckCircle2, Play, Rocket } from "lucide-react";
import { Link } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { ForbiddenInline } from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { api, ApiError, type CreatedObject } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";
import {
  emptyForm,
  toAgentYAML,
  validate,
  type ConfigForm,
  type FieldErrors,
} from "@/lib/config-form";

// PlaygroundPage — the m12.7 surface: define + RUN a fully-traced agent, then see
// its response and navigate to the native trace explorer, and EXPORT the same
// definition to a CRD. It reuses the config-builder's define model (toAgentYAML +
// validate) so define/run/export share ONE agent schema. Every control composes
// design tokens. The post-run trace affordance is a "View full trace" Link to
// /traces/:id (native trace page, m16.7) — no embedded Langfuse iframe (m17.13).

// Run is the lifecycle of a Playground invoke. On success we hold the returned
// traceId — the hand-off we link to the native /traces/:id (m16.7, m17.13).
type Run =
  | { kind: "idle" }
  | { kind: "running" }
  | { kind: "done"; traceId: string; response: string; consentRequired?: string[] }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

// Export is the export-to-CRD lifecycle: expand (preview) → apply. It reuses the
// config-builder's expand/apply path verbatim (same server-side mapping as the
// CLI); no raw YAML is hand-edited.
type Export =
  | { kind: "idle" }
  | { kind: "previewing" }
  | { kind: "preview"; yaml: string; manifest: string }
  | { kind: "applying"; yaml: string; manifest: string }
  | { kind: "applied"; created: CreatedObject[] }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

export function PlaygroundPage() {
  const [form, setForm] = useState<ConfigForm>(emptyForm);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [namespace, setNamespace] = useState("");
  const [input, setInput] = useState('{\n  "input": "Hello, agent"\n}');
  const [run, setRun] = useState<Run>({ kind: "idle" });
  const [exp, setExp] = useState<Export>({ kind: "idle" });
  // RBAC-aware chrome (§3): running an agent and applying a CRD are both
  // create-shaped ops; a viewer's affordances are hidden. DISPLAY-ONLY — the API
  // still enforces, so the 403 paths below stay live even if this is optimistic.
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");

  function set<K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // Run invokes the DEFINED agent by name. The agent must already be deployed
  // (the run resolves its endpoint server-side); the Playground's define form is
  // the same identity used for export. Input is parsed as JSON so the browser
  // sends a real object (a malformed input is caught before the round-trip).
  async function onRun() {
    const found = validate(form);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      setRun({ kind: "error", message: "Fix the highlighted fields before running." });
      return;
    }
    let parsedInput: unknown;
    try {
      parsedInput = input.trim() ? JSON.parse(input) : {};
    } catch {
      setRun({ kind: "error", message: "Input must be valid JSON." });
      return;
    }
    setRun({ kind: "running" });
    try {
      const res = await api.invoke({
        agent: form.name.trim(),
        namespace: namespace.trim(),
        input: parsedInput,
      });
      setRun({
        kind: "done",
        traceId: res.traceId,
        response: res.response,
        consentRequired: res.consentRequired,
      });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
    }
  }

  // Export-to-CRD: preview the expanded CRD (POST /api/expand), then apply it
  // (POST /api/agents) — the SAME caller-scoped path the config-builder uses.
  async function onPreview() {
    const found = validate(form);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      setExp({ kind: "error", message: "Fix the highlighted fields before exporting." });
      return;
    }
    const yaml = toAgentYAML(form);
    setExp({ kind: "previewing" });
    try {
      const manifest = await api.expand(yaml);
      setExp({ kind: "preview", yaml, manifest });
    } catch (err) {
      setExp(errorExport(err));
    }
  }

  async function onApply() {
    if (exp.kind !== "preview") return;
    const { yaml, manifest } = exp;
    setExp({ kind: "applying", yaml, manifest });
    try {
      const res = await api.createAgent(yaml, namespace.trim());
      setExp({ kind: "applied", created: res.created });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setExp(errorExport(err));
    }
  }

  const running = run.kind === "running";
  const exporting = exp.kind === "previewing" || exp.kind === "applying";

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Playground</h2>
        <p className="text-sm text-muted-foreground">
          Define an agent, run it fully traced, and inspect the trace — then export
          the same definition to a CRD. No raw YAML.
        </p>
      </div>

      <div className="grid gap-6 lg:grid-cols-[1fr_1fr]">
        {/* ── Define + run ─────────────────────────────────────────────── */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Define &amp; run</CardTitle>
            <CardDescription>
              A minimal agent definition + an input to invoke. The agent must be
              deployed; the run is traced end to end.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-5">
            <FormField id="name" label="Agent name" error={errors.name} hint="DNS label, ≤ 44 chars.">
              <Input
                id="name"
                value={form.name}
                onChange={(e) => set("name", e.target.value)}
                placeholder="echo-agent"
              />
            </FormField>

            <FormField id="namespace" label="Namespace" hint="Empty → the default namespace.">
              <Input
                id="namespace"
                value={namespace}
                onChange={(e) => setNamespace(e.target.value)}
                placeholder="default"
              />
            </FormField>

            <FormField id="image" label="Image" error={errors.image} hint="Used when exporting to a CRD.">
              <Input
                id="image"
                value={form.image}
                onChange={(e) => set("image", e.target.value)}
                placeholder="ghcr.io/ctxmesh/echo:v1"
              />
            </FormField>

            <FormField id="executionModel" label="Execution model">
              <Select
                id="executionModel"
                value={form.executionModel}
                onChange={(e) =>
                  set("executionModel", e.target.value as ConfigForm["executionModel"])
                }
              >
                <option value="serving">serving (request-driven)</option>
                <option value="eventing">eventing (broker-triggered)</option>
                <option value="job">job (one-shot)</option>
              </Select>
            </FormField>

            <FormField id="modelRoute" label="Model route" hint="ModelRoute alias for the agent's LLM calls (optional).">
              <Input
                id="modelRoute"
                value={form.modelRoute}
                onChange={(e) => set("modelRoute", e.target.value)}
                placeholder="default-model"
              />
            </FormField>

            <FormField id="input" label="Input (JSON)" hint="Posted verbatim to the agent's /invoke.">
              <Textarea
                id="input"
                className="min-h-[8rem] font-mono text-xs"
                value={input}
                onChange={(e) => setInput(e.target.value)}
              />
            </FormField>

            <div className="space-y-3 pt-1">
              <div className="flex items-center gap-3">
                {canRun ? (
                  <Button onClick={onRun} disabled={running}>
                    <Play className="h-4 w-4" />
                    {running ? "Running…" : "Run agent"}
                  </Button>
                ) : (
                  // RBAC-aware chrome: a viewer has no Run affordance (running is
                  // a write-shaped op). They can still Preview/Export below.
                  <p
                    className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-xs text-muted-foreground"
                    data-testid="run-readonly-note"
                  >
                    You have read-only access — running an agent requires create
                    permission on AgentDeployments.
                  </p>
                )}
                {run.kind === "error" && !run.forbidden && (
                  <p className="text-sm text-destructive" role="alert">
                    {run.message}
                    {run.status ? ` (${run.status})` : ""}
                  </p>
                )}
              </div>
              {run.kind === "error" && run.forbidden && (
                // A surface-level 403 → the explain-and-suggest 403 primitive.
                <ForbiddenInline
                  title="Not allowed to run this agent"
                  description="Your account can't invoke agents in this cluster."
                  detail={run.message}
                />
              )}
            </div>
          </CardContent>
        </Card>

        {/* ── Run result + native trace link ────────────────────────────── */}
        <div className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Run result</CardTitle>
              <CardDescription>
                The agent's response. Use "View full trace" for the native
                waterfall and span explorer.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {run.kind === "done" ? (
                <>
                  <div className="flex items-center gap-2 text-success">
                    <CheckCircle2 className="h-5 w-5" />
                    <span className="text-sm font-medium">Traced run complete</span>
                  </div>
                  {run.consentRequired && run.consentRequired.length > 0 && (
                    <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
                      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
                      <div className="space-y-1">
                        <p className="font-medium">Connect your account to continue</p>
                        <p className="text-muted-foreground">
                          This run needs your own credentials for{" "}
                          <span className="font-mono">{run.consentRequired.join(", ")}</span>.
                          Connect your account on the{" "}
                          <Link to="/tools/mcp-servers" className="underline">
                            MCP Servers
                          </Link>{" "}
                          page, then run again.
                        </p>
                      </div>
                    </div>
                  )}
                  <div className="rounded-md border p-3">
                    <p className="text-xs text-muted-foreground">trace id</p>
                    <p className="truncate font-mono text-xs" data-testid="trace-id">
                      {run.traceId}
                    </p>
                  </div>
                  <Textarea
                    aria-label="Agent response"
                    readOnly
                    className="min-h-[8rem] font-mono text-xs"
                    value={run.response}
                  />
                </>
              ) : (
                <p className="py-6 text-center text-sm text-muted-foreground">
                  {running
                    ? "Invoking the agent…"
                    : "Run the agent to see its response and trace."}
                </p>
              )}
            </CardContent>
          </Card>

          {/* Post-run: link to the native trace explorer (/traces/:id, m16.7).
              No embedded Langfuse iframe — link-out only (m17.13). */}
          {run.kind === "done" && (
            <div className="flex justify-start">
              <Link
                to={`/traces/${encodeURIComponent(run.traceId)}`}
                data-testid="view-full-trace"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                View full trace
              </Link>
            </div>
          )}
        </div>
      </div>

      {/* ── Export to CRD ──────────────────────────────────────────────── */}
      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="text-base">Export to CRD</CardTitle>
            <CardDescription>
              Take this definition to a CRD — the same server-side expand + apply
              as the config builder. Preview, then apply (client-go, RBAC-scoped).
            </CardDescription>
          </div>
          {(exp.kind === "preview" || exp.kind === "applying") && (
            <Badge variant="secondary">preview</Badge>
          )}
        </CardHeader>
        <CardContent className="space-y-4">
          {exp.kind === "applied" ? (
            <div className="space-y-3">
              <div className="flex items-center gap-2 text-success">
                <CheckCircle2 className="h-5 w-5" />
                <span className="text-sm font-medium">Applied to the cluster</span>
              </div>
              <div className="grid gap-2 sm:grid-cols-2">
                {exp.created.map((obj) => (
                  <div
                    key={`${obj.kind}/${obj.namespace}/${obj.name}`}
                    className="flex items-center justify-between rounded-md border p-3"
                  >
                    <div>
                      <p className="text-sm font-medium">{obj.name}</p>
                      <p className="text-xs text-muted-foreground">{obj.namespace}</p>
                    </div>
                    <Badge variant="success">{obj.kind}</Badge>
                  </div>
                ))}
              </div>
              <Button variant="outline" onClick={() => setExp({ kind: "idle" })}>
                Export again
              </Button>
            </div>
          ) : (
            <>
              <Textarea
                aria-label="Exported CRD preview"
                readOnly
                className="min-h-[16rem] font-mono text-xs"
                value={
                  exp.kind === "preview" || exp.kind === "applying"
                    ? exp.manifest
                    : ""
                }
                placeholder="Press “Preview CRD” to see the generated manifest."
              />
              {exp.kind === "error" && exp.forbidden ? (
                <ForbiddenInline
                  title="Not allowed to apply"
                  description="Your account can preview the manifest but can't create AgentDeployments in this cluster."
                  detail={exp.message}
                />
              ) : (
                exp.kind === "error" && (
                  <p className="text-sm text-destructive" role="alert">
                    {exp.message}
                    {exp.status ? ` (${exp.status})` : ""}
                  </p>
                )
              )}
              <div className="flex items-center gap-3">
                <Button onClick={onPreview} disabled={exporting} variant="outline">
                  {exp.kind === "previewing" ? "Expanding…" : "Preview CRD"}
                </Button>
                {/* Apply is a WRITE affordance — hidden for a viewer (Preview
                    stays available so they can still inspect the CRD). */}
                {canRun && (
                  <Button onClick={onApply} disabled={exp.kind !== "preview"}>
                    <Rocket className="h-4 w-4" />
                    {exp.kind === "applying" ? "Applying…" : "Apply to cluster"}
                  </Button>
                )}
              </div>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// errorRun / errorExport map a thrown error to the respective error state,
// preserving the HTTP status when it is an ApiError so the user sees the real
// reason (e.g. a 403 RBAC denial, a 409 not-ready).
function errorRun(err: unknown): Run {
  if (err instanceof ApiError) {
    return { kind: "error", message: err.message, status: err.status, forbidden: err.isForbidden };
  }
  return { kind: "error", message: err instanceof Error ? err.message : "request failed" };
}

function errorExport(err: unknown): Export {
  if (err instanceof ApiError) {
    return { kind: "error", message: err.message, status: err.status, forbidden: err.isForbidden };
  }
  return { kind: "error", message: err instanceof Error ? err.message : "request failed" };
}
