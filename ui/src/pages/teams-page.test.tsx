import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { TeamsPage, teamsClosingLine, triageTeam } from "@/pages/teams-page";
import type { AgentTeamSummary } from "@/lib/api";

// TeamsPage — the AgentTeam index (M151 §6.1 A1, §4.4 "Teams index" budget).
//
// The page was rebuilt in M151: the old inline roster panel became a real
// detail page at /teams/:ns/:name, and the table became the §4.4 budget. The
// tests moved with it rather than being dropped — every behaviour the old
// panel suite pinned (the roster is reachable from a row click, a not-ready
// team says WHY, a 403 is not a fake empty list) is still asserted here, with
// the panel assertions rewritten as navigation ones, plus a new group covering
// the property the redesign added: the page never prints a traffic number the
// teams API cannot answer.

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

/** Renders the page with a probe on the detail route, so navigation is observable. */
function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="location">{loc.pathname}</div>;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/teams"]}>
      <Routes>
        <Route path="/teams" element={<TeamsPage />} />
        <Route path="/teams/:ns/:name" element={<LocationProbe />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("TeamsPage — the §4.4 teams-index budget", () => {
  it("renders each team's declared shape, agent count, state and next step", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          team({ name: "research", supervisor: "planner", ready: true }),
          team({
            name: "support",
            supervisor: "triage",
            ready: false,
            reason: "MemberNotFound",
            roster: [
              { name: "a", agentRef: "one" },
              { name: "b", agentRef: "two" },
            ],
          }),
        ],
      },
    }));

    renderPage();

    expect(await screen.findByTestId("teams-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Agent teams" })).toBeInTheDocument();
    expect(screen.getByText("research")).toBeInTheDocument();

    // Shape: the declared ladder — roster length + the three resolved ceilings.
    expect(screen.getByText(/1 supervisor → 1 role$/)).toBeInTheDocument();
    expect(screen.getByText(/1 supervisor → 2 roles$/)).toBeInTheDocument();
    expect(
      screen.getAllByText(/depth ≤ 3 · fan-out ≤ 4 · 20 spawns/).length,
    ).toBeGreaterThan(0);

    // Readiness, and the humanized cause line under it (M144.1 vocabulary).
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByTestId("team-reason-support")).toHaveTextContent(
      "Member not found",
    );
  });

  it("sorts what is blocking above what is not", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          team({ name: "aaa-ready", ready: true }),
          team({ name: "zzz-broken", ready: false, reason: "MemberNotFound" }),
        ],
      },
    }));
    renderPage();
    await screen.findByText("aaa-ready");

    const rows = screen.getAllByRole("row").slice(1); // drop the header row
    expect(rows[0]).toHaveTextContent("zzz-broken");
    expect(rows[1]).toHaveTextContent("aaa-ready");
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

// ── The roster is still one click away; it just lives on its own page now ────
// (These replace the m76.6 inline-panel suite one for one.)
describe("TeamsPage → team detail", () => {
  it("row-click navigates to the team's detail route", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [team({ name: "research", namespace: "team-a" })] },
    }));
    renderPage();
    const row = await screen.findByRole("row", { name: /research/ });

    fireEvent.click(row);
    expect(await screen.findByTestId("location")).toHaveTextContent(
      "/teams/team-a/research",
    );
  });

  it("the next step on a broken roster points at the team, in the crit tone", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [team({ name: "broken", ready: false, reason: "MemberNotFound" })],
      },
    }));
    renderPage();

    const next = await screen.findByTestId("next-step-broken");
    expect(next).toHaveTextContent("Fix the roster");
    expect(next).toHaveAttribute("href", "/teams/default/broken");
    expect(next.className).toContain("text-destructive");
  });

  it("a ready team asks nothing of a person", async () => {
    installFetch(() => ({ ok: true, body: { items: [team({ name: "fine", ready: true })] } }));
    renderPage();
    expect(await screen.findByTestId("next-step-fine")).toHaveTextContent(
      "Nothing needed",
    );
  });
});

// ── §7.1: the page may not print what the endpoint cannot answer ────────────
describe("TeamsPage honesty", () => {
  it("states once that delegation traffic is not in the team API, and draws no strip", async () => {
    installFetch(() => ({ ok: true, body: { items: [team()] } }));
    renderPage();
    await screen.findByText("research");

    const notes = screen.getAllByRole("note");
    expect(notes).toHaveLength(1);
    expect(notes[0].textContent).toMatch(/Live delegation traffic isn.t in the team API/);
    // No column for running / queued / held: those counts do not exist.
    expect(screen.queryByRole("columnheader", { name: /running/i })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: /held/i })).toBeNull();
  });

  it("counts DISTINCT declared agents, not roster entries", () => {
    // Two roles pointing at the same agent is legal, and it is two roles but
    // ONE agent. Counting entries would overstate the fleet.
    const t = triageTeam(
      team({
        supervisor: "planner",
        roster: [
          { name: "first", agentRef: "worker" },
          { name: "second", agentRef: "worker" },
        ],
      }),
    );
    expect(t.roles).toBe(2);
    expect(t.declaredAgents).toBe(2); // planner + worker
  });

  it("the closing line counts only the rows it can see", () => {
    const rows = [
      triageTeam(team({ name: "a", ready: true })),
      triageTeam(team({ name: "b", ready: false, reason: "MemberNotFound" })),
    ];
    expect(teamsClosingLine(rows)).toContain("1 of the 2 teams needs a person");
    expect(teamsClosingLine([])).toBeNull();
  });
});
