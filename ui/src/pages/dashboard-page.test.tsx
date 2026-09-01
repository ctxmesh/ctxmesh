import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import {
  DashboardPage,
  attentionRows,
  bounds,
  census,
  fleetSentence,
  greeting,
  stages,
} from "@/pages/dashboard-page";
import { UNKNOWN, isKnown } from "@/components/kit";
import type { AgentSummary } from "@/lib/api";

// Home (route "/", archetype A11) — the page a person judges the product by.
//
// The suite is organised around the one property that matters most here: the
// page may never claim more than a backend answered. So most of these tests are
// about what is ABSENT — a windowed count that is not printed, a 501 that
// degrades calmly, a 403 that takes one panel rather than the page.
//
// Carried forward from the old dashboard suite, as the same intent:
//   • renders its panels from mocked BFF data (was: topology + runs + cost)
//   • a 501 on an optional backend degrades CALMLY, never "Failed to load …"
//   • each summary panel links to the surface that owns it in full
//     (was: the topology panel's link to /topology, the runs panel's to /runs —
//     neither panel survives §6.1 A11, so the promise is asserted on the panels
//     Home actually carries)
//   • no Langfuse iframe, no "Cost by model" chart
//   • cost is tenant-scoped (ADR 0077): the page points at /cost and — the
//     stronger form of the same assertion — never calls a tenant-less /api/cost
//   • the first-run checklist behaviours (DX-4), unchanged

interface Routes {
  [path: string]: unknown;
}

/** Paths Home fetches that a test did not care to stub. A quiet cluster. */
const DEFAULTS: Routes = {
  "/api/kills": [],
  "/api/agents": { agents: [], items: [], nextCursor: "" },
  "/api/namespaces": { namespaces: [{ name: "team-a" }] },
  "/api/approvals": [],
  "/api/tenants": { items: [] },
  "/api/tenants/usage": { items: [] },
  "/api/alerts": { items: [] },
  "/api/providers": { providers: [] },
  "/api/runs": { runs: [] },
};

const fetchedPaths: string[] = [];

/**
 * A URL-routed fetch mock. Home fans out over eight endpoints, so every test
 * starts from a complete, quiet cluster and overrides only the feed it is
 * about — otherwise an unstubbed 404 would silently become the thing under test.
 */
function routeFetch(routes: Routes = {}, statuses: Record<string, number> = {}) {
  const table = { ...DEFAULTS, ...routes };
  fetchedPaths.length = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      fetchedPaths.push(url);
      const status = statuses[path];
      if (status !== undefined) {
        return Promise.resolve({
          ok: false,
          status,
          json: async () => ({ error: `stubbed ${status}` }),
        } as Response);
      }
      const body = table[path];
      if (body === undefined) {
        return Promise.resolve({
          ok: false,
          status: 404,
          json: async () => ({}),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => body,
      } as Response);
    }),
  );
}

function agent(over: Partial<AgentSummary> = {}): AgentSummary {
  return {
    name: "demo-assistant",
    namespace: "team-a",
    image: "ghcr.io/acme/demo:1",
    phase: "Ready",
    ready: true,
    ...over,
  };
}

const FLEET: AgentSummary[] = [
  agent({ name: "demo-assistant" }),
  agent({ name: "support-triage", ready: false, phase: "Suspended", reason: "Suspended" }),
  agent({ name: "billing-agent", ready: false, phase: "Failed", reason: "RevisionFailed" }),
  agent({ name: "onboarding-bot", ready: false, phase: "Draft", isDraft: true }),
];

const agentsResponse = (items: AgentSummary[], nextCursor = "") => ({
  agents: items,
  items,
  nextCursor,
});

const providersConnected = {
  providers: [
    { provider: "anthropic", displayName: "Anthropic", models: [{ id: "claude-opus-4" }] },
  ],
};

const RAN = {
  runs: [
    {
      traceId: "t-abc",
      name: "checkout-flow",
      timestamp: "2026-07-01T00:00:00Z",
      costUSD: 0.5,
      tokens: 900,
      latencyMs: 120,
    },
  ],
};

