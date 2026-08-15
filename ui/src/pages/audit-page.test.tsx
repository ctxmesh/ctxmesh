import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AuditPage } from "@/pages/audit-page";
import type { AuditEvent } from "@/lib/api";

// AuditPage (m63.5, ADR 0056 §4) — the compliance audit trail viewer.
//
// Coverage:
//   • renders audit-page + audit-table with rows (actor / action / outcome badge)
//   • the filter bar (actor / action / kind) issues correct server-side query params
//   • a row's trace_id links to /traces/:id; a row without one shows no link
//   • cursor pagination: nextCursor drives the next page (keyset)
//   • 501 degrades calmly to audit-unavailable (NOT an error) — store not configured
//   • 403 surfaces the forbidden state (NOT a fake empty list) — no operator persona
//   • 500 surfaces a visible, retryable error
//   m76.5 H1: action + kind are Select dropdowns; "Any" clears the filter
//   m76.5 H2: from/to datetime-local inputs send RFC3339 query params
//   m76.5 H3: row-click opens the detail drawer with the full detail map + trace link
//   m76.5 H4: requiresWrite is gone (nav.ts rename — verified in nav.test.tsx)

function event(over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: 1,
    occurredAt: "2026-08-08T12:00:00Z",
    source: "bff",
    actor: "alice",
    actorKind: "user",
    action: "connect",
    resourceKind: "Provider",
    resourceName: "anthropic-x1",
    namespace: "team-a",
    outcome: "success",
    ...over,
  };
}

// installFetch stubs global fetch; the responder receives the parsed query string
// so tests can assert which params were sent. Defaults to an empty page.
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
        json: async () => r.body ?? { items: [], nextCursor: "" },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/audit"]}>
      <Routes>
        <Route path="/audit" element={<AuditPage />} />
        <Route path="/traces/:id" element={<div data-testid="trace-page-stub" />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("AuditPage — basic rendering (m63.5)", () => {
  it("renders audit-page and audit-table with row data", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          event({ id: 1, actor: "alice", action: "connect" }),
          event({ id: 2, actor: "bob", action: "grant.revoke", outcome: "denied" }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();

    expect(await screen.findByTestId("audit-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Audit events" })).toBeInTheDocument();
    expect(screen.getByText("alice")).toBeInTheDocument();
    expect(screen.getByText("bob")).toBeInTheDocument();
    // The outcome renders as a badge — both success and denied are shown.
    expect(screen.getByText("success")).toBeInTheDocument();
    expect(screen.getByText("denied")).toBeInTheDocument();
  });

  it("renders the filter bar with actor, action, kind inputs", async () => {
    installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));
    renderPage();

    await screen.findByTestId("audit-filter-bar");
    expect(screen.getByLabelText("Filter by actor")).toBeInTheDocument();
    expect(screen.getByLabelText("Filter by action")).toBeInTheDocument();
    expect(screen.getByLabelText("Filter by resource kind")).toBeInTheDocument();
  });

  it("a row's trace_id is surfaced as a View-run link in the detail drawer (H3)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          event({ id: 10, actor: "alice", traceId: "trace-abc" }),
          event({ id: 11, actor: "bob", traceId: undefined }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByText("alice");

    // The trace link is now in the drawer footer — open the drawer first.
    fireEvent.click(screen.getByText("alice"));
    const link = await screen.findByTestId("audit-trace-link-10");
    expect(link).toHaveAttribute("href", "/traces/trace-abc");
    // Close the drawer by clicking the X.
    fireEvent.click(screen.getByRole("button", { name: /Close panel/ }));

    // The row with no trace has no "View run" link.
    expect(screen.queryByTestId("audit-trace-link-11")).not.toBeInTheDocument();
  });
});

// ── Filters → query params ────────────────────────────────────────────────────

describe("AuditPage — filters issue correct server-side params (m63.5)", () => {
  it("actor filter sends ?actor=", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by actor"), { target: { value: "alice" } });
    await waitFor(() => expect(captured.some((u) => u.includes("actor=alice"))).toBe(true));
  });
});

// ── H1: action + kind are Select dropdowns ────────────────────────────────────

