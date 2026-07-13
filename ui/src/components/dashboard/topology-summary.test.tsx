import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { TopologySummary } from "@/components/dashboard/topology-summary";
import type { TopologyResponse, TopologyNode } from "@/lib/api";

function node(over: Partial<TopologyNode>): TopologyNode {
  return {
    id: "agent/default/x",
    kind: "agent",
    name: "x",
    namespace: "default",
    health: "ready",
    detail: "",
    ...over,
  };
}

function topo(nodes: TopologyNode[]): TopologyResponse {
  return { nodes, edges: [] };
}

describe("TopologySummary (scale-first dashboard card, m22.6/U5)", () => {
  it("shows counts + health rollup, not a node graph", () => {
    render(
      <MemoryRouter>
        <TopologySummary
          topology={topo([
            node({ id: "registry/default/r", kind: "registry", name: "r" }),
            node({ id: "agent/default/a1", name: "a1", health: "ready" }),
            node({ id: "agent/default/a2", name: "a2", health: "notReady" }),
            node({ id: "tool/default/t", kind: "tool", name: "t" }),
          ])}
        />
      </MemoryRouter>,
    );
    const card = screen.getByTestId("topology-summary");
    expect(card).toHaveTextContent("Registries");
    expect(card).toHaveTextContent("Agents");
    // 3 ready (registry + agent + tool), 1 not ready (a2).
    expect(card).toHaveTextContent("3 ready");
    expect(card).toHaveTextContent("1 not ready");
  });

  it("lists unhealthy nodes as hotspots, linked to their detail", () => {
    render(
      <MemoryRouter>
        <TopologySummary
          topology={topo([
            node({ id: "agent/default/ok", name: "ok", health: "ready" }),
            node({ id: "agent/default/bad", name: "bad", health: "notReady" }),
          ])}
        />
      </MemoryRouter>,
    );
    const hotspots = screen.getByTestId("topology-hotspots");
    const link = within(hotspots).getByText("bad");
    expect(link.tagName).toBe("A");
    expect(link).toHaveAttribute("href", "/agents/default/bad");
    // A healthy node is NOT a hotspot.
    expect(within(hotspots).queryByText("ok")).toBeNull();
  });

  it("renders an empty state when there is nothing yet", () => {
    render(
      <MemoryRouter>
        <TopologySummary topology={topo([])} />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("topology-summary-empty")).toBeInTheDocument();
  });
});
