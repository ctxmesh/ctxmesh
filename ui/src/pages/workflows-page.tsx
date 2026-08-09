import { useCallback, useEffect, useRef, useState } from "react";
import { GitFork } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { api, ApiError, type WorkflowSummary } from "@/lib/api";

// WorkflowsPage — the Workflow CR list surface (m67.9, ADR 0060).
//
// Read-only (caller-scoped, ADR 0011): each row is a Workflow CR — its name, namespace,
// step count, registry trust boundary, and validated status. A workflow is authored via
// YAML/kubectl; the console surfaces it for visibility and operator awareness.
// An invoke affordance lets the user start a workflow instance run from the list.
// A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// data-testid contract:
//   workflows-page         — root container
//   workflows-table        — the DataTable (aria-label="Workflows")
//   workflow-row-{name}    — each row (via rowKey)

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; workflows: WorkflowSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function WorkflowsPage() {
  const [query, setQuery] = useState("");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

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
        w.validated ? (
          <Badge variant="success">valid</Badge>
        ) : (
          <Badge variant="warning" title={w.reason}>
            {w.reason || "invalid"}
          </Badge>
        ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="workflows-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Workflows</h2>
        <p className="text-sm text-muted-foreground">
          Declarative graphs of agent invocations — conditional branching, map/loop control flow, and
          deterministic execution. Each workflow is validated by the controller (structure + CEL + registry
          membership) before it can be invoked. Authored via YAML/kubectl.
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
    </div>
  );
}
