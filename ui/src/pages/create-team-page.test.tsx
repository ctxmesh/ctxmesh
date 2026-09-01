import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { CreateTeamPage } from "@/pages/create-team-page";
import { ToastProvider } from "@/components/kit";
import { NamespaceProvider } from "@/lib/namespace";

// CreateTeamPage tests (m71.7; re-pointed at the M151 wizard shape) — tier 0
// (no cluster, no LLM).
//
// The flow is now the kit Wizard: Describe → Review the roster. The forward
// control belongs to the kit, so it has no testid and the tests reach it by the
// accessible name a person reads ("Generate the roster" / "Create the team") —
// the same intent the old `generate-btn` / `create-btn` ids carried.
//
// data-testid contract (mirrors the page):
//   create-team-page, registry-select, team-description,
//   roster-review, team-supervisor, team-roster-entry-{n},
//   regenerate-hint, empty-registry-hint, team-yaml

// Minimal valid AgentTeam YAML returned by the fake model.
const validTeamYAML = `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: my-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: worker-a
      agentRef: worker-bot
      description: does the work
`;

// installFetch stubs global fetch and routes by URL+method.
interface FetchOpts {
  registries?: unknown;
  generateStatus?: number;
  generateBody?: unknown;
  createStatus?: number;
  createBody?: unknown;
  namespaces?: unknown;
}

