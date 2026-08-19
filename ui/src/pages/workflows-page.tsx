import { useCallback, useEffect, useRef, useState } from "react";
import { GitFork, Play } from "lucide-react";
import { useNavigate } from "react-router-dom";

import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import { api, ApiError, type WorkflowSummary } from "@/lib/api";

// WorkflowsPage — the Workflow CR list surface (m67.9, ADR 0060).
//
// Read-only (caller-scoped, ADR 0011): each row is a Workflow CR — its name, namespace,
// step count, registry trust boundary, and validated status. A workflow is authored via
// YAML/kubectl; the console surfaces it for visibility and operator awareness.
// An invoke affordance lets the user start a workflow instance run from the list.
// A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// Invoke (m67.15): the per-row "Run" button opens an inline panel that calls
// api.createWorkflowRun(name, { input, namespace }) and navigates to the created run
// (the run trace view at /traces/:id). A minimal JSON input box (default empty object)
// is provided; invoke-with-empty-input is valid when the workflow's inputSchema is open.
//
// data-testid contract:
//   workflows-page         — root container
//   workflows-table        — the DataTable (aria-label="Workflows")
//   workflow-row-{name}    — each row (via rowKey)
//   invoke-panel           — the invoke card (when a workflow is selected)
//   invoke-input           — the JSON input textarea
//   invoke-submit          — the submit button
//   invoke-error           — error text on failure
//   invoke-cancel          — dismiss the invoke panel

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; workflows: WorkflowSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

type InvokeState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "error"; message: string };

export function WorkflowsPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  // Invoke panel state: selected workflow + input + submission state.
  const [invokeTarget, setInvokeTarget] = useState<WorkflowSummary | null>(null);
  const [invokeInput, setInvokeInput] = useState('{}');
  const [invokeState, setInvokeState] = useState<InvokeState>({ kind: "idle" });

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listWorkflows(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", workflows: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const all = loadState.kind === "ready" ? loadState.workflows : [];
  const q = query.trim().toLowerCase();
  const workflows = q ? all.filter((w) => w.name.toLowerCase().includes(q)) : all;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // openInvoke selects a workflow for the invoke panel.
  function openInvoke(wf: WorkflowSummary) {
    setInvokeTarget(wf);
    setInvokeInput("{}");
    setInvokeState({ kind: "idle" });
  }

  // closeInvoke dismisses the panel without invoking.
  function closeInvoke() {
    setInvokeTarget(null);
    setInvokeState({ kind: "idle" });
  }

  // onInvoke POSTs the workflow run and navigates to the created run's trace view on 202.
  async function onInvoke() {
    if (!invokeTarget) return;
    let parsedInput: unknown;
    try {
      parsedInput = invokeInput.trim() ? JSON.parse(invokeInput) : {};
    } catch {
      setInvokeState({ kind: "error", message: "Input must be valid JSON." });
      return;
    }
    setInvokeState({ kind: "submitting" });
    try {
      const result = await api.createWorkflowRun(invokeTarget.name, {
        input: parsedInput,
        namespace: invokeTarget.namespace,
      });
      // Navigate to the run's trace view — the run id is the trace entry point.
      navigate(`/traces/${encodeURIComponent(result.id)}`);
    } catch (err) {
      setInvokeState({
        kind: "error",
        message: err instanceof Error ? err.message : "invoke failed",
      });
    }
  }

  const columns: Column<WorkflowSummary>[] = [
    {
      id: "name",
      header: "Workflow",
      cell: (w) => <span className="font-medium">{w.name}</span>,
    },
    {
      id: "registry",
      header: "Registry",
      hideOnMobile: true,
      cell: (w) => (
        <span className="text-sm text-muted-foreground">{w.registryRef}</span>
      ),
    },
    {
      id: "steps",
      header: "Steps",
      hideOnMobile: true,
      cell: (w) => (
        <span className="text-sm text-muted-foreground">
          {w.stepCount === 1 ? "1 step" : `${w.stepCount} steps`}
        </span>
      ),
    },
    {
      id: "namespace",
      header: "Namespace",
      hideOnMobile: true,
      cell: (w) => (
        <span className="text-sm text-muted-foreground">{w.namespace}</span>
      ),
    },
    {
      id: "status",
      header: "Status",
      cell: (w) =>
        <StatusBadge ready={w.validated} phase={w.validated ? undefined : w.reason} />,
    },
    {
      id: "invoke",
      header: "",
      className: "text-right",
      cell: (w) => (
        <Button
          size="sm"
          variant="outline"
          disabled={!w.validated}
          title={w.validated ? `Run ${w.name}` : "Workflow is not valid — cannot invoke"}
          data-testid={`invoke-btn-${w.name}`}
          onClick={(e) => {
            e.stopPropagation();
            openInvoke(w);
          }}
        >
          <Play className="h-3.5 w-3.5" />
          Run
        </Button>
      ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="workflows-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Workflows</h2>
        <p className="text-sm text-muted-foreground">
          Declarative graphs of agent invocations — conditional branching, map/loop control flow, and
          deterministic execution. Each workflow is validated before it can be invoked.
        </p>
      </div>

      <DataTable<WorkflowSummary>
        columns={columns}
        rows={workflows}
        rowKey={(w) => `${w.namespace}/${w.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter workflows by name…"
        ariaLabel="Workflows"
        empty={{
          icon: GitFork,
          title: "No workflows",
          description:
            "No Workflow CRs defined yet. Apply a Workflow manifest with kubectl to define a declarative graph of agent invocations.",
        }}
      />

      {/* Invoke panel — shown when a workflow row's Run button is clicked. */}
      {invokeTarget && (
        <Card data-testid="invoke-panel">
          <CardHeader>
            <CardTitle className="text-base">
              Run <span className="font-mono">{invokeTarget.name}</span>
            </CardTitle>
            <CardDescription>
              Provide JSON input for the workflow invocation. An empty object{" "}
              <code className="font-mono text-xs">{"{}"}</code> is fine when the
              workflow accepts no required input.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="space-y-1">
              <label
                htmlFor="invoke-input"
                className="text-xs font-medium text-muted-foreground"
              >
                Input (JSON)
              </label>
              <Textarea
                id="invoke-input"
                data-testid="invoke-input"
                className="min-h-[6rem] font-mono text-xs"
                value={invokeInput}
                onChange={(e) => setInvokeInput(e.target.value)}
                placeholder="{}"
                disabled={invokeState.kind === "submitting"}
              />
            </div>
            {invokeState.kind === "error" && (
              <p className="text-sm text-destructive" role="alert" data-testid="invoke-error">
                {invokeState.message}
              </p>
            )}
            <div className="flex items-center gap-3">
              <Button
                data-testid="invoke-submit"
                onClick={() => void onInvoke()}
                disabled={invokeState.kind === "submitting"}
              >
                <Play className="h-4 w-4" />
                {invokeState.kind === "submitting" ? "Starting…" : "Start workflow run"}
              </Button>
              <Button
                variant="outline"
                data-testid="invoke-cancel"
                onClick={closeInvoke}
                disabled={invokeState.kind === "submitting"}
              >
                Cancel
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
