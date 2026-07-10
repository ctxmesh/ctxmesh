import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";

import { AgentsPage } from "@/pages/agents-page";

function stubFetch(body: unknown, ok = true, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok,
      status,
      json: async () => body,
    } as Response),
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("AgentsPage (foundation proof)", () => {
  it("renders the empty-state when the BFF returns no agents", async () => {
    stubFetch({ agents: [] });
    render(<AgentsPage />);
    expect(await screen.findByText("No agents yet")).toBeInTheDocument();
  });

  it("renders an agent row from the BFF list", async () => {
    stubFetch({
      agents: [
        {
          name: "echo",
          namespace: "prod",
          image: "echo:1.2.3",
          phase: "Ready",
          ready: true,
        },
      ],
    });
    render(<AgentsPage />);
    expect(await screen.findByText("echo")).toBeInTheDocument();
    expect(screen.getByText("prod")).toBeInTheDocument();
    expect(screen.getByText("echo:1.2.3")).toBeInTheDocument();
  });

  it("shows an error state when the BFF rejects (e.g. RBAC 403)", async () => {
    stubFetch({}, false, 403);
    render(<AgentsPage />);
    expect(await screen.findByText(/Failed to load agents/)).toBeInTheDocument();
  });
});
