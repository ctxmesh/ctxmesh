import { useMemo } from "react";
import {
  Background,
  Controls,
  Handle,
  Position,
  ReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import { cn } from "@/lib/utils";
import type {
  TopologyEdge,
  TopologyHealth,
  TopologyNode,
  TopologyResponse,
} from "@/lib/api";

// TopologyGraph renders the live cluster topology (registries → agents → tools)
// as a React Flow graph from the BFF's flat DTO. It is on-theme: every color
// resolves to a SEMANTIC design token (no hardcoded hex) so a re-brand is a
// token edit. Health maps to the status tokens (success/warning/muted).

// healthClasses maps a node's health onto SEMANTIC token utilities — never a raw
// color. Re-branding these is a token change in tokens.css.
const healthClasses: Record<TopologyHealth, string> = {
  ready: "border-success text-success",
  notReady: "border-destructive text-destructive",
  pending: "border-warning text-warning",
  unknown: "border-muted-foreground text-muted-foreground",
};

const kindLabel: Record<TopologyNode["kind"], string> = {
  registry: "Registry",
  agent: "Agent",
  tool: "Tool",
};

type FlowNodeData = {
  label: string;
  kind: TopologyNode["kind"];
  health: TopologyHealth;
  detail: string;
};

// TopologyFlowNode is the custom React Flow node — a token-driven card. It
// exposes source+target handles so edges attach on either side (the graph flows
// left→right: registry → agent → tool).
function TopologyFlowNode({ data }: NodeProps) {
  const d = data as FlowNodeData;
  return (
    <div
      className={cn(
        "min-w-40 rounded-md border-2 bg-card px-3 py-2 shadow-card",
        healthClasses[d.health],
      )}
    >
      <Handle
        type="target"
        position={Position.Left}
        className="!bg-muted-foreground"
      />
      <p className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
        {kindLabel[d.kind]}
      </p>
      <p className="truncate text-sm font-semibold text-card-foreground">
        {d.label}
      </p>
      {d.detail && (
        <p className="truncate font-mono text-[10px] text-muted-foreground">
          {d.detail}
        </p>
      )}
      <Handle
        type="source"
        position={Position.Right}
        className="!bg-muted-foreground"
      />
    </div>
  );
}

const nodeTypes = { topology: TopologyFlowNode };

// columnFor places a node in one of three left→right columns by kind, so the
// graph reads registries → agents → tools without a heavyweight layout engine.
const columnFor: Record<TopologyNode["kind"], number> = {
  registry: 0,
  agent: 1,
  tool: 2,
};

// toFlow projects the flat topology DTO onto React Flow nodes+edges, assigning a
// deterministic column/row layout (stable across renders and tests).
function toFlow(topology: TopologyResponse): { nodes: Node[]; edges: Edge[] } {
  const rowByColumn = new Map<number, number>();
  const nodes: Node[] = topology.nodes.map((n: TopologyNode) => {
    const col = columnFor[n.kind];
    const row = rowByColumn.get(col) ?? 0;
    rowByColumn.set(col, row + 1);
    return {
      id: n.id,
      type: "topology",
      position: { x: col * 260, y: row * 110 },
      data: {
        label: n.name,
        kind: n.kind,
        health: n.health,
        detail: n.detail,
      } satisfies FlowNodeData,
    };
  });
  const edges: Edge[] = topology.edges.map((e: TopologyEdge) => ({
    id: e.id,
    source: e.source,
    target: e.target,
  }));
  return { nodes, edges };
}

export function TopologyGraph({ topology }: { topology: TopologyResponse }) {
  const { nodes, edges } = useMemo(() => toFlow(topology), [topology]);

  if (topology.nodes.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No registries, agents, or tools yet.
      </div>
    );
  }

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      fitView
      proOptions={{ hideAttribution: true }}
      nodesDraggable={false}
      nodesConnectable={false}
    >
      <Background className="text-border" />
      <Controls showInteractive={false} />
    </ReactFlow>
  );
}
