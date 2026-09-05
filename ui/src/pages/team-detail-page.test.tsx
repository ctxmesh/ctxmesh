import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { TeamDetailPage } from "@/pages/team-detail-page";
import type { AgentSummary, AgentTeamSummary, RunTree, RunTreeNode } from "@/lib/api";

// TeamDetailPage — the team as an outline (M151 §6.1 A3, §5.22).
//
// The page exists because the outline is the only one of the three candidate
// team drawings that is SIZE-BLIND, so these tests are weighted towards the two
// properties that claim is made of, plus the honesty rules that decide what may
// appear on it at all:
//
//   • it reads the same at three roster rows and at a thousand-run tree — the
//     big case is windowed, and the window never lies about the true size;
//   • a roster member that does not resolve is DRAWN, never dropped, and
//     "missing" is never confused with "we could not look";
//   • delegation traffic is unknown, with a reason — never a zero;
//   • the bounds that ARE measurable are measured, and the one that is not says
//     so rather than drawing a bar.

// ── fetch harness ────────────────────────────────────────────────────────────

interface Reply {
  status?: number;
  body?: unknown;
}

interface Backend {
  teams?: AgentTeamSummary[];
  teamsStatus?: number;
  agents?: AgentSummary[];
  agentsStatus?: number;
  agentsCursor?: string;
  runsStatus?: number;
  runs?: Array<{ traceId: string; agentNs?: string; agentName?: string }>;
  tree?: RunTree;
}

function installFetch(b: Backend) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      const reply = (): Reply => {
        if (url.startsWith("/api/teams")) {
          if (b.teamsStatus) return { status: b.teamsStatus, body: { error: "nope" } };
          return { body: { items: b.teams ?? [] } };
        }
        if (url.startsWith("/api/agents")) {
          if (b.agentsStatus) return { status: b.agentsStatus, body: { error: "nope" } };
          const items = b.agents ?? [];
          return { body: { agents: items, items, nextCursor: b.agentsCursor ?? "" } };
        }
        if (/\/api\/runs\/[^/]+\/tree$/.test(url)) {
          return { body: b.tree ?? { rootId: "r", nodes: [] } };
        }
        if (url.startsWith("/api/runs")) {
          if (b.runsStatus) return { status: b.runsStatus, body: { error: "nope" } };
          return { body: { runs: b.runs ?? [], nextCursor: "" } };
        }
        return { body: {} };
      };
      const r = reply();
      const status = r.status ?? 200;
      return Promise.resolve({
        ok: status < 400,
        status,
        json: async () => r.body ?? {},
        text: async () => JSON.stringify(r.body ?? {}),
      } as Response);
    }),
  );
}

// ── the world ────────────────────────────────────────────────────────────────

const TEAM: AgentTeamSummary = {
  name: "support-pod",
  namespace: "default",
  registry: "core-registry",
  supervisor: "lead",
  roster: [
    { name: "researcher", agentRef: "finder", description: "Finds the policy." },
    { name: "writer", agentRef: "drafter" },
    { name: "escalation", agentRef: "escalation-agent" },
  ],
  members: ["lead", "finder"],
  ready: false,
  reason: "MemberNotFound",
  budget: { maxFanOut: 4, maxSpawnDepth: 6, maxTotalSpawns: 20 },
};

function agent(name: string, over: Partial<AgentSummary> = {}): AgentSummary {
  return {
    name,
    namespace: "default",
    image: `ghcr.io/acme/${name}:1.0.0`,
    phase: "Ready",
    ready: true,
    ...over,
  };
}

const AGENTS: AgentSummary[] = [
  agent("lead"),
  agent("finder"),
  agent("drafter", {
    phase: "NotReady",
    ready: false,
    reason: "RevisionFailed",
    message: "Container exited 137.",
  }),
];

function node(
  id: string,
  parentRunId: string | undefined,
  status = "succeeded",
): RunTreeNode {
  return {
    id,
    // The AGENT is deliberately not the run id: the tree column shows the agent
    // and the Run column shows the id, and a fixture that made them equal could
    // not tell the two apart.
    agent: `default/agent-${id}`,
    status,
    ...(parentRunId ? { parentRunId } : {}),
    rootRunId: "root-1",
    input: `task ${id}`,
    createdAt: "2026-08-31T09:00:00Z",
    updatedAt: "2026-08-31T09:01:00Z",
  };
}

