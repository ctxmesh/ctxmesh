import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { StopsPage } from "@/pages/stops-page";
import type { ActiveStop, AuditEvent } from "@/lib/api";

// StopsPage (m151.7) — the scoped kill switch's landing surface (ADR 0126, spec §6.2 gap 2).
//
// The tests below are weighted towards the page's HONESTY properties rather than its layout,
// because those are the ones that fail quietly and dangerously:
//
//   • an impact count the backend never sent must render as unknown, never as 0;
//   • "nothing is stopped" and "this install has no kill store" are different truths and must not
//     share a surface;
//   • the lift confirmation must state that the held backlog runs AT ONCE (ADR 0126 §3) — an
//     operator who does not expect that is surprised at the worst possible moment;
//   • a failed lift must leave the operator knowing the stop is STILL IN FORCE.

// ── fetch harness ───────────────────────────────────────────────────────────

interface Reply {
  ok?: boolean;
  status?: number;
  body?: unknown;
}

interface Call {
  url: string;
  method: string;
  body?: unknown;
}

function installFetch(respond: (url: string, method: string) => Reply) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({
        url,
        method,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      const r = respond(url, method);
      const status = r.status ?? (r.ok === false ? 500 : 200);
      return Promise.resolve({
        ok: r.ok ?? status < 400,
        status,
        json: async () => r.body ?? [],
        text: async () => JSON.stringify(r.body ?? {}),
      } as Response);
    }),
  );
  return calls;
}

/** The default backend: two active stops, an audit trail with one kill row and one un-kill. */
const FLEET_STOP: ActiveStop = {
  scope: "fleet",
  level: "fleet",
  reason: "provider outage — holding everything until the route is healthy",
  principal: "oncall@acme.example",
};

const NS_STOP: ActiveStop = {
  scope: "namespace:team-b",
  level: "namespace",
  namespace: "team-b",
  reason: "runaway delegation loop",
  principal: "sre@acme.example",
};

const AGENT_STOP: ActiveStop = {
  scope: "agent:team-d:ingest-coordinator",
  level: "agent",
  namespace: "team-d",
  agent: "ingest-coordinator",
  reason: "spawn budget exhausted",
  principal: "unattributed",
};

function auditRow(over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: 1,
    occurredAt: "2026-08-31T16:35:02Z",
    source: "bff",
    actor: "sre@acme.example",
    actorKind: "user",
    action: "killswitch.kill",
    resourceKind: "KillScope",
    resourceName: "namespace:team-b",
    outcome: "success",
    ...over,
  };
}

