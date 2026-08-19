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
    // The spawn budget renders with inline labels (fan-out / depth / total).
    expect(screen.getAllByText(/fan-out 4 · depth 3 · total\s*20/).length).toBeGreaterThan(0);
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
    expect(await screen.findByText("You don't have permission to view agent teams")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/you do not have permission to/)).toBeNull();
    expect(screen.queryByText("No agent teams")).toBeNull();
  });

  it("empty → a teaching empty state", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("No agent teams")).toBeInTheDocument());
  });
});

// ── I3 m76.6: inline detail panel (row-click) ──────────────────────────────
describe("TeamsPage detail panel (I3 m76.6)", () => {
  it("row-click opens the detail panel with roster members (name → agentRef · description)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          team({
            name: "research",
            supervisor: "planner",
            roster: [
              { name: "researcher", agentRef: "web-researcher", description: "searches the web" },
              { name: "writer", agentRef: "doc-writer" },
            ],
            ready: true,
          }),
        ],
      },
    }));

    renderPage();
    await screen.findByText("research");

    // Panel not visible yet.
    expect(screen.queryByTestId("team-detail")).toBeNull();

    // Click the row to open the panel.
    fireEvent.click(screen.getByText("research"));
    expect(await screen.findByTestId("team-detail")).toBeInTheDocument();

    // Member details: name → agentRef + description.
    const member = screen.getByTestId("team-member-researcher");
    expect(member).toHaveTextContent("researcher");
    expect(member).toHaveTextContent("web-researcher");
    expect(member).toHaveTextContent("searches the web");

    // Member without description renders agentRef but no description text.
    const writer = screen.getByTestId("team-member-writer");
    expect(writer).toHaveTextContent("writer");
    expect(writer).toHaveTextContent("doc-writer");
  });

  it("shows team-level readiness in the detail panel", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          team({ name: "broken", ready: false, reason: "MemberNotFound" }),
        ],
      },
    }));

    renderPage();
    await screen.findByText("broken");

    fireEvent.click(screen.getByText("broken"));
    await screen.findByTestId("team-detail");

    // H5: the badge is a status label ("Not ready"); the reason lives in the span (not duplicated).
    expect(screen.getByTestId("team-detail-notready-badge")).toHaveTextContent("Not ready");
    expect(screen.getByTestId("team-detail-notready-reason")).toHaveTextContent("MemberNotFound");
    // The reason must NOT also appear in the badge (the H5 double-render fix).
    expect(screen.getByTestId("team-detail-notready-badge")).not.toHaveTextContent("MemberNotFound");
  });

  it("clicking the same row again closes the panel (toggle)", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [team({ name: "research" })] },
    }));

    renderPage();
    // Wait for the table row to appear (inside the <table>).
    const row = await screen.findByRole("row", { name: /research/ });

    fireEvent.click(row);
    expect(await screen.findByTestId("team-detail")).toBeInTheDocument();

    // Click the row again to toggle the panel closed.
    fireEvent.click(row);
    await waitFor(() => expect(screen.queryByTestId("team-detail")).toBeNull());
  });

  it("Close button dismisses the detail panel", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [team({ name: "research" })] },
    }));

    renderPage();
    await screen.findByText("research");

    fireEvent.click(screen.getByText("research"));
    await screen.findByTestId("team-detail");

    fireEvent.click(screen.getByTestId("team-detail-close"));
    await waitFor(() => expect(screen.queryByTestId("team-detail")).toBeNull());
  });
});
