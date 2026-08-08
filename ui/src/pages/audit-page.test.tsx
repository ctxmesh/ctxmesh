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

  it("a row's trace_id links to /traces/:id; a row without one shows no link", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          event({ id: 10, traceId: "trace-abc" }),
          event({ id: 11, traceId: undefined }),
        ],
        nextCursor: "",
      },
    }));

    renderPage();

    const link = await screen.findByTestId("audit-trace-link-10");
    expect(link).toHaveAttribute("href", "/traces/trace-abc");
    // A row with no trace has no "View run" link.
    expect(screen.queryByTestId("audit-trace-link-11")).not.toBeInTheDocument();
  });
});

// ── Filters → query params ────────────────────────────────────────────────────

describe("AuditPage — filters issue correct server-side params (m63.5)", () => {
  it("actor + action + kind filters send ?actor=&action=&kind=", async () => {
    const captured = installFetch(() => ({ ok: true, body: { items: [], nextCursor: "" } }));

    renderPage();
    await screen.findByTestId("audit-filter-bar");

    fireEvent.change(screen.getByLabelText("Filter by actor"), { target: { value: "alice" } });
    await waitFor(() => expect(captured.some((u) => u.includes("actor=alice"))).toBe(true));

    fireEvent.change(screen.getByLabelText("Filter by action"), {
      target: { value: "grant.create" },
    });
    await waitFor(() =>
      expect(captured.some((u) => u.includes("action=grant.create"))).toBe(true),
    );

    fireEvent.change(screen.getByLabelText("Filter by resource kind"), {
      target: { value: "MCPGrant" },
    });
    await waitFor(() => expect(captured.some((u) => u.includes("kind=MCPGrant"))).toBe(true));
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