describe("AuditPage — H1: action + kind Select dropdowns (m76.5)", () => {
  it("action Select sends ?action= when a value is chosen", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by action"), {
      target: { value: "grant.create" },
    });
    await waitFor(() =>
      expect(captured.some((u) => u.includes("action=grant.create"))).toBe(true),
    );
  });

  it("action Select 'Any' (value='') omits ?action= from the request", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    // First set a value, then clear it back to "Any".
    fireEvent.change(screen.getByLabelText("Filter by action"), {
      target: { value: "connect" },
    });
    await waitFor(() => expect(captured.some((u) => u.includes("action=connect"))).toBe(true));

    fireEvent.change(screen.getByLabelText("Filter by action"), {
      target: { value: "" },
    });
    await waitFor(() => {
      const last = captured[captured.length - 1];
      expect(last).not.toContain("action=");
    });
  });

  it("kind Select sends ?kind= when a value is chosen", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by resource kind"), {
      target: { value: "MCPGrant" },
    });
    await waitFor(() => expect(captured.some((u) => u.includes("kind=MCPGrant"))).toBe(true));
  });

  it("kind Select 'Any' (value='') omits ?kind= from the request", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by resource kind"), {
      target: { value: "Provider" },
    });
    await waitFor(() => expect(captured.some((u) => u.includes("kind=Provider"))).toBe(true));

    fireEvent.change(screen.getByLabelText("Filter by resource kind"), {
      target: { value: "" },
    });
    await waitFor(() => {
      const last = captured[captured.length - 1];
      expect(last).not.toContain("kind=");
    });
  });
});

// ── H2: date-range from/to inputs ─────────────────────────────────────────────

describe("AuditPage — H2: date-range from/to inputs (m76.5)", () => {
  it("from input sends ?from= as RFC3339", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    // datetime-local value; the component converts to RFC3339 (toRFC3339).
    fireEvent.change(screen.getByLabelText("Filter from date"), {
      target: { value: "2026-08-03T14:00" },
    });
    // The param will be URL-encoded RFC3339 — just check "from=" is present.
    await waitFor(() => expect(captured.some((u) => u.includes("from="))).toBe(true));
  });

  it("to input sends ?to= as RFC3339", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter to date"), {
      target: { value: "2026-08-03T16:00" },
    });
    await waitFor(() => expect(captured.some((u) => u.includes("to="))).toBe(true));
  });

  it("empty from/to inputs omit the params", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    // Wait for initial load with no date params.
    await waitFor(() => expect(captured.length).toBeGreaterThan(0));
    const first = captured[0];
    expect(first).not.toContain("from=");
    expect(first).not.toContain("to=");
  });
});

// ── H3: row-click detail drawer ───────────────────────────────────────────────

describe("AuditPage — H3: row-click detail drawer (m76.5)", () => {
  it("clicking a row opens the detail drawer with the full detail map", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          event({
            id: 42,
            actor: "alice",
            action: "grant.create",
            resourceKind: "MCPGrant",
            resourceName: "scalekit",
            namespace: "team-a",
            detail: { server: "scalekit", requestId: "req-1" },
          }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByText("alice");

    // Click the row — the table row is keyboard/click accessible via onRowClick.
    fireEvent.click(screen.getByText("alice"));

    // The detail drawer opens.
    const drawer = await screen.findByTestId("audit-detail-drawer");

    // The full detail map entries are rendered inside the drawer.
    expect(drawer).toHaveTextContent("server");
    expect(drawer).toHaveTextContent("scalekit");
    expect(drawer).toHaveTextContent("requestId");
    expect(drawer).toHaveTextContent("req-1");
  });

  it("the drawer shows the View run link when traceId is present", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          event({ id: 10, actor: "bob", traceId: "trace-xyz", detail: {} }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByText("bob");
    fireEvent.click(screen.getByText("bob"));

    await screen.findByTestId("audit-detail-drawer");
    const link = screen.getByTestId("audit-trace-link-10");
    expect(link).toHaveAttribute("href", "/traces/trace-xyz");
  });

  it("the drawer has no View run link when traceId is absent", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [event({ id: 20, actor: "carol", traceId: undefined, detail: {} })],
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByText("carol");
    fireEvent.click(screen.getByText("carol"));

    await screen.findByTestId("audit-detail-drawer");
    expect(screen.queryByTestId("audit-trace-link-20")).not.toBeInTheDocument();
  });
});

