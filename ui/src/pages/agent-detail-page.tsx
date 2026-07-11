import * as React from "react";
import { useParams } from "react-router-dom";
import {
  Boxes,
  CheckCircle2,
  ExternalLink,
  Play,
  Terminal,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  DataTable,
  type Column,
  DetailDrawer,
  EmptyState,
  ForbiddenInline,
} from "@/components/kit";
import { RunInspector } from "@/components/dashboard/run-inspector";
import {
  api,
  ApiError,
  openLogStream,
  type AgentBinding,
  type AgentCondition,
  type AgentDetailResponse,
  type LogEventType,
  type RunSummary,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";

// AgentDetailPage — the agent LANDING page (first-agent-flow.md §5, m14.11). It
// closes the aha loop: watch the agent come alive (status timeline + live log
// tail) → run it (the Run panel) → see the trace with the tool span (the native
// run inspector). It reads GET /api/agents/{ns}/{name} (m14.7) for the detail,
// tails GET .../logs over SSE with the caller's bearer attached (fetch-stream,
// api.openLogStream), invokes via POST /api/invoke (m12.7), and opens the run
// inspector on the returned traceId (GET /api/traces/{id}/detail, m14.8).
//
// RBAC-aware chrome (§3, display-only): Run gates on agentdeployments.create; a
// viewer sees an explain-note, not a button. A 404 → not-found, a 403 →
// ForbiddenInline — the API server is the real gate (ADR 0011).

const TABS = ["Overview", "Logs", "Runs", "Bindings"] as const;
type Tab = (typeof TABS)[number];

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: AgentDetailResponse }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function AgentDetailPage() {
  const { ns = "", name = "" } = useParams();
  const [state, setState] = React.useState<Load>({ kind: "loading" });
  const [tab, setTab] = React.useState<Tab>("Overview");
  // The trace to inspect — set when a run returns a traceId; opens the inspector
  // drawer over the page (list context preserved).
  const [inspectTrace, setInspectTrace] = React.useState<string | null>(null);

  const load = React.useCallback(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .agentDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the agent",
          status: apiErr?.status,
          forbidden: apiErr?.isForbidden ?? false,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  React.useEffect(() => load(), [load]);

  if (state.kind === "loading") {
    return (
      <div className="mx-auto max-w-5xl">
        <p className="text-sm text-muted-foreground" data-testid="agent-detail-loading">
          Loading {name}…
        </p>
      </div>
    );
  }

  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="mx-auto max-w-5xl">
          <ForbiddenInline
            title={`Not allowed to view ${name}`}
            description="Your account can't read this agent in this namespace."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="mx-auto max-w-5xl" data-testid="agent-not-found">
          <EmptyState
            icon={Boxes}
            title="Agent not found"
            description={`No AgentDeployment "${name}" in ${ns || "this namespace"}. It may have been deleted, or the name is wrong.`}
            action={{ label: "Back to agents", onClick: () => history.back() }}
          />
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-5xl">
        <div
          className="rounded-lg border bg-card p-6 text-sm text-destructive shadow-card"
          role="alert"
          data-testid="agent-detail-error"
        >
          Couldn't load the agent: {state.message}
        </div>
      </div>
    );
  }

  const detail = state.detail;

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="agent-detail-page">
      <AgentHeader detail={detail} />

      <div className="flex flex-wrap gap-1 border-b" role="tablist" aria-label="Agent detail">
        {TABS.map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            data-testid={`tab-${t.toLowerCase()}`}
            className={`-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
              tab === t
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground"
            }`}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === "Overview" && (
        <OverviewTab detail={detail} onTraced={(id) => setInspectTrace(id)} />
      )}
      {tab === "Logs" && <LogsTab ns={detail.namespace} name={detail.name} ready={detail.ready} />}
      {tab === "Runs" && (
        <RunsTab agentName={detail.name} onInspect={(id) => setInspectTrace(id)} />
      )}
      {tab === "Bindings" && <BindingsTab bindings={detail.bindings} />}

      {/* The run inspector opens over the page (drawer) so list/tab context is
          kept. It closes back to exactly where you were. */}
      <DetailDrawer
        open={inspectTrace !== null}
        onClose={() => setInspectTrace(null)}
        title="Run inspector"
        subtitle={inspectTrace ?? undefined}
        size="lg"
      >
        {inspectTrace && <RunInspector traceId={inspectTrace} />}
      </DetailDrawer>
    </div>
  );
}