/** Root → two children, one of which is held; three levels overall. */
const SMALL_TREE: RunTree = {
  rootId: "root-1",
  nodes: [
    node("root-1", undefined, "running"),
    node("kid-a", "root-1"),
    node("kid-b", "root-1", "requires_action"),
    node("grand-a", "kid-a"),
  ],
};

/** Root with `width` children — the size-blind case. */
function wideTree(width: number): RunTree {
  const nodes: RunTreeNode[] = [node("root-1", undefined, "running")];
  for (let i = 0; i < width; i++) {
    nodes.push(node(`w-${String(i).padStart(3, "0")}`, "root-1", i === 7 ? "failed" : "succeeded"));
  }
  return { rootId: "root-1", nodes };
}

function renderPage(path = "/teams/default/support-pod") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/teams/:ns/:name" element={<TeamDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

const READY_BACKEND: Backend = {
  teams: [TEAM],
  agents: AGENTS,
  runs: [{ traceId: "trace-abc", agentNs: "default", agentName: "lead" }],
  tree: SMALL_TREE,
};

afterEach(() => vi.restoreAllMocks());

// ── The roster outline ───────────────────────────────────────────────────────

describe("TeamDetailPage — the roster outline", () => {
  it("draws the supervisor with its roster nested under it", async () => {
    installFetch(READY_BACKEND);
    renderPage();

    const grid = await screen.findByRole("treegrid", { name: "Team roster" });
    const sup = await screen.findByTestId("team-supervisor");
    expect(sup).toHaveTextContent("lead");
    // The supervisor is the root of the outline; the roster sits one level in.
    expect(sup.closest("tr")).toHaveAttribute("aria-level", "1");
    for (const role of ["researcher", "writer", "escalation"]) {
      const row = within(grid).getByTestId(`team-member-${role}`).closest("tr");
      expect(row).toHaveAttribute("aria-level", "2");
    }
    // Every roster row names the AGENT it summons, not just its role.
    expect(screen.getByTestId("team-member-researcher")).toHaveTextContent("finder");
  });

  it("joins each member's LIVE readiness from the agents list", async () => {
    installFetch(READY_BACKEND);
    renderPage();
    await screen.findByTestId("team-member-writer");

    const grid = screen.getByRole("treegrid", { name: "Team roster" });
    const writerRow = within(grid).getByTestId("team-member-writer").closest("tr")!;
    // drafter is NotReady/RevisionFailed → the crit status tag, and a next step.
    expect(within(writerRow).getByText("Not ready")).toBeInTheDocument();
    expect(screen.getByTestId("roster-next-writer")).toHaveTextContent("Open the failure");
  });

  it("DRAWS a roster member that does not resolve, instead of dropping it", async () => {
    installFetch(READY_BACKEND);
    renderPage();

    // escalation-agent is in the roster but not in the namespace's agent list.
    const row = (await screen.findByTestId("team-member-escalation")).closest("tr")!;
    expect(row).toHaveTextContent("escalation-agent");
    expect(within(row).getByText("no such agent")).toBeInTheDocument();
    const next = screen.getByTestId("roster-next-escalation");
    expect(next).toHaveTextContent("Create the agent");
    expect(next).toHaveAttribute("href", "/agents/new");
  });

  it("never calls a member missing when the agents list could not be read", async () => {
    installFetch({ ...READY_BACKEND, agentsStatus: 403 });
    renderPage();

    await screen.findByTestId("team-member-escalation");
    // "we could not look" is not "it is not there".
    expect(screen.queryByText("no such agent")).toBeNull();
    expect(screen.getAllByText("readiness unknown").length).toBe(4); // supervisor + 3 roles
    expect(
      screen.getByText(/Member readiness isn.t readable here/),
    ).toBeInTheDocument();
  });

  it("never claims a member is missing while the agent list is still being paged", async () => {
    // A non-empty cursor means the walk stopped early; absence proves nothing.
    installFetch({ ...READY_BACKEND, agents: [agent("lead")], agentsCursor: "more" });
    renderPage();
    await screen.findByTestId("team-member-researcher");
    expect(screen.queryByText("no such agent")).toBeNull();
    expect(screen.getAllByText("readiness unknown").length).toBe(3);
  });
});