// ── Cursor pagination ──────────────────────────────────────────────────────────

describe("AuditPage — keyset cursor pagination (m63.5)", () => {
  it("nextCursor drives the next page; Prev walks back", async () => {
    const captured = installFetch((qs) => {
      const cursor = qs.get("cursor") ?? "";
      if (!cursor) {
        return {
          ok: true,
          body: { items: [event({ id: 1, actor: "page-zero" })], nextCursor: "c1" },
        };
      }
      return {
        ok: true,
        body: { items: [event({ id: 2, actor: "page-one" })], nextCursor: "" },
      };
    });

    renderPage();
    expect(await screen.findByText("page-zero")).toBeInTheDocument();

    const next = screen.getByRole("button", { name: /Next page/ });
    expect(next).toBeEnabled();
    expect(screen.getByRole("button", { name: /Previous page/ })).toBeDisabled();

    fireEvent.click(next);
    expect(await screen.findByText("page-one")).toBeInTheDocument();
    expect(captured.some((u) => u.includes("cursor=c1"))).toBe(true);
    expect(screen.getByRole("button", { name: /Next page/ })).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: /Previous page/ }));
    expect(await screen.findByText("page-zero")).toBeInTheDocument();
  });
});

// ── Degrade discipline: 501 calm / 403 forbidden / 500 error ──────────────────

describe("AuditPage — 501 / 403 / 500 states (m63.5)", () => {
  it("501 degrades calmly to audit-unavailable (NOT an error)", async () => {
    installFetch(() => ({ ok: false, status: 501, body: { error: "audit not enabled" } }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByTestId("audit-unavailable")).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Retry/)).toBeNull();
  });

  it("403 surfaces the forbidden state, never a fake empty list", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you do not have permission to read the audit log" },
    }));

    renderPage();

    await waitFor(() =>
      expect(
        screen.getByText(/you do not have permission to read the audit log/),
      ).toBeInTheDocument(),
    );
    // A forbidden result is NOT the calm "not enabled" state and NOT the teaching empty.
    expect(screen.queryByTestId("audit-unavailable")).toBeNull();
    expect(screen.queryByText("No audit events")).toBeNull();
    // Forbidden is terminal — no Retry (re-fetching won't grant the persona).
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("500 surfaces a visible, retryable error", async () => {
    installFetch(() => ({ ok: false, status: 500, body: { error: "failed to read the audit log" } }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to read the audit log/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
    expect(screen.queryByTestId("audit-unavailable")).toBeNull();
  });
});

// ── P1-2: AUDIT_ACTIONS dropdown vocabulary matches Go source ────────────────

describe("AuditPage — P1-2 AUDIT_ACTIONS vocabulary (m76 close)", () => {
  it("action dropdown includes all real BFF+controller actions", async () => {
    installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));
    renderPage();
    await screen.findByTestId("audit-filter-bar");

    const select = screen.getByLabelText("Filter by action");
    // These are the real action strings the Go source writes (verified against
    // internal/bff/audit_events.go, shares.go, guardrail_event_handler.go,
    // internal/audit/audit.go). Each must appear as a <option> in the Select.
    const expected = [
      "connect",
      "grant.create",
      "grant.revoke",
      "share.create",
      "share.revoke",
      "guardrail.block",
      "create",
      "update",
      "delete",
    ];
    for (const action of expected) {
      expect(
        Array.from((select as HTMLSelectElement).options).some((o) => o.value === action),
        `expected action "${action}" to be an option`,
      ).toBe(true);
    }
  });

  it("action dropdown does NOT include connect.denied (denial is outcome=denied on action=connect)", async () => {
    installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));
    renderPage();
    await screen.findByTestId("audit-filter-bar");

    const select = screen.getByLabelText("Filter by action");
    expect(
      Array.from((select as HTMLSelectElement).options).some((o) => o.value === "connect.denied"),
      'action "connect.denied" must NOT be in the dropdown — it never exists as an action value',
    ).toBe(false);
  });
});
