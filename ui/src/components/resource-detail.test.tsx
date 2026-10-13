import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ToastProvider } from "@/components/kit";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { AgentRegistryDetailPage } from "@/pages/agent-registry-detail-page";
import { ModelRouteDetailPage } from "@/pages/model-route-detail-page";
import { SecretBindingDetailPage } from "@/pages/secret-binding-detail-page";

// The three CRUD kinds render on one ResourceDetailPage. Each case below is one kind: the
// record it reads, every field it must show, and the testids and copy of its shell.

interface Case {
  kind: string;
  route: string;
  list: string;
  endpoint: string;
  resource: string;
  page: () => React.ReactElement;
  body: Record<string, unknown>;
  /** Text every field of the record puts on the page. */
  fields: string[];
  testids: { page: string; loading: string; error: string; notFound: string; edit: string; delete: string };
  plural: string;
  back: RegExp;
  confirmLabel: RegExp;
  /** A testid only the kind's own edit wizard renders. */
  wizard: string;
}

const CASES: Case[] = [
  {
    kind: "model route",
    route: "/routes/:ns/:name",
    list: "/routes",
    endpoint: "/api/modelroutes",
    resource: "modelroutes",
    page: () => <ModelRouteDetailPage />,
    body: {
      phase: "Ready",
      ready: true,
      rateLimit: { tenantRPM: 120 },
      providers: [
        { provider: "anthropic", model: "fallback-model", priority: 2, apiBase: "https://fallback.example" },
        { provider: "openai", model: "first-model", priority: 1, secretBindingRef: "oai-key" },
      ],
    },
    fields: ["first-model", "fallback-model", "oai-key", "https://fallback.example", "First choice", "Falls back to", "The tenant rate cap"],
    testids: {
      page: "route-detail-page",
      loading: "route-detail-loading",
      error: "route-detail-error",
      notFound: "route-not-found",
      edit: "edit-route-button",
      delete: "delete-route-button",
    },
    plural: "model routes",
    back: /Back to routes/,
    confirmLabel: /Delete route/,
    wizard: "provider-entry-0",
  },
  {
    kind: "secret binding",
    route: "/secrets/:ns/:name",
    list: "/secrets",
    endpoint: "/api/secretbindings",
    resource: "secretbindings",
    page: () => <SecretBindingDetailPage />,
    body: { backend: "kubernetes", secretRef: { name: "the-secret", key: "api-key" }, phase: "Resolved", ready: true },
    fields: ["the-secret", "api-key", "kubernetes", "never read here", "The credential value is never shown here."],
    testids: {
      page: "secret-detail-page",
      loading: "secret-detail-loading",
      error: "secret-detail-error",
      notFound: "secret-not-found",
      edit: "edit-secret-button",
      delete: "delete-secret-button",
    },
    plural: "secret bindings",
    back: /Back to bindings/,
    confirmLabel: /Delete binding/,
    wizard: "no-value-form-note",
  },
  {
    kind: "agent registry",
    route: "/registries/:ns/:name",
    list: "/registries",
    endpoint: "/api/agentregistries",
    resource: "agentregistries",
    page: () => <AgentRegistryDetailPage />,
    body: {
      registryId: "reg-001",
      memberSelector: { matchLabels: { team: "billing" } },
      guards: { maxDepth: 4, hopBudget: 9 },
      roles: ["planner", "executor"],
      status: { members: ["billing-agent"], phase: "Ready", ready: true },
    },
    fields: ["reg-001", "planner", "executor", "4", "9", "team=billing", "billing-agent"],
    testids: {
      page: "registry-detail-page",
      loading: "registry-detail-loading",
      error: "registry-detail-error",
      notFound: "registry-not-found",
      edit: "edit-registry-button",
      delete: "delete-registry-button",
    },
    plural: "agent registries",
    back: /Back to registries/,
    confirmLabel: /Delete registry/,
    wizard: "registry-id-readonly",
  },
];

type Reply = { status: number; body: unknown } | "hang";

function installFetch(c: Case, opts: { caps?: Record<string, boolean>; read?: () => Reply } = {}) {
  const calls: { url: string; method: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method });
      const reply = (status: number, body: unknown) =>
        Promise.resolve({
          ok: status < 400,
          status,
          json: async () => body,
          text: async () => JSON.stringify(body),
        } as Response);
      const path = url.split("?")[0];
      if (path === "/api/namespaces") return reply(200, { namespaces: [{ name: "team-b" }] });
      if (path === "/api/capabilities") {
        const allowed = opts.caps ?? { get: true, list: true, update: true, delete: true };
        return reply(200, { namespace: "team-b", allowed: { [c.resource]: allowed }, flows: {} });
      }
      if (path === `${c.endpoint}/team-b/item` && method === "GET") {
        const r = opts.read?.() ?? { status: 200, body: { name: "item", namespace: "team-b", ...c.body } };
        if (r === "hang") return new Promise<Response>(() => {});
        return reply(r.status, r.body);
      }
      if (path === `${c.endpoint}/team-b/item` && method === "DELETE") return reply(204, {});
      // Used-by sections, provider and binding pickers: an empty answer.
      return reply(200, { items: [], agents: [], routes: [], providers: [] });
    }),
  );
  return calls;
}

