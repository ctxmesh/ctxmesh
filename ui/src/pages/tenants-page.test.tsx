import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { TenantsPage } from "@/pages/tenants-page";
import { ToastProvider } from "@/components/kit";

function installFetch(opts: {
  tenants?: { ok: boolean; status?: number; body: unknown };
  detail?: { ok: boolean; status?: number; body?: unknown };
  usage?: { ok: boolean; status?: number; body?: unknown };
  listUsage?: { ok: boolean; status?: number; body?: unknown };
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({ ok, status, json: async () => body } as Response);

      if (url === "/api/tenants" && method === "GET") {
        const r = opts.tenants ?? { ok: true, body: { items: [] } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 500));
      }
      // Batched near-cap usage (m54.5). Default 501 (no state-layer) so the column
      // hides unless a test opts in.
      if (url === "/api/tenants/usage" && method === "GET") {
        const r = opts.listUsage ?? { ok: false, status: 501 };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 501));
      }
      // Live usage (M49). Default 501 (no state-layer) so the panel hides the line unless a test opts in.
      if (url.match(/\/api\/tenants\/[^/]+\/usage$/) && method === "GET") {
        const r = opts.usage ?? { ok: false, status: 501 };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 501));
      }
      if (url.match(/\/api\/tenants\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? { ok: true, body: {} };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      return j({}, false, 404);
    }),
  );
}