function installFetch(opts: FetchOpts = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";

      const j = (json: unknown, ok: boolean, status: number) =>
        Promise.resolve({
          ok,
          status,
          json: async () => json,
          text: async () => JSON.stringify(json),
        } as Response);

      if (url.startsWith("/api/namespaces"))
        return j(opts.namespaces ?? { namespaces: [] }, true, 200);

      if (url.startsWith("/api/agentregistries") && method === "GET")
        return j(
          opts.registries ?? {
            items: [
              { name: "prod-registry", namespace: "default", registryId: "prod", roles: [], phase: "Ready", ready: true, memberSelector: { matchLabels: {} } },
            ],
            nextCursor: "",
          },
          true,
          200,
        );

      if (url === "/api/teams/generate" && method === "POST") {
        const status = opts.generateStatus ?? 200;
        const body = opts.generateBody ?? {
          teamYAML: validTeamYAML,
          model: "claude-sonnet-4-6",
          provider: "anthropic",
          warnings: [],
          eligibleMembers: ["orchestrator-bot", "worker-bot"],
        };
        return j(body, status < 300, status);
      }

      if (url === "/api/teams" && method === "POST") {
        const status = opts.createStatus ?? 201;
        const body = opts.createBody ?? {
          name: "my-team",
          namespace: "default",
          registry: "prod-registry",
          supervisor: "orchestrator-bot",
          roster: [{ name: "worker-a", agentRef: "worker-bot", description: "does the work" }],
          members: ["orchestrator-bot", "worker-bot"],
          ready: false,
          budget: { maxFanOut: 4, maxSpawnDepth: 3, maxTotalSpawns: 20 },
        };
        return j(body, status < 300, status);
      }

      return j({}, false, 404);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/teams/new"]}>
      <ToastProvider>
        <NamespaceProvider>
          <Routes>
            <Route path="/teams/new" element={<CreateTeamPage />} />
            <Route path="/teams" element={<div data-testid="teams-list">teams list</div>} />
          </Routes>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

/** The kit Wizard's forward control on the Describe step. */
function generateButton() {
  return screen.getByRole("button", { name: /Generate the roster/ });
}

/** The kit Wizard's Finish control on the Review step. */
function createButton() {
  return screen.getByRole("button", { name: /Create the team/ });
}

/**
 * The registry list arrives asynchronously and the forward control is gated on
 * a chosen registry, so every flow test waits for the picker to become the
 * loaded <select> before typing. (Before it loads the same testid is a
 * free-text input — deliberately, so a failed probe is never a dead end.)
 */
async function describeATeam(text: string) {
  await waitFor(() =>
    expect(screen.getByTestId("registry-select").tagName).toBe("SELECT"),
  );
  fireEvent.change(screen.getByTestId("team-description"), {
    target: { value: text },
  });
}

afterEach(() => vi.restoreAllMocks());

describe("CreateTeamPage (m71.7)", () => {
  it("renders the describe step with a registry selector, a description and the forward control", async () => {
    installFetch();
    renderPage();

    expect(await screen.findByTestId("create-team-page")).toBeInTheDocument();
    // Registry picker (an <input> until the list loads, then a <select>).
    expect(screen.getByTestId("registry-select")).toBeInTheDocument();
    expect(screen.getByTestId("team-description")).toBeInTheDocument();
    // The wizard's forward control names what it will do.
    expect(generateButton()).toBeInTheDocument();
  });

  it("calls generateTeam and renders the roster review on success", async () => {
    installFetch();
    renderPage();

    await describeATeam("An orchestrator with a researcher");
    fireEvent.click(generateButton());

    // The roster review step should appear.
    await waitFor(() =>
      expect(screen.getByTestId("roster-review")).toBeInTheDocument(),
    );

    // Supervisor should be rendered.
    expect(screen.getByTestId("team-supervisor")).toHaveTextContent("orchestrator-bot");

    // Roster entries.
    expect(screen.getByTestId("team-roster-entry-0")).toBeInTheDocument();

    // Create should be available.
    expect(createButton()).toBeInTheDocument();
  });

  it("marks the generated roster as a proposal, not a fact", async () => {
    // The honesty rule for A4: a model wrote this and nobody has confirmed it,
    // so the review says so — a `proposed` tag, the composing model named, and
    // a sentence stating nothing exists yet.
    installFetch();
    renderPage();

    await describeATeam("An orchestrator with a researcher");
    fireEvent.click(generateButton());

    const review = await screen.findByTestId("roster-review");
    expect(review).toHaveTextContent(/proposed/i);
    expect(review).toHaveTextContent(/does not exist yet/i);
    expect(review).toHaveTextContent(/claude-sonnet-4-6/);
  });

  it("an unreported eligible set reads as unknown, never as none", async () => {
    // A roster came back, so agents WERE eligible — an empty `eligibleMembers`
    // is the generator not saying, and must never render as an empty list.
    installFetch({
      generateBody: {
        teamYAML: validTeamYAML,
        model: "claude-sonnet-4-6",
        provider: "anthropic",
        warnings: [],
        eligibleMembers: [],
      },
    });
    renderPage();

    await describeATeam("An orchestrator with a researcher");
    fireEvent.click(generateButton());

    const review = await screen.findByTestId("roster-review");
    expect(review).toHaveTextContent(/not reported/i);
  });

  it("calls createTeam and shows success state on successful create", async () => {
    installFetch();
    renderPage();

    await describeATeam("An orchestrator team");
    fireEvent.click(generateButton());

    await waitFor(() => expect(createButton()).toBeInTheDocument());
    fireEvent.click(createButton());

    // The success panel appears immediately after the create resolves — proves
    // createTeam was called and returned the team summary. The 1200ms navigate
    // delay is not tested here (it is a setTimeout side effect; routing is covered
    // by the router integration in the app).
    await waitFor(() =>
      expect(screen.getByText(/redirecting to teams list/i)).toBeInTheDocument(),
    );
  });

  it("shows regenerate-hint on a 422 with regenerate:true", async () => {
    installFetch({
      generateStatus: 422,
      generateBody: {
        error: "the generated team spec was not valid",
        reason: "supervisor agentRef is not in the eligible agent set",
        teamYAML: "",
        regenerate: true,
      },
    });
    renderPage();

    await describeATeam("a team");
    fireEvent.click(generateButton());

    await waitFor(() =>
      expect(screen.getByTestId("regenerate-hint")).toBeInTheDocument(),
    );
    // Should NOT advance to the roster review on failure.
    expect(screen.queryByTestId("roster-review")).toBeNull();
    // The reason is shown, and the description the user typed is still there to
    // edit — the step is not swapped out from under them.
    expect(screen.getByTestId("regenerate-hint")).toHaveTextContent(
      /supervisor agentRef is not in the eligible agent set/,
    );
    expect(screen.getByTestId("team-description")).toHaveValue("a team");
  });

  it("shows empty-registry-hint when the registry has no eligible agents", async () => {
    installFetch({
      generateStatus: 422,
      generateBody: {
        error: "no eligible agents in this registry",
        reason: "the registry has no published members — create and publish agents before generating a team",
        teamYAML: "",
        regenerate: false,
      },
    });
    renderPage();

    await describeATeam("a team");
    fireEvent.click(generateButton());

    await waitFor(() =>
      expect(screen.getByTestId("regenerate-hint")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("empty-registry-hint")).toBeInTheDocument();
  });

  it("discloses the raw team.yaml in a code well, never an editable field", async () => {
    installFetch();
    renderPage();

    await describeATeam("An orchestrator with a researcher");
    fireEvent.click(generateButton());
    await screen.findByTestId("roster-review");

    fireEvent.click(screen.getByTestId("team-yaml-toggle"));
    const well = await screen.findByTestId("team-yaml");
    expect(well.tagName).toBe("PRE");
    expect(well).toHaveTextContent("AgentTeam");
  });
});