const approvals = [
  {
    runId: "run-plan-0001",
    agent: "demo-supervisor",
    namespace: "team-a",
    kind: "plan_approval",
    message: "Plan: 6 steps across 4 agents, estimated 118k tokens and $0.34.",
    waitingSince: "2026-08-31T08:59:12Z",
  },
  {
    runId: "run-step-0002",
    agent: "demo-assistant",
    namespace: "team-a",
    kind: "approval",
    message: "Approve create_refund for charge ch_2 (€4,412.00)?",
    waitingSince: "2026-08-31T09:41:44Z",
  },
];

/** A tenant with a declared cap, and live usage sitting past the 80% tick. */
const tenants = {
  items: [
    {
      name: "acme-core",
      namespaces: ["team-a"],
      memberNamespaces: 1,
      ready: true,
      model: { budgetUSD: "500.00" },
    },
  ],
};
const tenantUsage = { items: [{ name: "acme-core", spendUSD: 460, rpm: 12, inFlight: 1 }] };

function renderHome() {
  return render(
    <MemoryRouter>
      <DashboardPage />
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ─────────────────────────────────────────────────────────────────────────────
// The pure helpers — the page's authority rules, tested without a DOM
// ─────────────────────────────────────────────────────────────────────────────

describe("greeting", () => {
  it("names the part of the day at each boundary", () => {
    expect(greeting(new Date(2026, 8, 1, 0, 0))).toBe("Good morning");
    expect(greeting(new Date(2026, 8, 1, 11, 59))).toBe("Good morning");
    expect(greeting(new Date(2026, 8, 1, 12, 0))).toBe("Good afternoon");
    expect(greeting(new Date(2026, 8, 1, 17, 59))).toBe("Good afternoon");
    expect(greeting(new Date(2026, 8, 1, 18, 0))).toBe("Good evening");
    expect(greeting(new Date(2026, 8, 1, 23, 59))).toBe("Good evening");
  });
});

describe("census", () => {
  it("puts every agent in exactly one bucket, so the buckets sum to the fleet", () => {
    const c = census(FLEET, true);
    expect(c.total).toBe(4);
    expect(c.halted + c.failing + c.held + c.draft + c.serving + c.comingUp).toBe(4);
    expect(c.halted).toBe(1);
    expect(c.failing).toBe(1);
    expect(c.draft).toBe(1);
    expect(c.serving).toBe(1);
  });

  it("carries the window's completeness, which is the whole authority story", () => {
    expect(census(FLEET, false).complete).toBe(false);
  });
});

describe("stages", () => {
  it("states a fact per stage when the window IS the fleet", () => {
    const cells = stages(census(FLEET, true), 2);
    expect(cells.map((s) => s.name)).toEqual(["Build", "Govern", "Ship", "Improve"]);
    expect(cells.every((s) => s.fact !== undefined)).toBe(true);
  });

  it("omits every fleet fact when the window is one page — never a partial count", () => {
    const cells = stages(census(FLEET, false), 2);
    for (const cell of cells.slice(0, 3)) expect(cell.fact).toBeUndefined();
    // No stage may be lit either: a lit stage is a position claim.
    expect(cells.some((s) => s.active)).toBe(false);
  });

  it("leaves Improve unanswered when the alert store did not answer", () => {
    expect(stages(census(FLEET, true), undefined)[3].fact).toBeUndefined();
  });

  it("never lights Improve — nothing on this page places an agent there", () => {
    expect(stages(census(FLEET, true), 0)[3].active).toBeFalsy();
  });
});

describe("attentionRows", () => {
  it("sorts halted above failing above unfinished, and leaves serving out", () => {
    const rows = attentionRows(FLEET);
    expect(rows.map((r) => r.word)).toEqual(["halted", "failing", "unfinished"]);
    expect(rows.map((r) => r.name)).not.toContain("demo-assistant");
  });

  it("gives the crit tone only to a stop or a failure", () => {
    const rows = attentionRows(FLEET);
    expect(rows[0].tone).toBe("crit");
    expect(rows[1].tone).toBe("crit");
    expect(rows[2].tone).toBe("default");
  });
});

describe("bounds", () => {
  it("reads a declared cap and leaves an undeclared one UNKNOWN, never zero", () => {
    const rows = bounds(
      [
        {
          name: "capped",
          namespaces: [],
          memberNamespaces: 0,
          ready: true,
          model: { budgetUSD: "500.00" },
        },
        { name: "uncapped", namespaces: [], memberNamespaces: 0, ready: true },
      ],
      [{ name: "capped", spendUSD: 10, rpm: 0, inFlight: 0 }],
    );
    const capped = rows.find((r) => r.tenant === "capped")!;
    const uncapped = rows.find((r) => r.tenant === "uncapped")!;
    expect(capped.cap).toBe(500);
    expect(capped.used).toBe(10);
    expect(isKnown(uncapped.cap)).toBe(false);
    expect(uncapped.used).toBe(UNKNOWN);
  });

  it("marks every row UNKNOWN when the usage backend did not answer at all", () => {
    const rows = bounds(
      [
        {
          name: "capped",
          namespaces: [],
          memberNamespaces: 0,
          ready: true,
          model: { budgetUSD: "500.00" },
        },
      ],
      null,
    );
    expect(rows[0].cap).toBe(500);
    expect(isKnown(rows[0].used)).toBe(false);
  });

  it("sorts the closest to its cap first", () => {
    const rows = bounds(
      [
        {
          name: "quiet",
          namespaces: [],
          memberNamespaces: 0,
          ready: true,
          model: { budgetUSD: "100.00" },
        },
        {
          name: "hot",
          namespaces: [],
          memberNamespaces: 0,
          ready: true,
          model: { budgetUSD: "100.00" },
        },
      ],
      [
        { name: "quiet", spendUSD: 1, rpm: 0, inFlight: 0 },
        { name: "hot", spendUSD: 95, rpm: 0, inFlight: 0 },
      ],
    );
    expect(rows[0].tenant).toBe("hot");
  });
});

describe("fleetSentence", () => {
  it("omits a clause whose backend did not answer, rather than estimating it", () => {
    const line = fleetSentence({ stopped: 1, serving: 193 });
    expect(line).toContain("1 scope is stopped");
    expect(line).not.toContain("waiting on a person");
    expect(line).toContain("The other 193 agents are serving");
  });

  it("says the all-clear only when the backends that answered all answered zero", () => {
    expect(
      fleetSentence({ waiting: 0, stopped: 0, near: 0, attention: 0, serving: 12 }),
    ).toBe(
      "Nothing is waiting on a person, nothing is stopped and nothing is failing. All 12 agents are serving.",
    );
  });

  // M151 hardening B1: the all-clear used to be a fixed sentence naming all
  // three categories, so a console REFUSED the stop list still reassured the
  // operator that nothing was stopped.
  it("drops the stops clause from the all-clear when the stop list never answered", () => {
    expect(fleetSentence({ waiting: 0, attention: 0, serving: 12 })).toBe(
      "Nothing is waiting on a person and nothing is failing. All 12 agents are serving.",
    );
  });

  it("never claims an all-clear over a category no backend answered for", () => {
    // Only budgets answered, and budgets are not an all-clear category — so
    // there is no all-clear to give, only the serving count.
    expect(fleetSentence({ near: 0, serving: 4 })).toBe("All 4 agents are serving.");
    expect(fleetSentence({ near: 0 })).toBeNull();
  });

  it("renders nothing at all before any backend has answered", () => {
    expect(fleetSentence({})).toBeNull();
  });

  it("is grammatical at one and at many", () => {
    expect(fleetSentence({ waiting: 1 })).toBe("1 decision is waiting on a person.");
    expect(fleetSentence({ waiting: 3, attention: 2 })).toBe(
      "3 decisions are waiting on a person and 2 agents need looking at.",
    );
  });
});

// ─────────────────────────────────────────────────────────────────────────────
// The page
// ─────────────────────────────────────────────────────────────────────────────

describe("Home (render proof)", () => {
  it("renders the fleet strip, the waiting queue, the spending meters and the attention list from mocked BFF data", async () => {
    routeFetch({
      "/api/agents": agentsResponse(FLEET),
      "/api/approvals": approvals,
      "/api/tenants": tenants,
      "/api/tenants/usage": tenantUsage,
      "/api/providers": providersConnected,
      "/api/runs": RAN,
    });
    renderHome();

    // The sections A11 orders, each driven by its own backend.
    expect(await screen.findByRole("list", { name: "Fleet lifecycle" })).toBeInTheDocument();
    expect(await screen.findByTestId("home-waiting")).toBeInTheDocument();
    expect(await screen.findByTestId("home-spending")).toBeInTheDocument();
    expect(await screen.findByTestId("home-attention")).toBeInTheDocument();

    // The queue shows the ask itself.
    const waiting = screen.getByTestId("home-waiting");
    expect(within(waiting).getByText(/Approve create_refund/)).toBeInTheDocument();

    // Cost is tenant-scoped (ADR 0077): the retired by-model chart is still gone.
    expect(screen.queryByText("Cost by model")).toBeNull();

    // The fleet's blocked agents are named; the serving one is not.
    const attention = screen.getByTestId("home-attention");
    expect(within(attention).getByText("support-triage")).toBeInTheDocument();
    expect(within(attention).queryByText("demo-assistant")).toBeNull();
  });

  it("NEVER calls the tenant-less /api/cost, which is a guaranteed 400 (ADR 0077)", async () => {
    routeFetch({ "/api/agents": agentsResponse(FLEET), "/api/providers": providersConnected });
    renderHome();
    await screen.findByTestId("home-spending");
    expect(fetchedPaths.some((p) => p.includes("/api/cost"))).toBe(false);
  });

  it("counts the fleet only when the window IS the fleet (a cursor ⇒ no claim)", async () => {
    routeFetch({
      "/api/agents": agentsResponse(FLEET, "next-page-cursor"),
      "/api/providers": providersConnected,
    });
    renderHome();

    const strip = await screen.findByRole("list", { name: "Fleet lifecycle" });
    // Every fleet fact reads the §7.1 unknown copy rather than a windowed count.
    expect(within(strip).getAllByText("not yet known").length).toBeGreaterThan(0);
    expect(await screen.findByText(/first page of agents, not the fleet/i)).toBeInTheDocument();
  });

  it("degrades an unwired backend CALMLY (501), never as a red failure", async () => {
    // The alert store is the optional backend here — the same shape the old
    // dashboard tested with Langfuse: a 501 is "not part of this install", not
    // an error, and the rest of the page keeps working.
    routeFetch(
      { "/api/agents": agentsResponse(FLEET), "/api/providers": providersConnected },
      { "/api/alerts": 501 },
    );
    renderHome();

    expect(await screen.findByText(/Alerts aren’t configured/)).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load/)).toBeNull();
    // The panels that DID answer are untouched.
    expect(screen.getByTestId("home-waiting")).toBeInTheDocument();
  });

  it("draws a real cap with an empty track when live spend is unavailable — never a zero", async () => {
    routeFetch(
      {
        "/api/agents": agentsResponse(FLEET),
        "/api/tenants": tenants,
        "/api/providers": providersConnected,
      },
      { "/api/tenants/usage": 501 },
    );
    renderHome();

    const spending = await screen.findByTestId("home-spending");
    expect(within(spending).getByText(/tenant\/acme-core/)).toBeInTheDocument();
    expect(within(spending).getByText(/Live spend isn’t recorded/)).toBeInTheDocument();
    // No fabricated zero anywhere in the panel.
    expect(within(spending).queryByText("$0.00")).toBeNull();
  });

  it("collapses ONE panel to a forbidden state on a 403 — the page never fully 403s", async () => {
    routeFetch(
      { "/api/agents": agentsResponse(FLEET), "/api/providers": providersConnected },
      { "/api/tenants": 403 },
    );
    renderHome();

    expect(
      await screen.findByText("You don't have permission to view tenants"),
    ).toBeInTheDocument();
    // The rest of the page still rendered.
    expect(screen.getByRole("list", { name: "Fleet lifecycle" })).toBeInTheDocument();
    expect(screen.getByTestId("home-waiting")).toBeInTheDocument();
  });

  it("counts the workspaces that refused the queue instead of reading them as empty", async () => {
    routeFetch(
      {
        "/api/agents": agentsResponse(FLEET),
        "/api/namespaces": { namespaces: [{ name: "team-a" }, { name: "team-b" }] },
        "/api/providers": providersConnected,
      },
      { "/api/approvals": 403 },
    );
    renderHome();

    // Every workspace refused ⇒ the panel says "denied", never "nothing waiting".
    expect(
      await screen.findByText("You don't have permission to view approvals"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Nothing is waiting on a person\./)).toBeNull();
  });

  it("shows the stop in force before anything else, with the operator's reason", async () => {
    routeFetch({
      "/api/kills": [
        {
          scope: "ns:team-b",
          level: "namespace",
          namespace: "team-b",
          reason: "runaway delegation loop",
          principal: "oncall@acme.example",
        },
      ],
      "/api/agents": agentsResponse(FLEET),
      "/api/providers": providersConnected,
    });
    renderHome();

    const notice = await screen.findByTestId("home-stop-ns:team-b");
    expect(within(notice).getByText("team-b")).toBeInTheDocument();
    expect(within(notice).getByText(/runaway delegation loop/)).toBeInTheDocument();
    // The backend reports no impact counts, so the notice says unknown — not 0.
    expect(within(notice).getByText(/Impact/)).toBeInTheDocument();
  });

  it("summarises a tenant-level stop instead of misstating its reach", async () => {
    routeFetch({
      "/api/kills": [
        {
          scope: "tenant:acme-core",
          level: "tenant",
          tenant: "acme-core",
          reason: "billing incident",
          principal: "oncall@acme.example",
        },
      ],
      "/api/agents": agentsResponse(FLEET),
      "/api/providers": providersConnected,
    });
    renderHome();

    expect(await screen.findByText(/1 further stop is in force/)).toBeInTheDocument();
    expect(screen.queryByTestId("home-stop-tenant:acme-core")).toBeNull();
    // …and it is still counted in the lede.
    expect(screen.getByText(/1 scope is stopped/)).toBeInTheDocument();
  });

  it("says so plainly, and stays short, when nothing needs anyone", async () => {
    routeFetch({
      "/api/agents": agentsResponse([agent({ name: "a" }), agent({ name: "b" })]),
      "/api/providers": providersConnected,
      "/api/runs": RAN,
    });
    renderHome();

    expect(
      await screen.findByText(
        /Nothing is waiting on a person, nothing is stopped and nothing is failing/,
      ),
    ).toBeInTheDocument();
    // No "needs looking at" frame at all — an empty card is not an answer.
    await waitFor(() => expect(screen.queryByTestId("home-attention")).toBeNull());
    expect(screen.getByText(/Nothing needs a person\./)).toBeInTheDocument();
  });

  it("does NOT render any Langfuse embedded iframe (m16.11: the iframe stays demoted)", async () => {
    routeFetch({
      "/api/agents": agentsResponse(FLEET),
      "/api/providers": providersConnected,
    });
    renderHome();

    await screen.findByTestId("home-waiting");
    expect(document.querySelector("iframe")).toBeNull();
    expect(
      screen.queryByText(/Select a run to open its embedded Langfuse trace/),
    ).toBeNull();
    expect(screen.queryByText("Langfuse deep-view")).toBeNull();
  });

  it("links each panel to the surface that owns it in full", async () => {
    routeFetch({
      "/api/agents": agentsResponse(FLEET),
      "/api/approvals": approvals,
      "/api/tenants": tenants,
      "/api/tenants/usage": tenantUsage,
      "/api/alerts": {
        items: [
          {
            id: 411,
            namespace: "team-a",
            policy: "eu-ingest-budget",
            condition: "budgetSoft",
            type: "budgetSoft",
            value: "82%",
            message: "acme-core has consumed 82% of its monthly model budget.",
            firedAt: "2026-08-31T09:10:00Z",
            resolvedAt: null,
            firing: true,
          },
        ],
      },
      "/api/providers": providersConnected,
    });
    renderHome();

    const waiting = await screen.findByTestId("home-waiting");
    expect(within(waiting).getByText(/Open the queue/)).toHaveAttribute("href", "/approvals");

    const spending = screen.getByTestId("home-spending");
    expect(within(spending).getByText(/Open the cost page/)).toHaveAttribute("href", "/cost");

    const attention = screen.getByTestId("home-attention");
    expect(within(attention).getByText(/Open the fleet/)).toHaveAttribute("href", "/agents");

    const alerts = screen.getByTestId("home-alerts");
    expect(within(alerts).getByText(/Open the alerts/)).toHaveAttribute("href", "/alerts");

    // A queue row leads to the run that is actually asking.
    expect(screen.getByTestId("home-review-run-step-0002")).toHaveAttribute(
      "href",
      "/runs/run-step-0002",
    );
  });

  it("surfaces a fleet read failure as an error with a retry, not as an empty fleet", async () => {
    routeFetch({ "/api/providers": providersConnected }, { "/api/agents": 500 });
    renderHome();

    expect(await screen.findByText(/The fleet could not be read/)).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Fleet lifecycle" })).toBeNull();
  });
});

