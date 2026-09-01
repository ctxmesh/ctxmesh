import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AlertsPage } from "@/pages/alerts-page";
import type { AlertSummary } from "@/lib/api";

// AlertsPage (m70.6, ADR 0063 D2) — the fired-alert console feed.
//
// Coverage:
//   • renders alerts-page with rows: policy / condition / type / Firing badge
//   • Resolved badge when resolvedAt is set and firing is false
//   • 501 degrades calmly to alerts-unavailable (NOT an error) — store not configured
//   • 403 surfaces the forbidden state (NOT a fake empty list)
//   • 500 surfaces a visible, retryable error
//   • empty state renders the "No alerts" message

function alert_(over: Partial<AlertSummary> = {}): AlertSummary {
  return {
    id: 1,
    namespace: "team-a",
    policy: "budget-policy",
    condition: "monthly-spend",
    type: "budgetSoft",
    value: "42.50",
    message: "cost exceeded threshold",
    firedAt: "2026-08-08T12:00:00Z",
    resolvedAt: null,
    firing: true,
    ...over,
  };
}

// installFetch stubs global fetch; the responder receives the parsed query string.
function installFetch(
  respond: (qs: URLSearchParams) => { ok: boolean; status?: number; body?: unknown },
) {
  const captured: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = respond(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? { items: [] },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/alerts"]}>
      <Routes>
        <Route path="/alerts" element={<AlertsPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("AlertsPage — basic rendering (m70.6)", () => {
  it("renders alerts-page and alerts-table with row data", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({ id: 1, policy: "budget-policy", condition: "monthly-spend", firing: true }),
          alert_({
            id: 2,
            policy: "regression-policy",
            condition: "serving-regression",
            type: "regressionDetected",
            resolvedAt: "2026-08-08T13:00:00Z",
            firing: false,
          }),
        ],
      },
    }));

    renderPage();

    expect(await screen.findByTestId("alerts-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Fired alerts" })).toBeInTheDocument();
    // The page root renders during loading, so awaiting it proves nothing about
    // the rows — await the row data itself.
    expect(await screen.findByText("budget-policy")).toBeInTheDocument();
    expect(screen.getByText("regression-policy")).toBeInTheDocument();
  });

  it("shows Firing badge for an active alert and Resolved badge for a cleared one", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({ id: 1, firing: true, resolvedAt: null }),
          alert_({ id: 2, firing: false, resolvedAt: "2026-08-08T13:00:00Z" }),
        ],
      },
    }));

    renderPage();

    await screen.findAllByText("Firing");
    // The status-column badges render inside <td> elements (not as column headers).
    // Use getAllByText and check that at least one is a badge (not a th).
    const firingBadges = screen.getAllByText("Firing");
    expect(firingBadges.length).toBeGreaterThan(0);
    // "Resolved" also appears as a column header — check that the Badge variant
    // (rendered as a span inside the table body) is present.
    const resolvedEls = screen.getAllByText("Resolved");
    // At least two: one header and one badge. The badge is NOT inside a <th>.
    const resolvedBadge = resolvedEls.find((el) => el.closest("td") !== null);
    expect(resolvedBadge).toBeDefined();
  });
});

// ── Empty state ────────────────────────────────────────────────────────────────

describe("AlertsPage — empty state (m70.6)", () => {
  it("renders the No alerts empty state when items is empty", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));

    renderPage();

    await waitFor(() => expect(screen.getByText("No alerts")).toBeInTheDocument());
  });
});

// ── Degrade discipline: 501 calm / 403 forbidden / 500 error ──────────────────

