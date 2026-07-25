import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { TenantsPage } from "@/pages/tenants-page";
import { ToastProvider } from "@/components/kit";

function installFetch(opts: {
  tenants?: { ok: boolean; status?: number; body: unknown };
  detail?: { ok: boolean; status?: number; body?: unknown };
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
      if (url.match(/\/api\/tenants\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? { ok: true, body: {} };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      return j({}, false, 404);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <TenantsPage />
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("TenantsPage", () => {
  it("lists tenants with their member count + status", async () => {
    installFetch({
      tenants: {
        ok: true,
        body: {
          items: [
            { name: "alpha", memberNamespaces: 2, ready: true },
            { name: "beta", memberNamespaces: 1, ready: false },
          ],
        },
      },
    });
    renderPage();
    expect(await screen.findByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("beta")).toBeInTheDocument();
  });

  it("opens a detail panel (namespaces + model caps) on row click", async () => {
    installFetch({
      tenants: { ok: true, body: { items: [{ name: "alpha", memberNamespaces: 2, ready: true }] } },
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
    fireEvent.click(await screen.findByText("alpha"));
    expect(await screen.findByTestId("tenant-detail")).toBeInTheDocument();
    expect(screen.getByText("a1")).toBeInTheDocument();
    expect(screen.getByText(/\$100\.00/)).toBeInTheDocument();
  });

  it("teaches an empty state when there are no tenants", async () => {
    installFetch({ tenants: { ok: true, body: { items: [] } } });
    renderPage();
    expect(await screen.findByText("No tenants yet")).toBeInTheDocument();
  });

  it("surfaces a NamespaceConflict warning in the detail", async () => {
    installFetch({
      tenants: { ok: true, body: { items: [{ name: "alpha", memberNamespaces: 1, ready: true }] } },
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
    fireEvent.click(await screen.findByText("alpha"));
    expect(await screen.findByTestId("tenant-conflict")).toHaveTextContent(/already owned by another tenant/);
  });
});