function backend(
  opts: {
    stops?: ActiveStop[];
    audit?: Reply;
    lift?: Reply;
  } = {},
) {
  return (url: string, method: string): Reply => {
    if (url.startsWith("/api/kills")) return { body: opts.stops ?? [] };
    if (url.startsWith("/api/audit")) return opts.audit ?? { body: { items: [] } };
    if (url.startsWith("/api/kill/lift") && method === "POST") {
      return opts.lift ?? { body: { scope: "x", applied: true } };
    }
    return { body: [] };
  };
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/stops"]}>
      <StopsPage />
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ── what is stopped ─────────────────────────────────────────────────────────

describe("the in-force list", () => {
  it("lists every active stop, widest blast radius first", async () => {
    installFetch(backend({ stops: [AGENT_STOP, NS_STOP, FLEET_STOP] }));
    renderPage();

    const table = await screen.findByRole("table", { name: "Active stops" });
    const rows = within(table).getAllByRole("row").slice(1); // drop the header row
    expect(rows).toHaveLength(3);
    // A fleet stop holds more than a workspace stop, which holds more than one agent.
    expect(rows[0]).toHaveTextContent("everything");
    expect(rows[0]).toHaveTextContent("Fleet");
    expect(rows[1]).toHaveTextContent("team-b");
    expect(rows[1]).toHaveTextContent("Workspace");
    expect(rows[2]).toHaveTextContent("team-d/ingest-coordinator");
    expect(rows[2]).toHaveTextContent("Agent");
  });

  it("shows the reason, and reads the whole list cluster-wide", async () => {
    installFetch(backend({ stops: [NS_STOP] }));
    renderPage();
    expect(await screen.findByText(/runaway delegation loop/)).toBeInTheDocument();
    expect(screen.getByText(/cluster-wide/)).toBeInTheDocument();
  });

  it("renders 'unattributed' as an absence, not as a username", async () => {
    installFetch(backend({ stops: [AGENT_STOP] }));
    renderPage();
    const cell = await screen.findByTitle(/could not resolve who recorded this stop/i);
    expect(cell).toHaveTextContent("unattributed");
  });
});

// ── the honesty properties ──────────────────────────────────────────────────

describe("an impact count the backend never sent", () => {
  it("renders as unknown with a not-zero title — and never as 0", async () => {
    installFetch(backend({ stops: [NS_STOP] }));
    renderPage();

    const table = await screen.findByRole("table", { name: "Active stops" });
    const unknowns = within(table).getAllByTitle(
      /does not report this count\. It is unknown — not zero\./,
    );
    expect(unknowns.length).toBeGreaterThan(0);
    expect(unknowns[0]).toHaveTextContent("—");
    // The whole point: no cell in the table claims a measured zero.
    expect(within(table).queryByText("0")).toBeNull();
  });

  it("says why once, above the table, instead of once per row", async () => {
    installFetch(backend({ stops: [NS_STOP, FLEET_STOP] }));
    renderPage();
    const notes = await screen.findAllByText(/How much each stop is holding isn't reported\./);
    expect(notes).toHaveLength(1);
    expect(
      screen.getByText(/the counts are unknown, and unknown is not zero/i),
    ).toBeInTheDocument();
  });

  it("does not report a stop's start time when the audit trail cannot supply it", async () => {
    installFetch(backend({ stops: [NS_STOP], audit: { status: 501, body: {} } }));
    renderPage();
    const table = await screen.findByRole("table", { name: "Active stops" });
    expect(
      within(table).getAllByTitle(/unknown — not never/i).length,
    ).toBeGreaterThan(0);
  });

  it("joins the start time from the audit trail when it IS readable", async () => {
    installFetch(
      backend({
        stops: [NS_STOP],
        audit: { body: { items: [auditRow()] } },
      }),
    );
    renderPage();
    const table = await screen.findByRole("table", { name: "Active stops" });
    await waitFor(() =>
      expect(within(table).queryAllByTitle(/unknown — not never/i)).toHaveLength(0),
    );
  });
});

describe("nothing stopped vs. no kill store — two different truths", () => {
  it("reads as good news when the list is genuinely empty", async () => {
    installFetch(backend({ stops: [] }));
    renderPage();
    expect(await screen.findByText("Nothing is stopped.")).toBeInTheDocument();
    // An empty list is NOT evidence that the install has no kill store.
    expect(
      screen.queryByText(/The kill switch isn't configured on this install\./),
    ).toBeNull();
  });

  it("says the kill switch is unconfigured only when the backend says 501", async () => {
    installFetch(
      backend({ stops: [NS_STOP], lift: { status: 501, body: { error: "not configured" } } }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Lift the stop on team-b/ }));
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));

    expect(
      await screen.findByText(/The kill switch isn't configured on this install\./),
    ).toBeInTheDocument();
  });
});

// ── lifting ─────────────────────────────────────────────────────────────────

describe("lifting a stop", () => {
  it("confirms first, and states that the backlog runs at once", async () => {
    installFetch(backend({ stops: [NS_STOP] }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Lift the stop on team-b/ }));

    const dialog = screen.getByRole("alertdialog");
    expect(within(dialog).getByText("Lift the stop on team-b?")).toBeInTheDocument();
    expect(
      within(dialog).getByText(
        "Held runs start as soon as the stop is lifted — there is no staggered release, so expect the backlog to run at once.",
      ),
    ).toBeInTheDocument();
    // The mirror-image counts are unknown too — never a promised "0 runs will start".
    expect(within(dialog).getAllByText("not reported").length).toBeGreaterThan(0);
  });

  it("posts the exact scope the row names", async () => {
    const calls = installFetch(backend({ stops: [AGENT_STOP] }));
    renderPage();

    fireEvent.click(
      await screen.findByRole("button", { name: /Lift the stop on team-d\/ingest-coordinator/ }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));

    await waitFor(() => {
      const lift = calls.find((c) => c.url === "/api/kill/lift");
      expect(lift?.method).toBe("POST");
      expect(lift?.body).toEqual({
        level: "agent",
        namespace: "team-d",
        agent: "ingest-coordinator",
      });
    });
  });

  it("gates a fleet-wide lift behind the typed word, exactly as the stop was", async () => {
    const calls = installFetch(backend({ stops: [FLEET_STOP] }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Lift the stop on everything/ }));
    const confirm = screen.getByRole("button", { name: "Lift the stop" });
    expect(confirm).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: "everything" } });
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));

    await waitFor(() =>
      expect(calls.some((c) => c.url === "/api/kill/lift")).toBe(true),
    );
  });

  it("keeps the dialog open and says the stop is STILL IN FORCE when the lift is denied", async () => {
    installFetch(
      backend({
        stops: [NS_STOP],
        lift: { status: 403, body: { error: "forbidden: cannot lift a stop (the kill verb)" } },
      }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Lift the stop on team-b/ }));
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));

    expect(await screen.findByText(/it is still in force/i)).toBeInTheDocument();
    // Still open: an operator must not walk away believing the fleet is running.
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
  });

  it("reports applied:false as an honest no-op, not as a success", async () => {
    installFetch(
      backend({ stops: [NS_STOP], lift: { body: { scope: "namespace:team-b", applied: false } } }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Lift the stop on team-b/ }));
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));

    expect(
      await screen.findByText(/That scope was not stopped, so nothing was lifted\./),
    ).toBeInTheDocument();
  });
});

