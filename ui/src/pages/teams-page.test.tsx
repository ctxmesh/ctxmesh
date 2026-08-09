import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { TeamsPage } from "@/pages/teams-page";
import type { AgentTeamSummary } from "@/lib/api";

// TeamsPage (m64.11) — the AgentTeam orchestration rosters (read-only).

function team(over: Partial<AgentTeamSummary> = {}): AgentTeamSummary {
  return {
    name: "research",
    namespace: "default",
    registry: "research-team",
    supervisor: "planner",
    roster: [{ name: "researcher", agentRef: "web-researcher", description: "searches" }],
    members: ["planner", "web-researcher"],
    ready: true,
    budget: { maxFanOut: 4, maxSpawnDepth: 3, maxTotalSpawns: 20 },
    ...over,
  };
}

function installFetch(respond: () => { ok: boolean; status?: number; body?: unknown }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      const r = respond();
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? { items: [] },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/teams"]}>
      <Routes>
        <Route path="/teams" element={<TeamsPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("TeamsPage (m64.11)", () => {
  it("renders teams with supervisor, roster, budget, and a ready badge", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          team({ name: "research", supervisor: "planner", ready: true }),
          team({ name: "support", supervisor: "triage", ready: false, reason: "MemberNotFound" }),
        ],
      },
    }));

    renderPage();

    expect(await screen.findByTestId("teams-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Agent teams" })).toBeInTheDocument();
    expect(screen.getByText("research")).toBeInTheDocument();
    expect(screen.getByText("planner")).toBeInTheDocument();
    // The spawn budget renders as fan-out / depth / total.
    expect(screen.getAllByText("4 / 3 / 20").length).toBeGreaterThan(0);
    // Readiness badges: one ready, one with its NotReady reason.
    expect(screen.getByText("ready")).toBeInTheDocument();
    expect(screen.getByText("MemberNotFound")).toBeInTheDocument();
  });

  it("filters teams by name", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [team({ name: "research" }), team({ name: "support", supervisor: "triage" })] },
    }));
    renderPage();
    await screen.findByText("research");

    fireEvent.change(screen.getByLabelText("Filter list"), { target: { value: "sup" } });
    await waitFor(() => expect(screen.queryByText("research")).not.toBeInTheDocument());
    expect(screen.getByText("support")).toBeInTheDocument();
  });

  it("403 surfaces a forbidden state (never a fake empty list)", async () => {
    installFetch(() => ({ ok: false, status: 403, body: { error: "you do not have permission to list teams" } }));
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/you do not have permission to list teams/)).toBeInTheDocument(),
    );
    expect(screen.queryByText("No agent teams")).toBeNull();
  });

  it("empty → a teaching empty state", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("No agent teams")).toBeInTheDocument());
  });
});