// ── Header ──────────────────────────────────────────────────────────────────
function AgentHeader({ detail }: { detail: AgentDetailResponse }) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-2xl font-semibold tracking-tight">{detail.name}</h2>
        <Badge variant={detail.ready ? "success" : "warning"}>
          {detail.phase || (detail.ready ? "Ready" : "Pending")}
        </Badge>
        <span className="text-sm text-muted-foreground">{detail.namespace}</span>
      </div>
      <dl className="grid grid-cols-1 gap-x-8 gap-y-1.5 text-sm sm:grid-cols-2 lg:grid-cols-3">
        {detail.url && (
          <HeaderKV
            k="Route"
            v={
              <a
                href={detail.url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 truncate font-mono text-xs text-primary hover:underline"
                data-testid="agent-url"
              >
                {detail.url}
                <ExternalLink className="h-3 w-3 shrink-0" />
              </a>
            }
          />
        )}
        {detail.image && (
          <HeaderKV k="Image" v={<span className="truncate font-mono text-xs">{detail.image}</span>} />
        )}
        {detail.executionModel && <HeaderKV k="Execution" v={detail.executionModel} />}
        {detail.role && <HeaderKV k="Role" v={detail.role} />}
        <HeaderKV k="Scaling" v={`${detail.scaling.min} – ${detail.scaling.max}`} />
        {detail.latestVersion && (
          <HeaderKV k="Latest version" v={<span className="font-mono text-xs">{detail.latestVersion}</span>} />
        )}
      </dl>
    </div>
  );
}

function HeaderKV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <dt className="shrink-0 text-muted-foreground">{k}</dt>
      <dd className="min-w-0 truncate">{v}</dd>
    </div>
  );
}