// ── §7.1 honesty ─────────────────────────────────────────────────────────────

describe("TeamDetailPage — what it refuses to claim", () => {
  it("renders delegation traffic as unknown with a reason, never as zero", async () => {
    installFetch(READY_BACKEND);
    renderPage();
    const grid = await screen.findByRole("treegrid", { name: "Team roster" });

    const dashes = within(grid).getAllByTitle(
      "The teams API reports no delegation counts. Unknown — not zero.",
    );
    expect(dashes.length).toBe(4); // one per roster row
    for (const d of dashes) expect(d).toHaveTextContent("—");
    expect(within(grid).queryByText("0")).toBeNull();

    expect(
      screen.getByText(/Delegation traffic isn.t recorded per team/),
    ).toBeInTheDocument();
  });

  it("measures depth and total spawns against the budget, and leaves fan-out unknown", async () => {
    installFetch(READY_BACKEND);
    renderPage();
    const bounds = await screen.findByTestId("team-bounds");

    // SMALL_TREE is 4 runs: root + 3 sub-runs, deepest chain root→kid-a→grand-a.
    // The bounds strip paints before the run tree lands, so wait for the
    // MEASURED value rather than the first (honestly unknown) render.
    await waitFor(() =>
      expect(within(bounds).getByLabelText("spawn depth")).toHaveAttribute(
        "aria-valuenow",
        "2",
      ),
    );
    expect(within(bounds).getByLabelText("spawns in one run")).toHaveAttribute(
      "aria-valuenow",
      "3",
    );
    // Which spawns belonged to ONE supervisor step is not recorded, so the
    // fan-out ceiling gets no bar and no number.
    expect(within(bounds).queryByLabelText("fan-out per step")).toBeNull();
    expect(
      within(bounds).getByText(/Usage against this cap is not recorded/),
    ).toBeInTheDocument();
  });

  it("says the trace backend is absent rather than drawing an empty tree", async () => {
    installFetch({ ...READY_BACKEND, runsStatus: 501 });
    renderPage();
    expect(
      await screen.findByText("No trace backend is configured."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("treegrid", { name: "Delegation tree" })).toBeNull();
  });

  it("distinguishes 'no runs yet' from 'no backend'", async () => {
    installFetch({ ...READY_BACKEND, runs: [] });
    renderPage();
    expect(
      await screen.findByText("This supervisor has no recorded runs yet."),
    ).toBeInTheDocument();
  });

  it("a team that is not in the caller's list is a 404, not an empty detail", async () => {
    installFetch({ teams: [] });
    renderPage();
    expect(await screen.findByTestId("team-not-found")).toBeInTheDocument();
    expect(screen.queryByRole("treegrid")).toBeNull();
  });

  it("a 403 on teams renders the forbidden state, never a fake empty team", async () => {
    installFetch({ teamsStatus: 403 });
    renderPage();
    expect(
      await screen.findByText("You don't have permission to view agent teams"),
    ).toBeInTheDocument();
  });
});

// ── The size-blind contract ──────────────────────────────────────────────────

describe("TeamDetailPage — the delegation tree at both sizes", () => {
  it("at three sub-runs, everything that matters is already open", async () => {
    installFetch(READY_BACKEND);
    renderPage();
    const grid = await screen.findByRole("treegrid", { name: "Delegation tree" });
    // 4 nodes + the header row.
    expect(grid).toHaveAttribute("aria-rowcount", "5");
    // §4.5: the tree cell carries the agent NAME; the namespace it shares with
    // the team is not repeated on every row, it rides in `title`.
    expect(within(grid).getByText("agent-kid-b")).toBeInTheDocument();
    expect(within(grid).queryByText("default/agent-kid-b")).toBeNull();
    expect(within(grid).getByTitle("default/agent-kid-b")).toBeInTheDocument();
    // The held sub-run's next step is a hold, not a failure.
    expect(screen.getByTestId("run-next-kid-b")).toHaveTextContent("Review the hold");
  });

  it("at 400 sub-runs it summarises the remainder with the CALLER's count", async () => {
    installFetch({ ...READY_BACKEND, tree: wideTree(400) });
    renderPage();
    const grid = await screen.findByRole("treegrid", { name: "Delegation tree" });

    // The failing child is surfaced; the rest sit behind one honest summary.
    expect(within(grid).getByText("agent-w-007")).toBeInTheDocument();
    expect(within(grid).getByText("395 more, none need you")).toBeInTheDocument();
  });

  it("windows a 400-row tree without ever misreporting its size", async () => {
    installFetch({ ...READY_BACKEND, tree: wideTree(400) });
    renderPage();
    const grid = await screen.findByRole("treegrid", { name: "Delegation tree" });

    // findByText, not getByText: findByRole("treegrid") resolves as soon as the grid EXISTS,
    // which is before the 400 rows arrive, and the "Show all" affordance only appears once
    // there are enough rows to collapse. The synchronous getByText raced that and failed
    // intermittently under full-suite parallelism with `aria-rowcount="2"` — the header row
    // alone — which reads like a windowing bug and is really a missing await.
    fireEvent.click(await within(grid).findByText("Show all →"));

    await waitFor(() =>
      // 401 nodes + the header row — the TRUE total, always.
      expect(grid).toHaveAttribute("aria-rowcount", "402"),
    );
    // …while only a slice of them is in the DOM.
    const rendered = within(grid).getAllByRole("row").length;
    expect(rendered).toBeLessThan(120);
    expect(rendered).toBeGreaterThan(5);
  });

  it("stays walkable from the keyboard once windowed", async () => {
    installFetch({ ...READY_BACKEND, tree: wideTree(400) });
    renderPage();
    const grid = await screen.findByRole("treegrid", { name: "Delegation tree" });
    // findByText, not getByText: findByRole("treegrid") resolves as soon as the grid EXISTS,
    // which is before the 400 rows arrive, and the "Show all" affordance only appears once
    // there are enough rows to collapse. The synchronous getByText raced that and failed
    // intermittently under full-suite parallelism with `aria-rowcount="2"` — the header row
    // alone — which reads like a windowing bug and is really a missing await.
    fireEvent.click(await within(grid).findByText("Show all →"));
    await waitFor(() => expect(grid).toHaveAttribute("aria-rowcount", "402"));

    const first = within(grid).getAllByRole("row")[1]; // row 0 is the header
    first.focus();
    fireEvent.keyDown(first, { key: "End" });
    // End jumps to the last row, which windowing must have brought into the DOM.
    await waitFor(() => {
      const focused = document.activeElement as HTMLElement | null;
      expect(focused?.getAttribute("data-row-index")).toBe("400");
    });
  });

  it("opens on what needs a person and leaves the quiet branches behind a chevron", async () => {
    // A tree too big to open whole: a five-deep chain whose tail is held, plus
    // fifty quiet siblings. The page should walk the reader to the held run and
    // summarise everything that wants nothing.
    const nodes: RunTreeNode[] = [node("root-1", undefined, "running")];
    let parent = "root-1";
    for (let d = 1; d <= 5; d++) {
      const id = `deep-${d}`;
      nodes.push(node(id, parent, d === 5 ? "requires_action" : "succeeded"));
      parent = id;
    }
    for (let i = 0; i < 50; i++) {
      nodes.push(node(`quiet-${String(i).padStart(3, "0")}`, "root-1"));
    }
    installFetch({ ...READY_BACKEND, tree: { rootId: "root-1", nodes } });
    renderPage();

    const grid = await screen.findByRole("treegrid", { name: "Delegation tree" });
    // The path to the held run is open all five levels down…
    expect(within(grid).getByText("agent-deep-5")).toBeInTheDocument();
    expect(within(grid).getByText("agent-deep-5").closest("tr")).toHaveAttribute(
      "aria-level",
      "6",
    );
    // …and the fifty that want nothing are one honest line.
    expect(within(grid).getByText("46 more, none need you")).toBeInTheDocument();
  });
});
