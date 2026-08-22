import { useEffect, useRef, useState } from "react";
import { AlertTriangle, CheckCircle2, GitFork, Play, Rocket } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";

import { Badge, type BadgeProps } from "@/components/ui/badge";
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
  type CreatedObject,
  type RunDetail,
  type WorkflowNodeStatus,
} from "@/lib/api";
import { useDurableRun } from "@/lib/use-durable-run";
import {
  isValidHttpUrl,
  MCP_OAUTH_MESSAGE,
  type McpOAuthPopupMessage,
  readMcpOAuthReturn,
} from "@/lib/oauth-popup";
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
  // running carries the run id (for cancel) + the response accumulated live from token events +
  // the latest live step label (M78: "what step is my agent on right now"), empty until the first
  // `step` event.
  | { kind: "running"; runId: string; response: string; step?: string }
  | {
      kind: "done";
      traceId: string;
      response: string;
      runStatus: string;
      consentRequired?: string[];
      approval?: Approval;
      // Workflow instance fields (m67.9): present when the run is a workflow instance.
      workflowRef?: string;
      currentNode?: string;
      nodes?: WorkflowNodeStatus[];
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
  // The active inline-consent wait's teardown (OTH-2): onConnect registers a window `message`
  // listener + a popup-close poll interval; this holds their cleanup so the unmount effect can
  // clear them even if the user navigates away with the popup still open (they'd leak otherwise).
  const connectCleanupRef = useRef<(() => void) | null>(null);

  // The durable run engine (ADR 0093) — the SAME create-or-resume → stream → finalize engine the
  // console ChatPanel uses (this page previously duplicated it in onRun + onApprove). It owns the
  // SSE stream lifecycle (aborted on New-run / cancel / unmount); the callbacks project its
  // live/finalized state into this page's richer `Run` union (traceId, consent/approval,
  // workflow nodes) so the define/run/export chrome is unchanged.
  const runEngine = useDurableRun({
    onForbidden: () => reprobe(),
    onFinalized: (detail) => setRun(finalizedRun(detail)),
    onFinalizeError: (err) => setRun(errorRun(err)),
  });

  // Mirror the durable engine's LIVE stream state (running + accumulated tokens + step label)
  // into the `Run` union so the "Streaming…" panel + Cancel keep working exactly as before.
  useEffect(() => {
    if (runEngine.status === "streaming" || runEngine.status === "creating") {
      setRun({
        kind: "running",
        runId: runEngine.runId ?? "",
        response: runEngine.responseText,
        step: runEngine.step || undefined,
      });
    } else if (runEngine.status === "error") {
      setRun({ kind: "error", message: runEngine.error ?? "The run failed." });
    } else if (runEngine.status === "forbidden") {
      setRun({ kind: "error", message: runEngine.error ?? "Forbidden.", forbidden: true });
    }
    // Only react to the durable-run transitions; `done` is handled by onFinalized.
  }, [runEngine.status, runEngine.responseText, runEngine.step, runEngine.error, runEngine.runId]);

  // Identity pickers + deep link (DX-5): the checklist "Run" step and create→run land here
  // with ?agent=<name>&ns=<namespace>; pre-fill so the user never re-types. The namespace +
  // agent fields are PICKERS over the caller's known set (listNamespaces / listAgents — the
  // console's "finite set → Select" principle) while still allowing a new name (define+export).
  const [searchParams] = useSearchParams();
  const [namespaceOptions, setNamespaceOptions] = useState<string[]>([]);
  const [agentOptions, setAgentOptions] = useState<string[]>([]);
  const didInitRef = useRef(false);
  // A same-tab (popup-blocked) MCP OAuth return the boot handler stashed (DX-6) — surfaced as
  // a notice so consent-on-a-blocked-popup no longer ends in silence.
  const [oauthReturn, setOauthReturn] = useState<McpOAuthPopupMessage | null>(null);

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
    // Surface a same-tab (popup-blocked) MCP OAuth outcome the boot handler stashed (DX-6).
    setOauthReturn(readMcpOAuthReturn());
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

  // Unmount cleanup (OTH-2): tear down any active inline-consent wait (the `message` listener +
  // popup-close poll). The durable run engine owns aborting its own in-flight SSE stream on
  // unmount, so we only clear the consent wait here.
  useEffect(() => {
    return () => {
      connectCleanupRef.current?.();
    };
  }, []);

  // Run invokes the DEFINED agent by name via the shared durable run engine (ADR 0093 — the
  // SAME create → stream → finalize the console chat uses). The agent must already be deployed
  // (the run resolves its endpoint server-side); the Playground's define form is the same
  // identity used for export. Input is parsed as JSON so the browser sends a real object (a
  // malformed input is caught before the round-trip). The engine streams tokens + finalizes;
  // this page projects the finalized detail via onFinalized → finalizedRun().
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
    await runEngine.start({
      agent: form.name.trim(),
      namespace: namespace.trim(),
      input: parsedInput,
    });
  }

  // onApprove / onDeny resolve a human-in-the-loop pause (m32.4). Approve RESUMES the same run
  // (the engine re-streams to completion). Deny resolves it to a terminal "denied" state
  // directly (no re-stream). Both go through POST /api/runs/{id}/resume.
  async function onApprove(_runId: string) {
    // The engine resumes the run it is currently holding (the paused run == _runId).
    await runEngine.resume("approve");
  }

  async function onDeny(runId: string) {
    try {
      await api.resumeRun(runId, "deny");
      setRun({ kind: "done", traceId: "", response: "Approval denied — run cancelled.", runStatus: "cancelled" });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
    }
  }

  // onCancel stops a streaming run: the engine cancels its SSE stream + the run server-side,
  // then resets to idle.
  async function onCancel(_runId: string) {
    await runEngine.cancel();
    setRun({ kind: "idle" });
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
      // Popup blocked → fall back to a full-page SAME-TAB redirect. Validate the URL first
      // (DX-6) so a relative/`javascript:` value can't hijack the tab. The run state is lost,
      // but the connect completes and the boot handler surfaces the outcome on return so the
      // user knows to re-run. An invalid URL surfaces inline instead of a silent no-op.
      if (isValidHttpUrl(authorizationURL)) {
        window.location.href = authorizationURL;
      } else {
        setConnecting(null);
        setConnectError("the authorization URL returned by the server was invalid");
      }
      return;
    }

    // Resume when the popup reports back (message) or is closed; re-invoke once.
    let done = false;
    let poll = 0;
    function teardown() {
      window.removeEventListener("message", onMessage);
      window.clearInterval(poll);
      connectCleanupRef.current = null;
    }
    function finish() {
      if (done) return;
      done = true;
      teardown();
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
    // Expose the teardown so the unmount effect can clear the listener + poll (OTH-2) if the
    // user leaves while the consent popup is still open — otherwise both leak past this page.
    connectCleanupRef.current = teardown;
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
            {oauthReturn && (
              <div
                data-testid="mcp-oauth-return"
                className={`rounded-md border p-3 text-xs ${
                  oauthReturn.error
                    ? "border-destructive/40 text-destructive"
                    : "border-success/40 text-success"
                }`}
              >
                {oauthReturn.error
                  ? `Couldn't connect ${oauthReturn.server || "the server"}: ${oauthReturn.error}`
                  : `Connected ${oauthReturn.server || "your account"} — run again to continue.`}
                <button
                  type="button"
                  className="ml-2 underline"
                  onClick={() => setOauthReturn(null)}
                >
                  dismiss
                </button>
              </div>
            )}

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
                    <span className="text-sm font-medium">
                      {run.runStatus === "waiting" ? "Suspended — awaiting next node" : "Traced run complete"}
                    </span>
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
                        {/* m75.3: this is the deep-link target of the HITL approval-waiting
                            notification (ADR 0069 §3). The approver arrives here (caller-scoped —
                            NOT via a magic link) and approves/denies via /api/runs/{id}/resume. */}
                        <p className="font-medium">Waiting for your approval</p>
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
                  {run.nodes && run.nodes.length > 0 && (
                    <WorkflowGraphSection
                      nodes={run.nodes}
                      currentNode={run.currentNode}
                      runStatus={run.runStatus}
                    />
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
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex min-w-0 items-center gap-2">
                      <span className="text-sm text-muted-foreground" data-testid="run-streaming">
                        Streaming…
                      </span>
                      {/* Live step-visibility (M78, ADR 0071 §4): what step is the agent on now. */}
                      {run.step && (
                        <Badge
                          variant="secondary"
                          className="truncate font-mono text-[10px]"
                          data-testid="run-step"
                        >
                          {run.step}
                        </Badge>
                      )}
                    </div>
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

// WorkflowGraphSection renders the per-node status panel for a workflow instance run (m67.15).
// It mirrors the RuntimeSection / guardrail-section idiom on agent-detail-page.tsx.
//
// When the run status is "waiting", the overall label reads "Suspended — awaiting next node"
// (surfaced in the run-done banner above). The current node is highlighted.
// A failed node shows a destructive badge; a running node shows an info badge; pending = muted.
type NodeBadgeVariant = BadgeProps["variant"];
const NODE_STATUS_VARIANT: Record<WorkflowNodeStatus["status"], NodeBadgeVariant> = {
  pending: "secondary",
  running: "secondary",
  done: "success",
  failed: "destructive",
};
const NODE_STATUS_LABEL: Record<WorkflowNodeStatus["status"], string> = {
  pending: "pending",
  running: "running",
  done: "done",
  failed: "failed",
};

function WorkflowGraphSection({
  nodes,
  currentNode,
  runStatus,
}: {
  nodes: WorkflowNodeStatus[];
  currentNode?: string;
  runStatus: string;
}) {
  return (
    <div
      className="rounded-lg border bg-card p-4 shadow-card"
      data-testid="workflow-graph-section"
    >
      <div className="mb-3 flex items-center gap-2">
        <GitFork className="h-4 w-4 text-muted-foreground" />
        <p className="text-sm font-medium">Workflow</p>
        {runStatus === "waiting" && (
          <Badge variant="warning" className="text-[10px]" data-testid="workflow-suspended-badge">
            Suspended — awaiting next node
          </Badge>
        )}
      </div>
      <ol className="space-y-2" data-testid="workflow-node-list">
        {nodes.map((node) => {
          const isCurrent = node.name === currentNode;
          return (
            <li
              key={node.name}
              data-testid={`workflow-node-${node.name}`}
              className={`flex items-center justify-between gap-3 rounded-md border px-3 py-2 text-sm ${
                isCurrent ? "border-primary/40 bg-primary/5" : "bg-surface-2/40"
              }`}
            >
              <div className="flex min-w-0 items-center gap-2">
                {isCurrent && (
                  <span
                    className="h-2 w-2 shrink-0 rounded-full bg-primary"
                    aria-label="current node"
                    data-testid={`workflow-node-current-${node.name}`}
                  />
                )}
                <span className={`truncate font-medium ${isCurrent ? "" : "text-muted-foreground"}`}>
                  {node.name}
                </span>
                {node.agent && (
                  <span className="truncate font-mono text-xs text-muted-foreground">
                    {node.agent}
                  </span>
                )}
              </div>
              <div className="flex shrink-0 items-center gap-2">
                <Badge
                  variant={NODE_STATUS_VARIANT[node.status]}
                  className="text-[10px]"
                  data-testid={`workflow-node-status-${node.name}`}
                >
                  {NODE_STATUS_LABEL[node.status]}
                </Badge>
                {node.childRunId && (
                  <Link
                    to={`/traces/${encodeURIComponent(node.childRunId)}`}
                    data-testid={`workflow-node-run-link-${node.name}`}
                    className="truncate font-mono text-[10px] text-primary hover:underline"
                    onClick={(e) => e.stopPropagation()}
                  >
                    {node.childRunId.slice(0, 8)}…
                  </Link>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

// finalizedRun projects a finalized RunDetail (from the shared durable engine, ADR 0093) into
// this page's richer `Run` union — the traceId + response + consent/approval affordances +
// workflow nodes the "Run result" panel renders. A failed run maps to the error state (no trace
// panel). This is exactly the projection the page's old inline finalizeRun did, now fed by the
// hook's onFinalized callback.
function finalizedRun(detail: RunDetail): Run {
  if (detail.status === "failed") {
    return { kind: "error", message: detail.error || "The run failed." };
  }
  const ra = detail.requiresAction;
  const lastMessage = detail.messages?.length
    ? detail.messages[detail.messages.length - 1].content
    : "";
  // Derive failed node: when the run failed, the node at currentNode is the failed one. The BFF
  // exposes nodes with their stored status; we overlay "failed" for the current node when the run
  // itself is failed (UI-side derivation per m67.15).
  const nodes = detail.nodes?.map((n) => {
    if (detail.status === "failed" && n.name === detail.currentNode && n.status !== "done") {
      return { ...n, status: "failed" as const };
    }
    return n;
  });
  return {
    kind: "done",
    traceId: detail.traceId ?? "",
    response: lastMessage,
    runStatus: detail.status,
    consentRequired: ra?.kind === "consent_required" ? ra.servers : undefined,
    approval:
      ra?.kind === "approval"
        ? { runId: detail.id, key: ra.key ?? "", summary: ra.message ?? "" }
        : undefined,
    workflowRef: detail.workflowRef,
    currentNode: detail.currentNode,
    nodes,
  };
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