describe("Home — the first-run path (the aha entry point)", () => {
  it("renders the checklist when setup is incomplete (no providers)", async () => {
    routeFetch({ "/api/agents": agentsResponse(FLEET) });
    renderHome();

    expect(await screen.findByTestId("first-run-checklist")).toBeInTheDocument();
    expect(screen.getByTestId("first-run-cta")).toHaveTextContent(/Connect a provider/);
    expect(screen.getByTestId("first-run-step-0")).toBeInTheDocument();
  });

  it("still renders the checklist when runs is unavailable (minimal install, DX-4)", async () => {
    // On a minimal install observability is unwired → /api/runs 501. The
    // checklist must NOT vanish: a new user on an empty cluster needs it most.
    routeFetch({}, { "/api/runs": 501 });
    renderHome();

    expect(await screen.findByTestId("first-run-checklist")).toBeInTheDocument();
    expect(screen.getByTestId("first-run-cta")).toHaveTextContent(/Connect a provider/);
  });

  it("does NOT nag a set-up cluster whose runs feed is unavailable (DX-4 P3)", async () => {
    routeFetch(
      { "/api/agents": agentsResponse(FLEET), "/api/providers": providersConnected },
      { "/api/runs": 501 },
    );
    renderHome();

    await screen.findByTestId("home-waiting");
    await waitFor(() => expect(screen.queryByTestId("first-run-checklist")).toBeNull());
  });

  it("does NOT render the checklist when the setup is complete", async () => {
    routeFetch({
      "/api/agents": agentsResponse(FLEET),
      "/api/providers": providersConnected,
      "/api/runs": RAN,
    });
    renderHome();

    await screen.findByTestId("home-waiting");
    await waitFor(() => expect(screen.queryByTestId("first-run-checklist")).toBeNull());
  });

  it("does NOT render the checklist when the providers list fails to load (no false invitation)", async () => {
    routeFetch({ "/api/agents": agentsResponse(FLEET) }, { "/api/providers": 500 });
    renderHome();

    await screen.findByTestId("home-waiting");
    await waitFor(() => expect(screen.queryByTestId("first-run-checklist")).toBeNull());
  });

  it("IS the checklist on a genuinely empty install — not four empty frames", async () => {
    routeFetch({});
    renderHome();

    expect(await screen.findByTestId("first-run-checklist")).toBeInTheDocument();
    expect(screen.queryByTestId("home-waiting")).toBeNull();
    expect(screen.queryByTestId("home-spending")).toBeNull();
    expect(screen.queryByTestId("home-attention")).toBeNull();
  });
});