function renderAt(c: Case, search = "") {
  const prefix = c.route.split("/:")[0];
  return render(
    <MemoryRouter initialEntries={[`${prefix}/team-b/item${search}`]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path={c.route} element={c.page()} />
              <Route path={c.list} element={<div data-testid="the-list" />} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  localStorage.clear();
});

describe.each(CASES)("the $kind detail page", (c) => {
  it("renders every field of the record", async () => {
    installFetch(c);
    renderAt(c);
    const page = await screen.findByTestId(c.testids.page);
    for (const text of c.fields) {
      expect(await within(page).findAllByText(text, { exact: false }), text).not.toHaveLength(0);
    }
    expect(within(page).getByRole("heading", { level: 1, name: "item" })).toBeInTheDocument();
    expect(within(page).getByRole("link", { name: c.list === "/registries" ? "Registries" : c.plural.replace(/^./, (m) => m.toUpperCase()) })).toHaveAttribute("href", c.list);
  });

  it("offers Edit and Delete to a caller who may update and delete", async () => {
    installFetch(c);
    renderAt(c);
    expect(await screen.findByTestId(c.testids.edit)).toBeInTheDocument();
    expect(screen.getByTestId(c.testids.delete)).toBeInTheDocument();
  });

  it("offers neither to a caller who may only read", async () => {
    installFetch(c, { caps: { get: true, list: true, update: false, delete: false } });
    renderAt(c);
    await screen.findByTestId(c.testids.page);
    await waitFor(() => expect(screen.queryByTestId(c.testids.edit)).toBeNull());
    expect(screen.queryByTestId(c.testids.delete)).toBeNull();
  });

  it("opens its own edit wizard from the Edit button", async () => {
    installFetch(c);
    renderAt(c);
    fireEvent.click(await screen.findByTestId(c.testids.edit));
    expect(await screen.findByTestId(c.wizard)).toBeInTheDocument();
  });

  it("deletes by typed name, then lands on the list", async () => {
    const calls = installFetch(c);
    renderAt(c, "?delete=1");
    fireEvent.change(await screen.findByPlaceholderText("item"), { target: { value: "item" } });
    fireEvent.click(screen.getByRole("button", { name: c.confirmLabel }));
    expect(await screen.findByTestId("the-list")).toBeInTheDocument();
    expect(calls.some((x) => x.method === "DELETE" && x.url === `${c.endpoint}/team-b/item`)).toBe(true);
  });

  it("shows its loading state until the record arrives", async () => {
    installFetch(c, { read: () => "hang" });
    renderAt(c);
    expect(await screen.findByTestId(c.testids.loading)).toBeInTheDocument();
  });

  it("answers a 404 as an absence with the way back to its list", async () => {
    installFetch(c, { read: () => ({ status: 404, body: { error: "not found" } }) });
    renderAt(c);
    expect(await screen.findByTestId(c.testids.notFound)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("link", { name: c.back })).toHaveAttribute("href", c.list);
  });

  it("answers a 403 with the permission boundary named for its kind", async () => {
    installFetch(c, { read: () => ({ status: 403, body: { error: "forbidden" } }) });
    renderAt(c);
    expect(await screen.findByText("You don't have permission to view item.")).toBeInTheDocument();
    expect(screen.getByText(`Ask an admin for a role that can read ${c.plural}.`)).toBeInTheDocument();
    expect(screen.queryByTestId(c.testids.error)).toBeNull();
  });

  it("answers any other failure with a Retry that reads again", async () => {
    let fail = true;
    const calls = installFetch(c, {
      read: () => (fail ? { status: 500, body: { error: "boom" } } : { status: 200, body: { name: "item", namespace: "team-b", ...c.body } }),
    });
    renderAt(c);
    expect(await screen.findByTestId(c.testids.error)).toBeInTheDocument();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByTestId(c.testids.page)).toBeInTheDocument();
    expect(calls.filter((x) => x.method === "GET" && x.url === `${c.endpoint}/team-b/item`)).toHaveLength(2);
  });
});
