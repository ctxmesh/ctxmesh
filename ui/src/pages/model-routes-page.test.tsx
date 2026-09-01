import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ModelRoutesPage } from "@/pages/model-routes-page";
import { ModelRouteDetailPage, NewModelRoutePage } from "@/pages/model-route-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Helpers --------------------------------------------------------------------

function installFetch(opts: {
  caps?: Record<string, Record<string, boolean>>;
  routes?: (qs: URLSearchParams) => { ok: boolean; status?: number; body: unknown };
  detail?: { ok: boolean; status?: number; body?: unknown };
  update?: { ok: boolean; status?: number; body?: unknown };
  remove?: { ok: boolean; status?: number };
  create?: { ok: boolean; status?: number; body?: unknown };
}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({ ok, status, json: async () => body } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({
          namespace: "",
          allowed: opts.caps ?? {
            modelroutes: { create: true, update: true, delete: true },
          },
        });
      // List
      if (url.startsWith("/api/modelroutes") && method === "GET" && !url.includes("/default/")) {
        const qs = new URLSearchParams(url.split("?")[1] ?? "");
        const r = opts.routes?.(qs) ?? { ok: true, body: { items: [], nextCursor: "" } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 500));
      }
      // Create
      if (url === "/api/modelroutes" && method === "POST") {
        const r = opts.create ?? { ok: true, body: { name: "new-route", namespace: "default", providers: [], phase: "Pending", ready: false } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 422));
      }
      // Detail GET
      if (url.match(/\/api\/modelroutes\/[^/]+\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? {
          ok: true,
          body: { name: "gpt4", namespace: "default", providers: [{ provider: "openai", model: "gpt-4o", priority: 1, secretBindingRef: "oai-key", apiBase: "https://api.openai.com/v1" }], phase: "Ready", ready: true },
        };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      // Update
      if (url.match(/\/api\/modelroutes\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.update ?? { ok: true, body: { name: "gpt4", namespace: "default", providers: [], phase: "Ready", ready: true } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 422));
      }
      // Delete
      if (url.match(/\/api\/modelroutes\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.remove ?? { ok: true };
        return Promise.resolve({ ok: r.ok, status: r.status ?? (r.ok ? 204 : 403), json: async () => ({}) } as Response);
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderList(path = "/routes") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <Routes>
            <Route path="routes" element={<ModelRoutesPage />} />
          </Routes>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
}

function renderDetail(path = "/routes/default/gpt4") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="routes/:ns/:name" element={<ModelRouteDetailPage />} />
              <Route path="routes" element={<div>routes list</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function renderCreate() {
  return render(
    <MemoryRouter initialEntries={["/routes/new"]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="routes/new" element={<NewModelRoutePage />} />
              <Route path="routes/:ns/:name" element={<div>detail</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

// List tests -----------------------------------------------------------------

describe("ModelRoutesPage — list", () => {
  it("renders the teaching empty state when BFF returns no items", async () => {
    installFetch({ routes: () => ({ ok: true, body: { items: [], nextCursor: "" } }) });
    renderList();
    expect(await screen.findByText("No model routes yet")).toBeInTheDocument();
  });

  it("renders a route row from items", async () => {
    installFetch({
      routes: () => ({
        ok: true,
        body: {
          items: [{ name: "gpt4", namespace: "default", providers: [{ provider: "openai", model: "gpt-4o", priority: 1 }], phase: "Ready", ready: true }],
          nextCursor: "",
        },
      }),
    });
    renderList();
    expect(await screen.findByText("gpt4")).toBeInTheDocument();
    expect(screen.getByText("openai/gpt-4o")).toBeInTheDocument();
  });

  it("paginates: Next pushes cursor, Prev pops it", async () => {
    const calls = installFetch({
      routes: (qs) => {
        const cursor = qs.get("cursor") ?? "";
        if (!cursor) {
          return { ok: true, body: { items: [{ name: "route-0", namespace: "default", providers: [], phase: "Ready", ready: true }], nextCursor: "c1" } };
        }
        return { ok: true, body: { items: [{ name: "route-1", namespace: "default", providers: [], phase: "Ready", ready: true }], nextCursor: "" } };
      },
    });
    renderList();
    expect(await screen.findByText("route-0")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Next page/ }));
    expect(await screen.findByText("route-1")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("cursor=c1"))).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: /Previous page/ }));
    expect(await screen.findByText("route-0")).toBeInTheDocument();
  });

  it("renders 403 as the forbidden variant, not empty list", async () => {
    installFetch({ routes: () => ({ ok: false, status: 403, body: { error: "forbidden: cannot list modelroutes" } }) });
    renderList();
    expect(await screen.findByText("You don't have permission to view model routes")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/forbidden: cannot/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });
});

// RBAC tests -----------------------------------------------------------------

describe("ModelRoutesPage — RBAC-aware row actions", () => {
  const oneRoute = () => ({
    ok: true,
    body: {
      items: [{ name: "gpt4", namespace: "default", providers: [], phase: "Ready", ready: true }],
      nextCursor: "",
    },
  });

  it("viewer sees NO edit or delete buttons", async () => {
    installFetch({ caps: { modelroutes: { create: false, update: false, delete: false } }, routes: () => oneRoute() });
    renderList();
    await screen.findByText("gpt4");
    expect(screen.queryByTestId("edit-gpt4")).toBeNull();
    expect(screen.queryByTestId("delete-gpt4")).toBeNull();
  });

  it("caller with update+delete sees both edit and delete", async () => {
    installFetch({ caps: { modelroutes: { create: true, update: true, delete: true } }, routes: () => oneRoute() });
    renderList();
    await screen.findByText("gpt4");
    expect(screen.getByTestId("edit-gpt4")).toBeInTheDocument();
    expect(screen.getByTestId("delete-gpt4")).toBeInTheDocument();
  });
});

// Detail tests ---------------------------------------------------------------

describe("ModelRouteDetailPage", () => {
  it("renders provider details including apiBase field", async () => {
    installFetch({});
    renderDetail();
    expect(await screen.findByTestId("route-detail-page")).toBeInTheDocument();
    expect(screen.getByText("openai")).toBeInTheDocument();
    expect(screen.getByText("gpt-4o")).toBeInTheDocument();
    // apiBase must be present and shown
    expect(screen.getByText("https://api.openai.com/v1")).toBeInTheDocument();
    expect(screen.getByText("oai-key")).toBeInTheDocument();
  });

  it("shows 404 empty state on not-found", async () => {
    installFetch({ detail: { ok: false, status: 404, body: { error: "not found" } } });
    renderDetail();
    expect(await screen.findByTestId("route-not-found")).toBeInTheDocument();
  });

  it("edit wizard submits correct body including apiBase", async () => {
    const calls = installFetch({
      update: { ok: true, body: { name: "gpt4", namespace: "default", providers: [{ provider: "openai", model: "gpt-4o", priority: 1, apiBase: "https://api.openai.com/v1" }], phase: "Ready", ready: true } },
    });
    renderDetail("/routes/default/gpt4?edit=1");
    // Wait for wizard to load
    expect(await screen.findByTestId("provider-entry-0")).toBeInTheDocument();
    // Advance past providers → rate-limit → review and submit
    // The Wizard "Next" button is labeled "Continue"
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => screen.getByTestId("rate-limit-rpm"));
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Review step
    await waitFor(() => screen.getByTestId("route-edit-review"));
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() => {
      const putCall = calls.find((c) => c.method === "PUT" && c.url.includes("/api/modelroutes/"));
      expect(putCall).toBeDefined();
      const body = JSON.parse(putCall!.body);
      // apiBase must be submitted
      expect(body.providers[0].apiBase).toBe("https://api.openai.com/v1");
    });
  });

  it("surfaces a 422 validation error in the form", async () => {
    installFetch({ update: { ok: false, status: 422, body: { error: "ModelRoute rejected: secretBindingRef required for non-mock provider" } } });
    renderDetail("/routes/default/gpt4?edit=1");
    await screen.findByTestId("provider-entry-0");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => screen.getByTestId("rate-limit-rpm"));
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => screen.getByTestId("route-edit-review"));
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    expect(await screen.findByTestId("route-edit-error")).toBeInTheDocument();
    expect(screen.getByTestId("route-edit-error").textContent).toMatch(
      /secretBindingRef required/,
    );
  });

  it("typed-name delete dialog calls DELETE on confirm", async () => {
    const calls = installFetch({ remove: { ok: true } });
    renderDetail("/routes/default/gpt4?delete=1");
    await screen.findByTestId("route-detail-page");
    // ConfirmDialog uses placeholder matching the confirmText for the input
    fireEvent.change(screen.getByPlaceholderText("gpt4"), { target: { value: "gpt4" } });
    fireEvent.click(screen.getByRole("button", { name: /Delete route/i }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === "DELETE" && c.url.includes("/api/modelroutes/default/gpt4"))).toBe(true);
    });
  });
});

