import { useEffect, useRef, useState } from "react";
import { AlertTriangle, CheckCircle2, Play, Rocket } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";

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
import { ComboSelect, ForbiddenInline } from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import {
  api,
  ApiError,
  openRunStream,
  type CreatedObject,
} from "@/lib/api";
import { MCP_OAUTH_MESSAGE } from "@/lib/oauth-popup";
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
// Approval is a human-in-the-loop pause (m32.4): the run awaits an approve/deny on `key`.
type Approval = { runId: string; key: string; summary: string };

type Run =
  | { kind: "idle" }
  // running carries the run id (for cancel) + the response accumulated live from token events.
  | { kind: "running"; runId: string; response: string }
  | {
      kind: "done";
      traceId: string;
      response: string;
      consentRequired?: string[];
      approval?: Approval;
    }
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
  // Inline consent (ADR 0031, m26.2): the server currently being connected (its inline
  // "Connect" button shows a spinner) and any begin-consent error.
  const [connecting, setConnecting] = useState<string | null>(null);
  const [connectError, setConnectError] = useState<string | null>(null);
  // RBAC-aware chrome (§3): running an agent and applying a CRD are both
  // create-shaped ops; a viewer's affordances are hidden. DISPLAY-ONLY — the API
  // still enforces, so the 403 paths below stay live even if this is optimistic.
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
  // The active run's SSE stream canceller (m32.8) — aborts on New-run / cancel / unmount.
  const streamCancelRef = useRef<(() => void) | null>(null);

  // Identity pickers + deep link (DX-5): the checklist "Run" step and create→run land here
  // with ?agent=<name>&ns=<namespace>; pre-fill so the user never re-types. The namespace +
  // agent fields are PICKERS over the caller's known set (listNamespaces / listAgents — the
  // console's "finite set → Select" principle) while still allowing a new name (define+export).
  const [searchParams] = useSearchParams();
  const [namespaceOptions, setNamespaceOptions] = useState<string[]>([]);
  const [agentOptions, setAgentOptions] = useState<string[]>([]);
  const didInitRef = useRef(false);

  function set<K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // Deep-link pre-fill + namespace load, ONCE on mount (a ref guards against re-applying the
  // pre-fill over the user's later edits). A 403/failure just leaves the picker empty — the
  // field still accepts a custom value.
  useEffect(() => {
    if (didInitRef.current) return;
    didInitRef.current = true;
    const agentParam = searchParams.get("agent") ?? "";
    const nsParam = searchParams.get("ns") ?? searchParams.get("namespace") ?? "";
    if (agentParam) setForm((f) => ({ ...f, name: agentParam }));
    if (nsParam) setNamespace(nsParam);
    const ctrl = new AbortController();
    api
      .namespaces(ctrl.signal)
      .then((res) => setNamespaceOptions(res.namespaces.map((n) => n.name)))
      .catch(() => {});
    return () => ctrl.abort();
  }, [searchParams]);

  // Load the agents in the selected namespace so the agent field is a picker of what's
  // actually deployed there; re-scopes when the namespace changes (prior request aborted).
  useEffect(() => {
    const ctrl = new AbortController();
    api
      .listAgents({ namespace: namespace.trim() || undefined, limit: 100 }, ctrl.signal)
      .then((res) => setAgentOptions(res.agents.map((a) => a.name)))
      .catch(() => setAgentOptions([]));
    return () => ctrl.abort();
  }, [namespace]);

  // Run invokes the DEFINED agent by name. The agent must already be deployed
  // (the run resolves its endpoint server-side); the Playground's define form is
  // the same identity used for export. Input is parsed as JSON so the browser
  // sends a real object (a malformed input is caught before the round-trip).
  async function onRun() {
    // Run invokes an EXISTING, already-deployed agent BY NAME — so it needs only a valid
    // agent name (+ valid JSON input below). Image / scaling / budget are define/export-time
    // fields validated by "Export to CRD"; requiring them here wrongly BLOCKED invoking a
    // live agent (the run silently never fired → no response, no consent CTA). Validate the
    // name only for a run.
    const nameError = validate(form).name;
    if (nameError) {
      setErrors({ name: nameError });
      setRun({ kind: "error", message: nameError });
      return;
    }
    setErrors({});
    let parsedInput: unknown;
    try {
      parsedInput = input.trim() ? JSON.parse(input) : {};
    } catch {
      setRun({ kind: "error", message: "Input must be valid JSON." });
      return;
    }
    // Stream the run (ADR 0034, m32.8): create it, then consume its SSE event stream —
    // tokens render live; a requires_action pause (consent/approval) or a terminal close
    // is finalized by reading the structured run state.
    let runId: string;
    try {
      runId = (
        await api.createRun({
          agent: form.name.trim(),
          namespace: namespace.trim(),
          input: parsedInput,
        })
      ).id;
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
      return;
    }

    setRun({ kind: "running", runId, response: "" });
    let acc = "";
    let finalized = false;
    const finalize = () => {
      if (finalized) return;
      finalized = true;
      stopStream();
      void finalizeRun(runId, acc);
    };
    const cancelStream = openRunStream(runId, {
      onEvent: (kind, data) => {
        if (kind === "token") {
          acc += data;
          setRun({ kind: "running", runId, response: acc });
        } else if (kind === "message") {
          acc = data;
          setRun({ kind: "running", runId, response: acc });
        } else if (kind === "state" && data === "requires_action") {
          // requires_action is NOT terminal, so the stream stays open — stop it and finalize.
          finalize();
        }
      },
      onClose: finalize,
      onError: (message, status) => {
        finalized = true;
        setRun({ kind: "error", message, status });
      },
      onForbidden: (message) => {
        finalized = true;
        reprobe();
        setRun({ kind: "error", message, forbidden: true });
      },
    });
    streamCancelRef.current = cancelStream;
  }

  // finalizeRun reads the structured run state after the stream ends (or pauses at
  // requires_action) and renders the outcome — the SSE stream carries tokens but not the
  // traceId / requiresAction, which live on the run object.
  async function finalizeRun(runId: string, streamed: string) {
    try {
      const detail = await api.getRun(runId);
      const ra = detail.requiresAction;
      const lastMessage = detail.messages?.length
        ? detail.messages[detail.messages.length - 1].content
        : streamed;
      if (detail.status === "failed") {
        setRun({ kind: "error", message: detail.error || "The run failed." });
        return;
      }
      setRun({
        kind: "done",
        traceId: detail.traceId ?? "",
        response: lastMessage,
        consentRequired: ra?.kind === "consent_required" ? ra.servers : undefined,
        approval:
          ra?.kind === "approval"
            ? { runId, key: ra.key ?? "", summary: ra.message ?? "" }
            : undefined,
      });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
    }
  }

  // onApprove / onDeny resolve a human-in-the-loop pause (m32.4): approve re-invokes (the run
  // resumes and streams to completion), deny cancels it. Approve re-streams from the same run.
  async function onApprove(runId: string) {
    try {
      await api.resumeRun(runId, "approve");
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
      return;
    }
    setRun({ kind: "running", runId, response: "" });
    let acc = "";
    let finalized = false;
    const finalize = () => {
      if (finalized) return;
      finalized = true;
      stopStream();
      void finalizeRun(runId, acc);
    };
    const cancelStream = openRunStream(runId, {
      onEvent: (kind, data) => {
        if (kind === "token") {
          acc += data;
          setRun({ kind: "running", runId, response: acc });
        } else if (kind === "message") {
          acc = data;
        } else if (kind === "state" && data === "requires_action") {
          finalize();
        }
      },
      onClose: finalize,
      onError: (message, status) => {
        finalized = true;
        setRun({ kind: "error", message, status });
      },
    });
    streamCancelRef.current = cancelStream;
  }

  async function onDeny(runId: string) {
    try {
      await api.resumeRun(runId, "deny");
      setRun({ kind: "done", traceId: "", response: "Approval denied — run cancelled." });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
    }
  }

  // onCancel stops a streaming run: cancel the SSE, then cancel the run server-side.
  async function onCancel(runId: string) {
    stopStream();
    try {
      await api.cancelRun(runId);
    } catch {
      // best-effort — the run may already be terminal; the UI resets regardless.
    }
    setRun({ kind: "idle" });
  }

  function stopStream() {
    streamCancelRef.current?.();
    streamCancelRef.current = null;
  }

  // onConnect runs the INLINE per-user consent (ADR 0031, m26.2): begin the OAuth grant
  // for the named server (the BFF recovers the OAuth config, so we send only the server),
  // open the provider consent in a POPUP so this run stays on screen, then — when the
  // popup messages back (the auto-close bridge) or is closed — RE-RUN the same invoke so
  // the freshly-connected credential is injected. The token never touches the SPA; the
  // exchange is entirely server-side. Popup blocked → full-page redirect fallback.
  async function onConnect(server: string) {
    setConnectError(null);
    setConnecting(server);
    let authorizationURL: string;
    try {
      const res = await api.beginMcpGrant({
        server,
        namespace: namespace.trim(),
        // The console's OAuth callback — needed for a legacy server the BFF must re-discover.
        redirectUri: `${window.location.origin}/api/mcp/oauth/callback`,
      });
      authorizationURL = res.authorizationURL;
    } catch (err) {
      setConnecting(null);
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setConnectError(err instanceof Error ? err.message : "Couldn't start the connect flow.");
      return;
    }

    const popup = window.open(
      authorizationURL,
      "ctxmesh-oauth-connect",
      "width=520,height=680,menubar=no,toolbar=no",
    );
    if (!popup) {
      // Popup blocked → fall back to a full-page redirect. The run state is lost, but
      // the connect completes and the user re-runs on return.
      window.location.href = authorizationURL;
      return;
    }

    // Resume when the popup reports back (message) or is closed; re-invoke once.
    let done = false;
    let poll = 0;
    function finish() {
      if (done) return;
      done = true;
      window.removeEventListener("message", onMessage);
      window.clearInterval(poll);
      setConnecting(null);
      void onRun(); // re-invoke in place — the resume
    }
    function onMessage(e: MessageEvent) {
      if (e.origin !== window.location.origin) return;
      const data = e.data as { type?: string } | null;
      if (data?.type === MCP_OAUTH_MESSAGE) finish();
    }
    window.addEventListener("message", onMessage);
    poll = window.setInterval(() => {
      if (popup.closed) finish();
    }, 700);
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
            <FormField
              id="namespace"
              label="Namespace"
              hint="Pick a namespace you can access — empty → the default namespace."
            >
              <ComboSelect
                id="namespace"
                value={namespace}
                options={namespaceOptions}
                onChange={setNamespace}
                placeholder="default namespace"
                customPlaceholder="namespace"
                testId="playground-namespace"
              />
            </FormField>

            <FormField
              id="name"
              label="Agent"
              error={errors.name}
              hint="Pick a deployed agent to run, or name a new one to define + export (≤ 44 chars)."
            >
              <ComboSelect
                id="name"
                value={form.name}
                options={agentOptions}
                onChange={(v) => set("name", v)}
                placeholder="— pick an agent —"
                customPlaceholder="new agent name"
                testId="playground-agent"
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
                      <div className="space-y-2">
                        <p className="font-medium">Connect your account to continue</p>
                        <p className="text-muted-foreground">
                          This run needs your own credentials. Connect, and it re-runs
                          automatically.
                        </p>
                        <div className="flex flex-wrap gap-2">
                          {run.consentRequired.map((server) => (
                            <Button
                              key={server}
                              size="sm"
                              variant="outline"
                              disabled={connecting !== null}
                              onClick={() => void onConnect(server)}
                              data-testid={`connect-${server}`}
                            >
                              {connecting === server ? "Connecting…" : `Connect ${server}`}
                            </Button>
                          ))}
                        </div>
                        {connectError && (
                          <p className="text-destructive" data-testid="connect-error">
                            {connectError}
                          </p>
                        )}
                      </div>
                    </div>
                  )}
                  {run.approval && (
                    <div
                      className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm"
                      data-testid="approval-request"
                    >
                      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
                      <div className="space-y-2">
                        <p className="font-medium">Approval needed to continue</p>
                        <p className="text-muted-foreground">{run.approval.summary}</p>
                        <div className="flex flex-wrap gap-2">
                          <Button
                            size="sm"
                            disabled={running}
                            onClick={() => run.approval && void onApprove(run.approval.runId)}
                            data-testid="approve-run"
                          >
                            Approve
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={running}
                            onClick={() => run.approval && void onDeny(run.approval.runId)}
                            data-testid="deny-run"
                          >
                            Deny
                          </Button>
                        </div>
                      </div>
                    </div>
                  )}
                  {run.traceId && (
                    <div className="rounded-md border p-3">
                      <p className="text-xs text-muted-foreground">trace id</p>
                      <p className="truncate font-mono text-xs" data-testid="trace-id">
                        {run.traceId}
                      </p>
                    </div>
                  )}
                  <Textarea
                    aria-label="Agent response"
                    readOnly
                    className="min-h-[8rem] font-mono text-xs"
                    value={run.response}
                  />
                </>
              ) : run.kind === "running" ? (
                <div className="space-y-3">
                  <div className="flex items-center justify-between">
                    <span className="text-sm text-muted-foreground" data-testid="run-streaming">
                      Streaming…
                    </span>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => void onCancel(run.runId)}
                      data-testid="cancel-run"
                    >
                      Cancel
                    </Button>
                  </div>
                  <Textarea
                    aria-label="Agent response"
                    readOnly
                    className="min-h-[8rem] font-mono text-xs"
                    value={run.response}
                  />
                </div>
              ) : (
                <p className="py-6 text-center text-sm text-muted-foreground">
                  Run the agent to see its response and trace.
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