function renderPage(path = "/tenants") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <TenantsPage />
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("TenantsPage", () => {
  it("lists tenants with their member count + status", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: {
          items: [
            { name: "alpha", namespaces: ["a1", "a2"], memberNamespaces: 2, ready: true },
            { name: "beta", namespaces: ["b1"], memberNamespaces: 1, ready: false },
          ],
        },
      },
    });
    renderPage();
    expect(await screen.findByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("beta")).toBeInTheDocument();
  });

  it("flags near-cap / at-cap tenants on the list (m54.5)", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: {
          items: [
            // near cap: 85 rpm of a 100 cap → "Near cap"
            { name: "warm", namespaces: ["w1"], memberNamespaces: 1, ready: true, model: { rpm: 100 } },
            // over cap: $120 spent of a $100 budget → "At cap"
            { name: "hot", namespaces: ["h1"], memberNamespaces: 1, ready: true, model: { budgetUSD: "100.00" } },
            // comfortable: 10 rpm of 100 → no badge
            { name: "cool", namespaces: ["c1"], memberNamespaces: 1, ready: true, model: { rpm: 100 } },
            // no caps → no badge (nothing to be near)
            { name: "uncapped", namespaces: ["u1"], memberNamespaces: 1, ready: true },
          ],
        },
      },
      listUsage: {
        ok: true,
        body: {
          items: [
            { name: "warm", spendUSD: 0, rpm: 85, inFlight: 0 },
            { name: "hot", spendUSD: 120, rpm: 0, inFlight: 0 },
            { name: "cool", spendUSD: 0, rpm: 10, inFlight: 0 },
            { name: "uncapped", spendUSD: 999, rpm: 999, inFlight: 9 },
          ],
        },
      },
    });
    renderPage();
    expect(await screen.findByTestId("tenant-nearcap-warm")).toHaveTextContent("Near cap");
    expect(screen.getByTestId("tenant-nearcap-hot")).toHaveTextContent("At cap");
    expect(screen.queryByTestId("tenant-nearcap-cool")).not.toBeInTheDocument();
    expect(screen.queryByTestId("tenant-nearcap-uncapped")).not.toBeInTheDocument();
  });

  it("the near-cap column hides gracefully when the state-layer is absent (501)", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: { items: [{ name: "alpha", namespaces: ["a1"], memberNamespaces: 1, ready: true, model: { rpm: 100 } }] },
      },
      // listUsage defaults to 501 → the column shows nothing, never errors.
    });
    renderPage();
    expect(await screen.findByText("alpha")).toBeInTheDocument();
    expect(screen.queryByTestId("tenant-nearcap-alpha")).not.toBeInTheDocument();
  });

  it("filters by namespace, not just tenant name (M47 — 'who owns X?')", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: {
          items: [
            { name: "alpha", namespaces: ["team-a", "shared"], memberNamespaces: 2, ready: true },
            { name: "beta", namespaces: ["team-b"], memberNamespaces: 1, ready: true },
          ],
        },
      },
    });
    renderPage();
    await screen.findByText("alpha");
    // Typing a NAMESPACE that only alpha owns narrows the list to alpha.
    fireEvent.change(screen.getByPlaceholderText(/name or namespace/i), {
      target: { value: "team-a" },
    });
    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.queryByText("beta")).not.toBeInTheDocument();
  });

  it("pre-fills the filter from ?q= so an agent→tenant namespace link lands filtered (m49.4)", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: {
          items: [
            { name: "alpha", namespaces: ["team-a", "shared"], memberNamespaces: 2, ready: true },
            { name: "beta", namespaces: ["team-b"], memberNamespaces: 1, ready: true },
          ],
        },
      },
    });
    renderPage("/tenants?q=team-a");
    // The owning tenant is already filtered in from the deep-link (no typing).
    expect(await screen.findByText("alpha")).toBeInTheDocument();
    expect(screen.queryByText("beta")).not.toBeInTheDocument();
  });

  it("opens a detail panel (namespaces + model caps) on row click", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: { items: [{ name: "alpha", namespaces: ["a1", "a2"], memberNamespaces: 2, ready: true }] },
      },
      detail: {
        ok: true,
        body: {
          name: "alpha",
          namespaces: ["a1", "a2"],
          model: { budgetUSD: "100.00", rpm: 600, maxConcurrent: 20 },
          memberNamespaces: 2,
          ready: true,
          conditions: [],
        },
      },
    });
    renderPage();
    const row = await screen.findByText("alpha");
    fireEvent.click(row.closest("tr") ?? row);
    // The detail fetch resolves into the panel (member namespaces + labelled model caps).
    expect(await screen.findByText("a1", {}, { timeout: 3000 })).toBeInTheDocument();
    expect(screen.getByText(/\$100\.00 budget/)).toBeInTheDocument();
  });

  it("shows live usage vs the caps when the state-layer is wired (M49)", async () => {
    installFetch({
      tenants: { ok: true, body: { items: [{ name: "alpha", namespaces: ["a1"], memberNamespaces: 1, ready: true }] } },
      detail: {
        ok: true,
        body: {
          name: "alpha",
          namespaces: ["a1"],
          model: { budgetUSD: "100.00", rpm: 600, maxConcurrent: 20 },
          memberNamespaces: 1,
          ready: true,
          conditions: [],
        },
      },
      usage: { ok: true, body: { spendUSD: 42.5, rpm: 120, inFlight: 3 } },
    });
    renderPage();
    const row = await screen.findByText("alpha");
    fireEvent.click(row.closest("tr") ?? row);
    const usage = await screen.findByTestId("tenant-usage", {}, { timeout: 3000 });
    expect(usage).toHaveTextContent(/\$42\.50 \/ \$100\.00 spent/);
    expect(usage).toHaveTextContent(/120 \/ 600 req\/min/);
    expect(usage).toHaveTextContent(/3 \/ 20 in-flight/);
  });

  it("hides the live-usage line when no state-layer is configured (501)", async () => {
    installFetch({
      tenants: { ok: true, body: { items: [{ name: "alpha", namespaces: ["a1"], memberNamespaces: 1, ready: true }] } },
      detail: {
        ok: true,
        body: {
          name: "alpha",
          namespaces: ["a1"],
          model: { budgetUSD: "100.00", rpm: 600, maxConcurrent: 20 },
          memberNamespaces: 1,
          ready: true,
          conditions: [],
        },
      },
      // usage omitted → default 501
    });
    renderPage();
    const row = await screen.findByText("alpha");
    fireEvent.click(row.closest("tr") ?? row);
    // The panel opens (model caps render) but the live-usage line stays hidden.
    expect(await screen.findByText(/\$100\.00 budget/)).toBeInTheDocument();
    expect(screen.queryByTestId("tenant-usage")).not.toBeInTheDocument();
  });

  it("teaches an empty state when there are no tenants", async () => {
    installFetch({ tenants: { ok: true, body: { items: [] } } });
    renderPage();
    expect(await screen.findByText("No tenants yet")).toBeInTheDocument();
  });

  it("surfaces a NamespaceConflict warning in the detail", async () => {
    installFetch({
      tenants: { ok: true, body: { items: [{ name: "alpha", namespaces: ["a1"], memberNamespaces: 1, ready: true }] } },
      detail: {
        ok: true,
        body: {
          name: "alpha",
          namespaces: ["a1"],
          memberNamespaces: 1,
          ready: true,
          conditions: [
            { type: "NamespaceConflict", status: "True", reason: "ClaimedByAnotherTenant", message: "skipped namespaces already owned by another tenant: [shared]" },
          ],
        },
      },
    });
    renderPage();
    const row = await screen.findByText("alpha");
    fireEvent.click(row.closest("tr") ?? row);
    expect(await screen.findByTestId("tenant-conflict")).toHaveTextContent(/already owned by another tenant/);
  });
});