// Create tests ---------------------------------------------------------------

describe("NewModelRoutePage", () => {
  it("create wizard submits correct body with apiBase and navigates", async () => {
    const calls = installFetch({
      create: { ok: true, body: { name: "my-route", namespace: "default", providers: [{ provider: "openai", model: "gpt-4o", priority: 1, apiBase: "https://api.openai.com/v1" }], phase: "Pending", ready: false } },
    });
    renderCreate();
    // Fill identity step
    fireEvent.change(screen.getByTestId("new-route-name"), { target: { value: "my-route" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Fill provider
    await waitFor(() => screen.getByTestId("new-provider-entry-0"));
    fireEvent.change(screen.getByTestId("new-provider-name-0"), { target: { value: "openai" } });
    fireEvent.change(screen.getByTestId("new-provider-model-0"), { target: { value: "gpt-4o" } });
    fireEvent.change(screen.getByTestId("new-provider-apibase-0"), { target: { value: "https://api.openai.com/v1" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // rate limit step
    await waitFor(() => screen.getByTestId("new-rate-limit-rpm"));
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // review
    await waitFor(() => screen.getByTestId("new-route-review"));
    fireEvent.click(screen.getByRole("button", { name: /Create route/i }));
    await waitFor(() => {
      const postCall = calls.find((c) => c.method === "POST" && c.url === "/api/modelroutes");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      expect(body.name).toBe("my-route");
      expect(body.providers[0].apiBase).toBe("https://api.openai.com/v1");
    });
  });
});


// ── Archetype A2 (M151 spec §6.1) — the detail surface ──────────────────────
// The redesign's contract: the providers read as a failover ORDER rather than a
// bag, an absent credential says so in words, the rate cap draws the bound it
// was given without inventing a usage figure, and a form that has diverged from
// the record says so before it is saved.

const ROUTE = (over: Record<string, unknown> = {}) => ({
  name: "gpt4",
  namespace: "default",
  phase: "Ready",
  ready: true,
  providers: [
    { provider: "openai", model: "gpt-4o", priority: 1, secretBindingRef: "oai-key" },
  ],
  ...over,
});

describe("ModelRouteDetailPage — archetype A2 (M151)", () => {
  it("renders the providers as a failover order, not in payload order", async () => {
    installFetch({
      detail: {
        ok: true,
        body: ROUTE({
          providers: [
            { provider: "openai", model: "gpt-4o", priority: 3, secretBindingRef: "oai-key" },
            { provider: "anthropic", model: "claude-opus-4", priority: 1, secretBindingRef: "anthropic" },
            // No binding and no apiBase — an absence the row must NAME.
            { provider: "vertex", model: "gemini-2.5-pro", priority: 2 },
          ],
        }),
      },
    });
    renderDetail();
    await screen.findByTestId("route-detail-page");

    const first = screen.getByTestId("provider-row-0");
    const second = screen.getByTestId("provider-row-1");
    const third = screen.getByTestId("provider-row-2");

    // Priority 1 leads, whatever order the array arrived in.
    expect(first).toHaveTextContent("First choice");
    expect(first).toHaveTextContent("anthropic");
    expect(second).toHaveTextContent("Falls back to");
    expect(second).toHaveTextContent("vertex");
    expect(third).toHaveTextContent("openai");

    // An unattached credential is a stated absence, never a blank cell.
    expect(second).toHaveTextContent("not attached");
    // A provider with no override says which endpoint it will actually use.
    expect(first).toHaveTextContent("the provider default");
  });

  it("teaches, rather than sits blank, when a route has no providers", async () => {
    installFetch({ detail: { ok: true, body: ROUTE({ providers: [] }) } });
    renderDetail();
    await screen.findByTestId("route-detail-page");
    expect(screen.getByText("This route has no providers yet.")).toBeInTheDocument();
    // Calm — an unconfigured route is not a failure.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws the rate cap but never a usage figure it was not given", async () => {
    installFetch({ detail: { ok: true, body: ROUTE({ rateLimit: { tenantRPM: 600 } }) } });
    renderDetail();
    await screen.findByTestId("route-detail-page");

    // The bound is real and shown; the usage against it is absent, and the
    // meter says so in words rather than drawing a fill at zero.
    expect(screen.getByText("600/min")).toBeInTheDocument();
    expect(screen.getByText(/not recorded for this install/)).toBeInTheDocument();
  });

  it("says an uncapped route is uncapped, rather than showing a zero cap", async () => {
    installFetch({ detail: { ok: true, body: ROUTE() } });
    renderDetail();
    await screen.findByTestId("route-detail-page");
    expect(screen.getByText("not capped")).toBeInTheDocument();
    expect(screen.queryByText("0")).toBeNull();
  });

  it("makes an unsaved edit visibly unsaved — and un-marks one put back", async () => {
    installFetch({ detail: { ok: true, body: ROUTE() } });
    renderDetail("/routes/default/gpt4?edit=1");
    await screen.findByTestId("provider-entry-0");

    expect(screen.queryByTestId("route-edit-unsaved")).toBeNull();

    fireEvent.change(screen.getByTestId("provider-priority-0"), {
      target: { value: "5" },
    });
    expect(screen.getByTestId("route-edit-unsaved")).toBeInTheDocument();
    expect(screen.getByText("changed — not saved yet")).toBeInTheDocument();

    // Put back to what the record says: not a change.
    fireEvent.change(screen.getByTestId("provider-priority-0"), {
      target: { value: "1" },
    });
    expect(screen.queryByTestId("route-edit-unsaved")).toBeNull();
  });

  it("shows the 404 as an honest absence with a way back, not an error", async () => {
    installFetch({ detail: { ok: false, status: 404, body: { error: "not found" } } });
    renderDetail();
    expect(await screen.findByTestId("route-not-found")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("link", { name: /Back to routes/ })).toHaveAttribute(
      "href",
      "/routes",
    );
  });
});