// ── the record, and the trail ───────────────────────────────────────────────

describe("the full record", () => {
  it("opens on row click so a dropped column is never a lost fact", async () => {
    installFetch(backend({ stops: [NS_STOP] }));
    renderPage();

    const table = await screen.findByRole("table", { name: "Active stops" });
    fireEvent.click(within(table).getAllByRole("row")[1]);

    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("namespace:team-b")).toBeInTheDocument();
    expect(within(drawer).getByText("Agents refusing new runs")).toBeInTheDocument();
    expect(within(drawer).getByText("Runs in flight")).toBeInTheDocument();
    // The honest limit ADR 0126 refuses to leave implied.
    expect(within(drawer).getByText(/pure local work is not interrupted/i)).toBeInTheDocument();
  });
});

describe("the lifted trail", () => {
  it("lists successful un-kills, and only those", async () => {
    installFetch(
      backend({
        stops: [],
        audit: {
          body: {
            items: [
              auditRow({ id: 2, action: "killswitch.unkill", resourceName: "namespace:team-a" }),
              auditRow({ id: 3, action: "killswitch.unkill", outcome: "denied", resourceName: "fleet" }),
              auditRow({ id: 4, action: "killswitch.kill", resourceName: "namespace:team-b" }),
            ],
          },
        },
      }),
    );
    renderPage();

    const table = await screen.findByRole("table", { name: "Recently lifted stops" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("namespace:team-a");
  });

  it("says the audit trail is unconfigured rather than showing an empty section", async () => {
    installFetch(backend({ stops: [], audit: { status: 501, body: {} } }));
    renderPage();
    expect(
      await screen.findByText(/The audit trail isn't configured on this install\./),
    ).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Recently lifted stops" })).toBeNull();
  });

  it("renders a forbidden trail without taking the live stop list down with it", async () => {
    installFetch(
      backend({ stops: [NS_STOP], audit: { status: 403, body: { error: "forbidden" } } }),
    );
    renderPage();

    expect(
      await screen.findByText(/permission to read the audit trail/i),
    ).toBeInTheDocument();
    // Panel isolation: the stops themselves still render.
    expect(screen.getByRole("table", { name: "Active stops" })).toBeInTheDocument();
  });
});

describe("when the stop list itself fails", () => {
  it("renders the forbidden state rather than a fake empty list", async () => {
    installFetch((url) =>
      url.startsWith("/api/kills")
        ? { status: 403, body: { error: "forbidden" } }
        : { body: { items: [] } },
    );
    renderPage();

    expect(await screen.findByText(/permission to view stops/i)).toBeInTheDocument();
    expect(screen.queryByText("Nothing is stopped.")).toBeNull();
  });
});
