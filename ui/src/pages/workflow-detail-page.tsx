import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, ArrowRight, GitBranch, Repeat, Rows3, Shield, Workflow } from "lucide-react";

import { ForbiddenInline, StatusBadge } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { buttonVariants } from "@/components/ui/button";
import {
  api,
  ApiError,
  type WorkflowDetailResponse,
  type WorkflowGraphEdge,
  type WorkflowGraphNode,
} from "@/lib/api";

// WorkflowDetailPage — the DECLARED workflow DAG on the shared delegation canvas
// (M144-canvas, ADR 0115). Renders GET /api/workflows/{ns}/{name}: each step is a
// canvas node (task | choice | map | loop) and its control flow is labeled edges
// (next / branch / default / catch / map / join / loop). Read-only — the declared
// structure, not a run (a run gets the Live lens on the run detail page).

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; detail: WorkflowDetailResponse }
  | { kind: "error"; message: string; forbidden: boolean };

// Edge tint follows the canvas status vocabulary: error paths red, conditional/
// fan-out flow info-blue, plain sequencing muted. Never brand.
const EDGE_TINT: Record<string, string> = {
  next: "text-muted-foreground",
  default: "text-muted-foreground",
  branch: "text-info",
  catch: "text-destructive",
  map: "text-info",
  join: "text-info",
  loop: "text-info",
};

function NodeKindMark({ kind }: { kind: string }) {
  if (kind === "choice") return <GitBranch className="h-3.5 w-3.5 text-muted-foreground" />;
  if (kind === "map") return <Rows3 className="h-3.5 w-3.5 text-muted-foreground" />;
  if (kind === "loop") return <Repeat className="h-3.5 w-3.5 text-muted-foreground" />;
  return <span className="h-2 w-2 rounded-full bg-muted-foreground/60" aria-hidden />;
}

function EdgeRow({ edge }: { edge: WorkflowGraphEdge }) {
  const tint = EDGE_TINT[edge.kind] ?? "text-muted-foreground";
  return (
    <div className="flex items-center gap-1.5 text-xs">
      <ArrowRight className={`h-3.5 w-3.5 shrink-0 ${tint}`} />
      <span className={`font-medium ${tint}`}>{edge.kind}</span>
      {edge.label && edge.label !== edge.kind && (
        <span className="truncate font-mono text-[11px] text-muted-foreground" title={edge.label}>
          {edge.label}
        </span>
      )}
      <span className="text-muted-foreground">→</span>
      <span className="truncate font-mono text-[11px] font-medium">{edge.to}</span>
    </div>
  );
}

function WorkflowNodeCard({ node }: { node: WorkflowGraphNode }) {
  return (
    <div className="rounded-md border bg-card px-3 py-2" data-testid={`workflow-node-${node.name}`}>
      <div className="flex items-center gap-2">
        <NodeKindMark kind={node.kind} />
        <span className="truncate text-sm font-medium">{node.name}</span>
        {node.start && (
          <Badge variant="secondary" className="text-[10px]">
            start
          </Badge>
        )}
        <Badge variant="muted" className="ml-auto text-[10px]">
          {node.kind}
        </Badge>
      </div>
      {node.agentRef && (
        <p className="mt-0.5 font-mono text-[11px] text-muted-foreground">→ {node.agentRef}</p>
      )}
      {node.edges.length > 0 ? (
        <div className="mt-2 space-y-1 border-t border-dashed border-muted-foreground/25 pt-2">
          {node.edges.map((e, i) => (
            <EdgeRow key={`${e.kind}-${e.to}-${i}`} edge={e} />
          ))}
        </div>
      ) : (
        <p className="mt-2 border-t border-dashed border-muted-foreground/25 pt-2 text-[11px] text-muted-foreground">
          terminal — the workflow ends here
        </p>
      )}
    </div>
  );
}

export function WorkflowDetailPage() {
  const { ns, name } = useParams<{ ns: string; name: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    if (!ns || !name) return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .getWorkflow(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [ns, name]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="workflow-detail-page">
      <Link
        to="/workflows"
        className={buttonVariants({ variant: "ghost", size: "sm" }) + " w-fit"}
      >
        <ArrowLeft className="mr-2 h-4 w-4" />
        Workflows
      </Link>

      {state.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="workflow-detail-loading">
          Loading workflow…
        </p>
      )}

      {state.kind === "error" &&
        (state.forbidden ? (
          <ForbiddenInline
            title="Not allowed to view this workflow"
            description="Your account can't read workflows in this namespace."
            detail={state.message}
          />
        ) : (
          <p className="text-sm text-destructive" role="alert" data-testid="workflow-detail-error">
            {state.message}
          </p>
        ))}

      {state.kind === "ready" && (
        <>
          <div className="flex items-start justify-between gap-4">
            <div>
              <div className="flex items-center gap-2">
                <Workflow className="h-5 w-5 text-muted-foreground" />
                <h2 className="text-2xl font-semibold tracking-tight">{state.detail.name}</h2>
              </div>
              <p className="mt-1 text-sm text-muted-foreground">
                The declared step graph — {state.detail.nodes.length} step
                {state.detail.nodes.length === 1 ? "" : "s"} and how control flows between them.
              </p>
            </div>
            <StatusBadge
              ready={state.detail.validated}
              phase={state.detail.validated ? "Validated" : "Not validated"}
              reason={state.detail.reason}
            />
          </div>

          {/* Trust boundary (same fence framing as the Team Sheet). */}
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 rounded-md border-l-2 border-info bg-info/5 px-3 py-2 text-xs">
            <Shield className="h-3.5 w-3.5 text-info" />
            <span className="text-muted-foreground">Trust boundary · registry</span>
            <span className="font-mono font-medium">{state.detail.registryRef}</span>
            <span className="font-mono text-muted-foreground">· namespace {state.detail.namespace}</span>
          </div>

          {/* The DAG — nodes in declared order (start first), each with its labeled
              outgoing edges. Robust, no fragile layout: the edge rows name their
              targets, so the graph structure reads without spaghetti. */}
          <div className="space-y-2" data-testid="workflow-dag">
            {state.detail.nodes.map((node) => (
              <WorkflowNodeCard key={node.name} node={node} />
            ))}
          </div>
        </>
      )}
    </div>
  );
}