describe("AlertsPage — 501 / 403 / 500 states (m70.6)", () => {
  it("501 degrades calmly to alerts-unavailable (NOT an error)", async () => {
    installFetch(() => ({ ok: false, status: 501, body: { error: "alert store not enabled" } }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByTestId("alerts-unavailable")).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Retry/)).toBeNull();
  });

  it("403 surfaces the forbidden state, never a fake empty list", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you do not have permission to read the alerts feed" },
    }));

    renderPage();

    expect(
      await screen.findByText("You don't have permission to view alerts"),
    ).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/you do not have permission to/)).toBeNull();
    expect(screen.queryByTestId("alerts-unavailable")).toBeNull();
    expect(screen.queryByText("No alerts")).toBeNull();
    // Forbidden is terminal — no Retry.
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("500 surfaces a visible, retryable error", async () => {
    installFetch(() => ({
      ok: false,
      status: 500,
      body: { error: "failed to read the alerts feed" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to read the alerts feed/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
    expect(screen.queryByTestId("alerts-unavailable")).toBeNull();
  });
});

// ── M151: the editorial ACTIVITY-FEED conversion ──────────────────────────────

describe("AlertsPage — activity feed (M151, §2.2 / §4.4 / §7.2)", () => {
  it("a firing alert carries a verb-first next step; a cleared one says Nothing needed", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({ id: 1, agent: "team-a/billing", firing: true, resolvedAt: null }),
          alert_({ id: 2, firing: false, resolvedAt: "2026-08-08T13:00:00Z" }),
        ],
      },
    }));

    renderPage();

    const step = await screen.findByTestId("next-step-1");
    expect(step).toHaveTextContent("Open the agent");
    expect(step).toHaveAttribute("href", "/agents/team-a/billing");

    const quiet = screen.getByTestId("next-step-2");
    expect(quiet).toHaveTextContent("Nothing needed");
    expect(quiet.tagName).not.toBe("A");
    expect(quiet.tagName).not.toBe("BUTTON");
  });

  it("a bound crossed while still serving reads warn (§2.2)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({
            id: 1,
            type: "budgetSoft",
            message: "82% of the monthly model budget is spent.",
            firing: true,
          }),
        ],
      },
    }));

    renderPage();
    const badge = (await screen.findAllByText("Firing"))[0];
    expect(badge.className).toContain("bg-warning-surface");
  });

  it("a firing alert whose words say work is being refused reads crit (§2.2)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({
            id: 1,
            type: "budgetHard",
            message: "Hard budget reached — new runs are being rejected.",
            firing: true,
          }),
        ],
      },
    }));

    renderPage();
    const badge = (await screen.findAllByText("Firing"))[0];
    expect(badge.className).toContain("bg-destructive-surface");
  });

  it("sorts newest-first — a feed is chronological, not triaged", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({ id: 1, firedAt: "2026-08-08T10:00:00Z", firing: true, resolvedAt: null }),
          alert_({
            id: 2,
            firedAt: "2026-08-08T12:00:00Z",
            firing: false,
            resolvedAt: "2026-08-08T12:30:00Z",
          }),
        ],
      },
    }));

    renderPage();
    await screen.findByTestId("next-step-2");

    const order = screen
      .getAllByTestId(/^next-step-/)
      .map((el) => el.getAttribute("data-testid"));
    // The newer, already-cleared alert leads the older one that is still firing.
    expect(order).toEqual(["next-step-2", "next-step-1"]);
  });

  it("the Needs you chip narrows the window to the alerts still firing", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          alert_({ id: 1, policy: "still-firing", firing: true, resolvedAt: null }),
          alert_({
            id: 2,
            policy: "already-cleared",
            firing: false,
            resolvedAt: "2026-08-08T13:00:00Z",
          }),
        ],
      },
    }));

    renderPage();
    await screen.findByText("still-firing");

    fireEvent.click(screen.getByRole("radio", { name: /Needs you/ }));
    expect(screen.getByText("still-firing")).toBeInTheDocument();
    expect(screen.queryByText("already-cleared")).toBeNull();
  });

  it("501 is a calm note — not an error, not a retry", async () => {
    installFetch(() => ({ ok: false, status: 501, body: { error: "alert store not enabled" } }));

    renderPage();

    const wrapper = await screen.findByTestId("alerts-unavailable");
    // QuietNote's role is `note` — deliberately never `alert` or `status`.
    expect(wrapper.querySelector('[role="note"]')).not.toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
    expect(screen.queryByText("No alerts")).toBeNull();
  });
});
