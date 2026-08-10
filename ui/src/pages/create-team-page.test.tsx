import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { CreateTeamPage } from "@/pages/create-team-page";
import { ToastProvider } from "@/components/kit";
import { NamespaceProvider } from "@/lib/namespace";

// CreateTeamPage tests (m71.7) — tier 0 (no cluster, no LLM).
//
// data-testid contract (mirrors the page):
//   create-team-page, registry-select, team-description,
//   generate-btn, roster-review, team-supervisor, team-roster-entry-{n},
//   create-btn, regenerate-hint, empty-registry-hint

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

afterEach(() => vi.restoreAllMocks());

describe("CreateTeamPage (m71.7)", () => {
  it("renders the describe form with a registry selector and description textarea", async () => {
    installFetch();
    renderPage();

    expect(await screen.findByTestId("create-team-page")).toBeInTheDocument();
    // Registry select should appear (either a <select> or an <input>)
    expect(screen.getByTestId("registry-select")).toBeInTheDocument();
    expect(screen.getByTestId("team-description")).toBeInTheDocument();
    expect(screen.getByTestId("generate-btn")).toBeInTheDocument();
  });

  it("calls generateTeam and renders the roster review on success", async () => {
    installFetch();
    renderPage();

    // Wait for registry to load
    await screen.findByTestId("registry-select");

    fireEvent.change(screen.getByTestId("team-description"), {
      target: { value: "An orchestrator with a researcher" },
    });
    fireEvent.click(screen.getByTestId("generate-btn"));

    // The roster review should appear
    await waitFor(() =>
      expect(screen.getByTestId("roster-review")).toBeInTheDocument(),
    );

    // Supervisor should be rendered
    expect(screen.getByTestId("team-supervisor")).toHaveTextContent("orchestrator-bot");

    // Roster entries
    expect(screen.getByTestId("team-roster-entry-0")).toBeInTheDocument();

    // Create button should be available
    expect(screen.getByTestId("create-btn")).toBeInTheDocument();
  });

  it("calls createTeam and shows success state on successful create", async () => {
    installFetch();
    renderPage();

    await screen.findByTestId("registry-select");
    fireEvent.change(screen.getByTestId("team-description"), {
      target: { value: "An orchestrator team" },
    });
    fireEvent.click(screen.getByTestId("generate-btn"));

    await waitFor(() => expect(screen.getByTestId("create-btn")).toBeInTheDocument());
    fireEvent.click(screen.getByTestId("create-btn"));

    // The success banner appears immediately after the create resolves — proves
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

    await screen.findByTestId("registry-select");
    fireEvent.change(screen.getByTestId("team-description"), {
      target: { value: "a team" },
    });
    fireEvent.click(screen.getByTestId("generate-btn"));

    await waitFor(() =>
      expect(screen.getByTestId("regenerate-hint")).toBeInTheDocument(),
    );
    // Should NOT show roster review on failure
    expect(screen.queryByTestId("roster-review")).toBeNull();
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

    await screen.findByTestId("registry-select");
    fireEvent.change(screen.getByTestId("team-description"), {
      target: { value: "a team" },
    });
    fireEvent.click(screen.getByTestId("generate-btn"));

    await waitFor(() =>
      expect(screen.getByTestId("regenerate-hint")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("empty-registry-hint")).toBeInTheDocument();
  });
});