// ── Overview tab (spec summary + status timeline + Run panel) ────────────────
function OverviewTab({
  detail,
  onTraced,
}: {
  detail: AgentDetailResponse;
  onTraced: (traceId: string) => void;
}) {
  return (
    <div className="grid gap-6 lg:grid-cols-[1fr_20rem]">
      <div className="space-y-6">
        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Spec</p>
          <dl className="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
            <SpecKV k="Execution" v={detail.executionModel || "—"} />
            <SpecKV k="Image" v={<span className="font-mono text-xs">{detail.image || "—"}</span>} />
            <SpecKV k="Role" v={detail.role || "—"} />
            <SpecKV k="Scaling" v={`${detail.scaling.min} – ${detail.scaling.max}`} />
          </dl>
        </div>

        <StatusTimeline conditions={detail.conditions} ready={detail.ready} phase={detail.phase} />

        {detail.versions.length > 0 && (
          <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="versions-list">
            <p className="mb-3 text-sm font-medium">Versions</p>
            <ul className="space-y-1.5">
              {detail.versions.map((v) => (
                <li key={v} className="flex items-center gap-2 text-sm">
                  <span className="font-mono text-xs">{v}</span>
                  {v === detail.latestVersion && (
                    <Badge variant="secondary" className="text-[10px]">
                      latest
                    </Badge>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}

        <div className="rounded-lg border bg-card p-5 shadow-card">
          <p className="mb-3 text-sm font-medium">Bindings</p>
          <BindingsList bindings={detail.bindings} />
        </div>
      </div>

      <RunPanel
        ns={detail.namespace}
        name={detail.name}
        ready={detail.ready}
        onTraced={onTraced}
      />
    </div>
  );
}

function SpecKV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{k}</dt>
      <dd>{v}</dd>
    </>
  );
}

// StatusTimeline renders the readiness progression from the status conditions —
// the "watch it come alive" surface. Each condition is a dot (green True / red
// False / muted Unknown) + its type/reason/message.
function StatusTimeline({
  conditions,
  ready,
  phase,
}: {
  conditions: AgentCondition[];
  ready: boolean;
  phase: string;
}) {
  return (
    <div className="rounded-lg border bg-card p-5 shadow-card" data-testid="status-timeline">
      <div className="mb-3 flex items-center gap-2">
        <p className="text-sm font-medium">Status timeline</p>
        <Badge variant={ready ? "success" : "warning"} className="text-[10px]">
          {phase || (ready ? "Ready" : "Pending")}
        </Badge>
      </div>
      {conditions.length === 0 ? (
        <p className="text-sm text-muted-foreground">No status conditions reported yet.</p>
      ) : (
        <ol className="space-y-3">
          {conditions.map((c, i) => {
            const tone =
              c.status === "True"
                ? "bg-success"
                : c.status === "False"
                  ? "bg-destructive"
                  : "bg-border-strong";
            return (
              <li key={`${c.type}-${i}`} className="flex gap-3" data-testid={`condition-${c.type}`}>
                <div className="flex flex-col items-center">
                  <span className={`mt-1 h-2.5 w-2.5 rounded-full ${tone}`} />
                  {i < conditions.length - 1 && <span className="mt-1 h-full w-px bg-border" />}
                </div>
                <div className="pb-1">
                  <p className="text-sm font-medium">
                    {c.type}
                    {c.reason && (
                      <span className="ml-2 font-normal text-muted-foreground">{c.reason}</span>
                    )}
                  </p>
                  {c.message && <p className="text-xs text-muted-foreground">{c.message}</p>}
                  {c.lastTransitionTime && (
                    <p className="text-[10px] text-muted-foreground">{c.lastTransitionTime}</p>
                  )}
                </div>
              </li>
            );
          })}
        </ol>
      )}
    </div>
  );
}

// ── Run panel (define input → invoke → open the run inspector) ───────────────
type RunState =
  | { kind: "idle" }
  | { kind: "running" }
  | { kind: "done"; traceId: string; response: string }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

function RunPanel({
  ns,
  name,
  ready,
  onTraced,
}: {
  ns: string;
  name: string;
  ready: boolean;
  onTraced: (traceId: string) => void;
}) {
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
  const [input, setInput] = React.useState('{\n  "prompt": "Hello, agent"\n}');
  const [run, setRun] = React.useState<RunState>({ kind: "idle" });

  async function onRun() {
    let parsed: unknown;
    try {
      parsed = input.trim() ? JSON.parse(input) : {};
    } catch {
      setRun({ kind: "error", message: "Input must be valid JSON.", forbidden: false });
      return;
    }
    setRun({ kind: "running" });
    try {
      const res = await api.invoke({ agent: name, namespace: ns, input: parsed });
      setRun({ kind: "done", traceId: res.traceId, response: res.response });
      onTraced(res.traceId);
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      const apiErr = err instanceof ApiError ? err : null;
      setRun({
        kind: "error",
        message: err instanceof Error ? err.message : "run failed",
        status: apiErr?.status,
        forbidden: apiErr?.isForbidden ?? false,
      });
    }
  }

  return (
    <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="run-panel">
      <div className="mb-3 flex items-center gap-2">
        <Play className="h-4 w-4 text-primary" />
        <p className="text-sm font-medium">Run</p>
      </div>

      {!canRun ? (
        <p
          className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-xs text-muted-foreground"
          data-testid="run-readonly-note"
        >
          You have read-only access — running an agent requires create permission on
          AgentDeployments.
        </p>
      ) : (
        <>
          <Textarea
            aria-label="Run input (JSON)"
            rows={5}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            className="font-mono text-xs"
          />
          {!ready && (
            <p className="mt-2 text-xs text-warning-foreground" data-testid="run-not-ready-note">
              The agent isn't Ready yet — a run may fail until it comes up.
            </p>
          )}
          <Button
            className="mt-3 w-full"
            size="sm"
            onClick={onRun}
            disabled={run.kind === "running"}
            data-testid="run-button"
          >
            <Play className="h-4 w-4" />
            {run.kind === "running" ? "Running…" : "Run agent"}
          </Button>

          {run.kind === "done" && (
            <div
              className="mt-4 space-y-2 rounded-md bg-surface-3 p-3 text-xs"
              data-testid="run-result"
            >
              <div className="flex items-center gap-1.5 text-success">
                <CheckCircle2 className="h-4 w-4" />
                <span className="font-medium">Traced run complete</span>
              </div>
              <p className="whitespace-pre-wrap break-words">{run.response}</p>
              <button
                type="button"
                onClick={() => onTraced(run.traceId)}
                className="font-mono text-primary hover:underline"
                data-testid="open-trace"
              >
                trace {run.traceId} →
              </button>
            </div>
          )}

          {run.kind === "error" && run.forbidden && (
            <div className="mt-3">
              <ForbiddenInline
                title="Not allowed to run this agent"
                description="Your account can't invoke agents in this cluster."
                detail={run.message}
              />
            </div>
          )}
          {run.kind === "error" && !run.forbidden && (
            <p className="mt-3 text-xs text-destructive" role="alert" data-testid="run-error">
              {run.message}
              {run.status ? ` (${run.status})` : ""}
            </p>
          )}
        </>
      )}
    </div>
  );
}

// ── Logs tab (live SSE tail, bearer-attached fetch-stream) ───────────────────
type LogLine = { seq: number; text: string };
type LogPhase = "connecting" | "waiting" | "streaming" | "ended" | "error" | "forbidden";

function LogsTab({ ns, name, ready }: { ns: string; name: string; ready: boolean }) {
  const [lines, setLines] = React.useState<LogLine[]>([]);
  const [phase, setPhase] = React.useState<LogPhase>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
  const seqRef = React.useRef(0);

  React.useEffect(() => {
    setLines([]);
    setPhase("connecting");
    setErrorMsg("");
    seqRef.current = 0;

    // The SSE tail over fetch-stream: the Bearer rides the request (EventSource
    // can't set headers). We follow the stream and render every frame honestly.
    const cancel = openLogStream(
      ns,
      name,
      {
        onEvent: (type: LogEventType, data: string) => {
          if (type === "log") {
            setPhase("streaming");
            setLines((prev) => [...prev, { seq: seqRef.current++, text: data }]);
          } else if (type === "waiting") {
            setPhase((p) => (p === "streaming" ? p : "waiting"));
          } else if (type === "error") {
            // An IN-STREAM error frame (mid-stream break / pods-log denied after
            // the stream opened) — surfaced honestly, distinct from a pre-stream
            // 403 (handled by onForbidden below).
            setPhase("error");
            setErrorMsg(data);
          } else if (type === "end") {
            setPhase((p) => (p === "error" || p === "forbidden" ? p : "ended"));
          }
        },
        // A PRE-STREAM 403 (RBAC denied pods list) — an HTTP status before any
        // frame. Rendered as a forbidden state, NOT an in-stream error.
        onForbidden: (message: string) => {
          setPhase("forbidden");
          setErrorMsg(message);
        },
        onError: (message: string) => {
          setPhase("error");
          setErrorMsg(message);
        },
      },
      { follow: true, tailLines: 200 },
    );

    // Cancel the stream on unmount / tab-switch — no leak.
    return cancel;
  }, [ns, name]);

  if (phase === "forbidden") {
    return (
      <ForbiddenInline
        title="Not allowed to read logs"
        description="Your account can't read pod logs in this namespace."
        detail={errorMsg}
      />
    );
  }

  return (
    <div className="overflow-hidden rounded-lg border bg-surface-3" data-testid="logs-tab">
      <div className="flex items-center justify-between border-b bg-card/60 px-4 py-2">
        <div className="flex items-center gap-2 text-sm">
          <Terminal className="h-4 w-4" /> Live tail
          {phase === "streaming" && (
            <span className="h-2 w-2 animate-pulse rounded-full bg-success" aria-label="streaming" />
          )}
        </div>
        <span className="text-xs text-muted-foreground" data-testid="logs-status">
          {phase === "connecting" && "connecting…"}
          {phase === "waiting" && "waiting for the agent to start"}
          {phase === "streaming" && `${lines.length} lines`}
          {phase === "ended" && "stream ended"}
          {phase === "error" && "stream error"}
        </span>
      </div>

      {phase === "waiting" && lines.length === 0 ? (
        <div
          className="flex h-40 items-center justify-center text-sm text-muted-foreground"
          data-testid="logs-waiting"
        >
          {ready
            ? "Waiting for the agent to start — no running pod yet."
            : "The agent is still coming up — waiting for its first pod."}
        </div>
      ) : (
        <pre className="max-h-80 overflow-y-auto p-4 font-mono text-xs leading-relaxed">
          {lines.map((l) => (
            <div key={l.seq} data-testid="log-line">
              {l.text}
            </div>
          ))}
          {phase === "error" && (
            <div className="mt-2 text-destructive" role="alert" data-testid="logs-error">
              — log stream error: {errorMsg}
            </div>
          )}
          {phase === "ended" && lines.length === 0 && (
            <div className="text-muted-foreground">No log output.</div>
          )}
        </pre>
      )}
    </div>
  );
}

// ── Runs tab (recent runs filtered to this agent) ────────────────────────────
// Reuses the shipped GET /api/runs list (m12) and filters client-side by the
// agent's name (the run's `name` is the agent name). Cheap and honest — a full
// per-agent server filter is a later surface; here we scope the recent window.
function RunsTab({
  agentName,
  onInspect,
}: {
  agentName: string;
  onInspect: (traceId: string) => void;
}) {
  const [state, setState] = React.useState<
    | { kind: "loading" }
    | { kind: "ready"; runs: RunSummary[] }
    | { kind: "error"; message: string; forbidden: boolean }
  >({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .runs(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.runs.filter((r) => r.name === agentName);
        setState({ kind: "ready", runs: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load runs",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [agentName]);

  const cols: Column<RunSummary>[] = [
    {
      id: "traceId",
      header: "Run",
      cell: (r) => <span className="font-mono text-xs">{r.traceId}</span>,
    },
    { id: "timestamp", header: "When", hideOnMobile: true, cell: (r) => r.timestamp },
    {
      id: "tokens",
      header: "Tokens",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">{r.tokens.toLocaleString()}</span>,
    },
    {
      id: "cost",
      header: "Cost",
      className: "text-right",
      cell: (r) => <span className="tabular-nums">${r.costUSD.toFixed(3)}</span>,
    },
    {
      id: "latency",
      header: "Latency",
      className: "text-right",
      hideOnMobile: true,
      cell: (r) => <span className="tabular-nums">{Math.round(r.latencyMs)}ms</span>,
    },
  ];

  if (state.kind === "error" && state.forbidden) {
    return (
      <ForbiddenInline
        title="Not allowed to read runs"
        description="Your account can't read run history in this cluster."
        detail={state.message}
      />
    );
  }

  return (
    <div data-testid="runs-tab">
      <DataTable<RunSummary>
        columns={cols}
        rows={state.kind === "ready" ? state.runs : []}
        rowKey={(r) => r.traceId}
        loading={state.kind === "loading"}
        error={
          state.kind === "error"
            ? { message: state.message, forbidden: false, onRetry: undefined }
            : null
        }
        onRowClick={(r) => onInspect(r.traceId)}
        ariaLabel="Recent runs"
        empty={{
          icon: Play,
          title: "No runs yet",
          description: "Run this agent from the Overview tab to see its traced runs here.",
        }}
      />
    </div>
  );
}

// ── Bindings tab / list ──────────────────────────────────────────────────────
function BindingsTab({ bindings }: { bindings: AgentBinding[] }) {
  return (
    <div data-testid="bindings-tab">
      <BindingsList bindings={bindings} />
    </div>
  );
}

function BindingsList({ bindings }: { bindings: AgentBinding[] }) {
  if (bindings.length === 0) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="bindings-empty">
        No bindings reference this agent yet.
      </p>
    );
  }
  return (
    <div className="space-y-2">
      {bindings.map((b) => (
        <div
          key={`${b.kind}/${b.name}`}
          className="flex items-center justify-between gap-3 rounded-md border bg-surface-2/40 px-4 py-3 text-sm"
          data-testid={`binding-${b.name}`}
        >
          <div className="flex min-w-0 items-center gap-2">
            <Badge variant="secondary" className="text-[10px]">
              {b.kind}
            </Badge>
            <span className="truncate">{b.detail || b.name}</span>
          </div>
          <Badge variant={b.ready ? "success" : "warning"} className="text-[10px]">
            {b.ready ? "ready" : "pending"}
          </Badge>
        </div>
      ))}
    </div>
  );
}
