import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { RunsPage } from "@/pages/runs-page";
import type { RunSummary } from "@/lib/api";

// RunsPage (m16.8) — paginated + filterable global runs browser.
//
// Coverage:
//   • renders runs-table with rows; row-click navigates to /traces/:id
//   • agent filter issues correct query params
//   • from/to filters issue correct query params
//   • q filter issues correct query params
//   • q short-page: empty page + nextCursor → still shows "load more" (NOT bare "no results")
//   • cursor pagination: nextCursor drives the next page
//   • 502 surfaces as a visible error (NOT silent empty)
//   • 501 degrades calmly (runs-unavailable, no error toast)
//   • RBAC: viewer sees NO write actions column

function run(over: Partial<RunSummary> = {}): RunSummary {
  return {
    traceId: "t1",
    name: "billing-agent",
    timestamp: "2026-07-11T12:00:00Z",
    costUSD: 0.003,
    tokens: 860,
    latencyMs: 1840,
    ...over,
  };
}

// installFetch stubs global fetch. The `runs` responder receives the
// parsed URLSearchParams so tests can assert which params were sent.
function installFetch(
  runs: (qs: URLSearchParams) => { ok: boolean; status?: number; body?: unknown },
) {
  const captured: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = runs(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? { runs: [], nextCursor: "" },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <Routes>
        <Route path="/runs" element={<RunsPage />} />
        <Route path="/traces/:id" element={<div data-testid="trace-page-stub" />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("RunsPage — basic rendering (m16.8)", () => {
  it("renders runs-page and runs-table with row data", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        runs: [
          run({ traceId: "t1", name: "billing-agent" }),
          run({ traceId: "t2", name: "support-agent" }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();

    // Page root
    expect(await screen.findByTestId("runs-page")).toBeInTheDocument();
    // The DataTable gets aria-label="Runs"
    expect(screen.getByRole("table", { name: "Runs" })).toBeInTheDocument();
    // Row data rendered
    expect(screen.getByText("billing-agent")).toBeInTheDocument();
    expect(screen.getByText("support-agent")).toBeInTheDocument();
  });

  it("navigates to /traces/:traceId on row click", async () => {
    installFetch(() => ({
      ok: true,
      body: { runs: [run({ traceId: "abc123", name: "my-agent" })], nextCursor: "" },
    }));

    renderPage();

    const row = await screen.findByText("my-agent");
    fireEvent.click(row.closest("tr")!);

    await waitFor(() =>
      expect(screen.getByTestId("trace-page-stub")).toBeInTheDocument(),
    );
  });

  it("renders the filter bar with agent, from, to inputs (NO status filter)", async () => {
    installFetch(() => ({ ok: true, body: { runs: [], nextCursor: "" } }));
    renderPage();

    await screen.findByTestId("runs-filter-bar");
    expect(screen.getByLabelText("Filter by agent")).toBeInTheDocument();
    expect(screen.getByLabelText("Filter from date")).toBeInTheDocument();
    expect(screen.getByLabelText("Filter to date")).toBeInTheDocument();
    // No status filter — confirm absence of any status-related element
    expect(screen.queryByLabelText(/status/i)).toBeNull();
  });
});

// ── Filter → query params ─────────────────────────────────────────────────────

describe("RunsPage — filters issue correct query params (m16.8)", () => {
  it("agent filter sends ?agent=ns/name to the API", async () => {
    const captured = installFetch(() => ({
      ok: true,
      body: { runs: [], nextCursor: "" },
    }));

    renderPage();
    await screen.findByTestId("runs-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by agent"), {
      target: { value: "prod/billing-agent" },
    });

    await waitFor(() =>
      expect(captured.some((u) => u.includes("agent=prod%2Fbilling-agent"))).toBe(true),
    );
  });

  it("from filter sends ?from= to the API", async () => {
    const captured = installFetch(() => ({
      ok: true,
      body: { runs: [], nextCursor: "" },
    }));

    renderPage();
    await screen.findByTestId("runs-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter from date"), {
      target: { value: "2026-07-01T00:00" },
    });

    await waitFor(() =>
      expect(captured.some((u) => u.includes("from="))).toBe(true),
    );
    // m24 fix: the datetime-local value ("2026-07-01T00:00", no seconds/tz) must be
    // sent as RFC3339, not raw — the BFF 400s on a non-RFC3339 `from` ("Something
    // went wrong: from must be RFC3339"). The sent value carries seconds + a zone.
    const url = captured.find((u) => u.includes("from="))!;
    const sentFrom = decodeURIComponent(new URL(url, "http://x").searchParams.get("from")!);
    expect(sentFrom).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
    expect(sentFrom).not.toBe("2026-07-01T00:00");
  });

  it("to filter sends ?to= to the API", async () => {
    const captured = installFetch(() => ({
      ok: true,
      body: { runs: [], nextCursor: "" },
    }));

    renderPage();
    await screen.findByTestId("runs-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter to date"), {
      target: { value: "2026-07-11T23:59" },
    });

    await waitFor(() =>
      expect(captured.some((u) => u.includes("to="))).toBe(true),
    );
  });

  it("q filter sends ?q= to the API", async () => {
    const captured = installFetch(() => ({
      ok: true,
      body: { runs: [], nextCursor: "" },
    }));

    renderPage();
    await screen.findByTestId("runs-page");

    const filterInput = await screen.findByLabelText("Filter list");
    fireEvent.change(filterInput, { target: { value: "billing" } });

    await waitFor(() =>
      expect(captured.some((u) => u.includes("q=billing"))).toBe(true),
    );
  });
});

// ── q short-page handling (m16.3 carry-forward) ───────────────────────────────

describe("RunsPage — q short-page: nextCursor present → offer load more (m16.8)", () => {
  it("empty q-filtered page with nextCursor still offers Next, does NOT show bare 'no results'", async () => {
    // The server returns zero rows for this q but a non-empty nextCursor —
    // the matches may be on a later page (q is page-windowed). The UI MUST NOT
    // show a bare "no results" message; it must keep Next live.
    installFetch((qs) => {
      if (qs.get("q")) {
        // q filtered page — empty window but more pages
        return { ok: true, body: { runs: [], nextCursor: "c2" } };
      }
      return { ok: true, body: { runs: [run()], nextCursor: "c2" } };
    });

    renderPage();
    await screen.findByText("billing-agent");

    const filterInput = screen.getByLabelText("Filter list");
    fireEvent.change(filterInput, { target: { value: "zzz-no-match" } });

    // Should NOT show a bare "No runs yet" (the teaching empty state)
    await waitFor(() =>
      expect(screen.getByText(/No matches in this page/)).toBeInTheDocument(),
    );
    // Next page button must remain enabled (nextCursor is "c2")
    expect(screen.getByRole("button", { name: /Next page/ })).toBeEnabled();
    // Should NOT show a terminal "No runs yet"
    expect(screen.queryByText("No runs yet")).toBeNull();
  });
});

// ── Cursor pagination ──────────────────────────────────────────────────────────

describe("RunsPage — cursor pagination (m16.8)", () => {
  it("nextCursor drives the next page; Prev walks back", async () => {
    const captured = installFetch((qs) => {
      const cursor = qs.get("cursor") ?? "";
      if (!cursor) {
        return {
          ok: true,
          body: { runs: [run({ traceId: "p0", name: "page-zero" })], nextCursor: "c1" },
        };
      }
      return {
        ok: true,
        body: { runs: [run({ traceId: "p1", name: "page-one" })], nextCursor: "" },
      };
    });

    renderPage();
    expect(await screen.findByText("page-zero")).toBeInTheDocument();

    const next = screen.getByRole("button", { name: /Next page/ });
    const prev = screen.getByRole("button", { name: /Previous page/ });
    expect(next).toBeEnabled();
    expect(prev).toBeDisabled();

    fireEvent.click(next);
    expect(await screen.findByText("page-one")).toBeInTheDocument();
    // Confirm the 2nd fetch included cursor=c1
    expect(captured.some((u) => u.includes("cursor=c1"))).toBe(true);
    // On the last page Next is dead
    expect(screen.getByRole("button", { name: /Next page/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Previous page/ })).toBeEnabled();

    // Prev walks back
    fireEvent.click(screen.getByRole("button", { name: /Previous page/ }));
    expect(await screen.findByText("page-zero")).toBeInTheDocument();
  });
});

// ── Error handling ─────────────────────────────────────────────────────────────

describe("RunsPage — error and 501 states (m16.8)", () => {
  it("502 surfaces as a visible error (NOT a silent empty state)", async () => {
    installFetch(() => ({
      ok: false,
      status: 502,
      body: { error: "upstream Langfuse unreachable" },
    }));

    renderPage();

    // The DataTable's ErrorState should appear — NOT the teaching empty state.
    await waitFor(() =>
      expect(
        screen.getByText(/upstream Langfuse unreachable/),
      ).toBeInTheDocument(),
    );
    // The teaching "No runs yet" must NOT appear
    expect(screen.queryByText("No runs yet")).toBeNull();
  });

  it("502 offers a Retry button (not a forbidden state)", async () => {
    installFetch(() => ({
      ok: false,
      status: 502,
      body: { error: "upstream Langfuse unreachable" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/upstream Langfuse unreachable/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
  });

  it("501 degrades calmly to runs-unavailable (NOT an error toast)", async () => {
    installFetch(() => ({
      ok: false,
      status: 501,
      body: { error: "langfuse not configured" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByTestId("runs-unavailable")).toBeInTheDocument(),
    );
    // No error message "upstream" or generic error shown
    expect(screen.queryByText(/Retry/)).toBeNull();
  });
});

// ── RBAC: viewer sees no write actions ────────────────────────────────────────

describe("RunsPage — RBAC (m16.8)", () => {
  it("viewer sees no edit or delete buttons in the runs table", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        runs: [run({ traceId: "t1", name: "billing-agent" })],
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByText("billing-agent");

    // The RunsPage has no write affordances — no edit/delete buttons ever
    expect(screen.queryByRole("button", { name: /edit/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeNull();
  });
});
