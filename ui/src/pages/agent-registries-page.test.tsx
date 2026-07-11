import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AgentRegistriesPage } from "@/pages/agent-registries-page";
import { AgentRegistryDetailPage, NewAgentRegistryPage } from "@/pages/agent-registry-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Helpers --------------------------------------------------------------------

const DEFAULT_DETAIL = {
  name: "prod-registry",
  namespace: "default",
  registryId: "prod-reg-001",
  memberSelector: { matchLabels: { "app": "billing" } },
  guards: { maxDepth: 5, hopBudget: 20 },
  roles: ["planner", "executor"],
  status: { members: ["billing-agent"], phase: "Ready", ready: true },
};

function installFetch(opts: {
  caps?: Record<string, Record<string, boolean>>;
  registries?: (qs: URLSearchParams) => { ok: boolean; status?: number; body: unknown };
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
        return j({ namespace: "", allowed: opts.caps ?? { agentregistries: { create: true, update: true, delete: true } } });
      if (url.startsWith("/api/agentregistries") && method === "GET" && !url.includes("/default/")) {
        const qs = new URLSearchParams(url.split("?")[1] ?? "");
        const r = opts.registries?.(qs) ?? { ok: true, body: { items: [], nextCursor: "" } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 500));
      }
      if (url === "/api/agentregistries" && method === "POST") {
        const r = opts.create ?? { ok: true, body: { ...DEFAULT_DETAIL, name: "new-registry" } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 422));
      }
      if (url.match(/\/api\/agentregistries\/[^/]+\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      if (url.match(/\/api\/agentregistries\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.update ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 422));
      }
      if (url.match(/\/api\/agentregistries\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.remove ?? { ok: true };
        return Promise.resolve({ ok: r.ok, status: r.status ?? (r.ok ? 204 : 403), json: async () => ({}) } as Response);
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderList() {
  return render(
    <MemoryRouter initialEntries={["/registries"]}>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <Routes>
            <Route path="registries" element={<AgentRegistriesPage />} />
          </Routes>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
}

function renderDetail(path = "/registries/default/prod-registry") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="registries/:ns/:name" element={<AgentRegistryDetailPage />} />
              <Route path="registries" element={<div>registries list</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function renderCreate() {
  return render(
    <MemoryRouter initialEntries={["/registries/new"]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="registries/new" element={<NewAgentRegistryPage />} />
              <Route path="registries/:ns/:name" element={<div>detail</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

// List tests -----------------------------------------------------------------

describe("AgentRegistriesPage — list", () => {
  it("renders teaching empty state", async () => {
    installFetch({ registries: () => ({ ok: true, body: { items: [], nextCursor: "" } }) });
    renderList();
    expect(await screen.findByText("No agent registries yet")).toBeInTheDocument();
  });

  it("renders registry row with registryId and roles", async () => {
    installFetch({
      registries: () => ({
        ok: true,
        body: {
          items: [{
            name: "prod-registry",
            namespace: "default",
            registryId: "prod-reg-001",
            memberSelector: {},
            roles: ["planner", "executor"],
            phase: "Ready",
            ready: true,
          }],
          nextCursor: "",
        },
      }),
    });
    renderList();
    expect(await screen.findByText("prod-registry")).toBeInTheDocument();
    expect(screen.getByText("prod-reg-001")).toBeInTheDocument();
    expect(screen.getByText("planner, executor")).toBeInTheDocument();
  });

  it("paginates with cursor", async () => {
    const calls = installFetch({
      registries: (qs) => {
        const cursor = qs.get("cursor") ?? "";
        if (!cursor) return { ok: true, body: { items: [{ name: "reg-0", namespace: "default", registryId: "r0", memberSelector: {}, roles: [], phase: "Ready", ready: true }], nextCursor: "c1" } };
        return { ok: true, body: { items: [{ name: "reg-1", namespace: "default", registryId: "r1", memberSelector: {}, roles: [], phase: "Ready", ready: true }], nextCursor: "" } };
      },
    });
    renderList();
    expect(await screen.findByText("reg-0")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Next page/ }));
    expect(await screen.findByText("reg-1")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("cursor=c1"))).toBe(true);
  });

  it("renders 403 as forbidden variant", async () => {
    installFetch({ registries: () => ({ ok: false, status: 403, body: { error: "forbidden: cannot list agentregistries" } }) });
    renderList();
    expect(await screen.findByText(/forbidden: cannot list agentregistries/)).toBeInTheDocument();
  });
});

// RBAC tests -----------------------------------------------------------------

describe("AgentRegistriesPage — RBAC-aware row actions", () => {
  const oneReg = () => ({
    ok: true,
    body: {
      items: [{ name: "prod-registry", namespace: "default", registryId: "r1", memberSelector: {}, roles: [], phase: "Ready", ready: true }],
      nextCursor: "",
    },
  });

  it("viewer sees no edit or delete buttons", async () => {
    installFetch({ caps: { agentregistries: { create: false, update: false, delete: false } }, registries: () => oneReg() });
    renderList();
    await screen.findByText("prod-registry");
    expect(screen.queryByTestId("edit-prod-registry")).toBeNull();
    expect(screen.queryByTestId("delete-prod-registry")).toBeNull();
  });

  it("caller with update+delete sees both buttons", async () => {
    installFetch({ caps: { agentregistries: { create: true, update: true, delete: true } }, registries: () => oneReg() });
    renderList();
    await screen.findByText("prod-registry");
    expect(screen.getByTestId("edit-prod-registry")).toBeInTheDocument();
    expect(screen.getByTestId("delete-prod-registry")).toBeInTheDocument();
  });
});

// Detail + constraint tests --------------------------------------------------

describe("AgentRegistryDetailPage", () => {
  it("renders registryId as READ-ONLY — no editable registryId input on edit", async () => {
    installFetch({});
    renderDetail("/registries/default/prod-registry?edit=1");
    await screen.findByTestId("registry-id-readonly");
    // The registryId is shown in a read-only div, not an editable input
    expect(screen.getByTestId("registry-id-readonly")).toHaveTextContent("prod-reg-001");
    // Confirm there's no editable input with the registryId value that could be changed
    const editableInputs = document.querySelectorAll("input");
    const registryIdInputs = Array.from(editableInputs).filter(
      (el) => el.value === "prod-reg-001",
    );
    expect(registryIdInputs.length).toBe(0);
  });

  it("NO egress/allowlist field in any form or detail panel", async () => {
    installFetch({});
    renderDetail();
    await screen.findByTestId("registry-detail-page");
    // No egress, allowlist, or NetworkPolicy input
    expect(screen.queryByLabelText(/egress/i)).toBeNull();
    expect(screen.queryByLabelText(/allowlist/i)).toBeNull();
    expect(screen.queryByLabelText(/network.?policy/i)).toBeNull();
    expect(screen.queryByTestId("egress-input")).toBeNull();
    expect(screen.queryByTestId("allowlist-input")).toBeNull();
  });

  it("NO egress/allowlist field in edit wizard", async () => {
    installFetch({});
    renderDetail("/registries/default/prod-registry?edit=1");
    await screen.findByTestId("registry-id-readonly");
    // Navigate through all wizard steps (wizard uses "Continue" not "Next")
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => screen.getByTestId("edit-roles"));
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("registry-edit-review");
    // Ensure egress never appeared in any step
    expect(screen.queryByLabelText(/egress/i)).toBeNull();
    expect(screen.queryByLabelText(/allowlist/i)).toBeNull();
    expect(screen.queryByText(/egress/i)).toBeNull();
    expect(screen.queryByText(/allowlist/i)).toBeNull();
  });

  it("detail page shows members and roles", async () => {
    installFetch({});
    renderDetail();
    expect(await screen.findByTestId("registry-detail-page")).toBeInTheDocument();
    // registryId shown read-only in detail
    expect(screen.getByTestId("registry-id-display")).toHaveTextContent("prod-reg-001");
    // members shown
    expect(screen.getByTestId("registry-members-panel")).toBeInTheDocument();
    expect(screen.getByText("billing-agent")).toBeInTheDocument();
  });

  it("PUT body does NOT include registryId (immutable by construction)", async () => {
    const calls = installFetch({ update: { ok: true, body: DEFAULT_DETAIL } });
    renderDetail("/registries/default/prod-registry?edit=1");
    await screen.findByTestId("registry-id-readonly");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-roles");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("registry-edit-review");
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => {
      const putCall = calls.find((c) => c.method === "PUT" && c.url.includes("/api/agentregistries/"));
      expect(putCall).toBeDefined();
      const body = JSON.parse(putCall!.body);
      // The PUT body must NOT include registryId (it's omitted from AgentRegistryUpdateRequest)
      expect(body.registryId).toBeUndefined();
      // No egress field either
      expect(body.egress).toBeUndefined();
      expect(body.allowlist).toBeUndefined();
    });
  });

  it("typed-name delete calls DELETE on confirm", async () => {
    const calls = installFetch({ remove: { ok: true } });
    renderDetail("/registries/default/prod-registry?delete=1");
    await screen.findByTestId("registry-detail-page");
    fireEvent.change(screen.getByPlaceholderText("prod-registry"), { target: { value: "prod-registry" } });
    fireEvent.click(screen.getByRole("button", { name: /Delete registry/ }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === "DELETE" && c.url.includes("/api/agentregistries/default/prod-registry"))).toBe(true);
    });
  });
});

// Create tests ---------------------------------------------------------------

describe("NewAgentRegistryPage", () => {
  it("create wizard submits body with registryId but no egress field", async () => {
    const calls = installFetch({ create: { ok: true, body: { ...DEFAULT_DETAIL, name: "new-registry" } } });
    renderCreate();
    // Step 1: identity
    fireEvent.change(screen.getByTestId("new-registry-name"), { target: { value: "new-registry" } });
    fireEvent.change(screen.getByTestId("new-registry-id"), { target: { value: "new-reg-001" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Step 2: member selector
    await waitFor(() => screen.getByTestId("add-label-button"));
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Step 3: roles
    await screen.findByTestId("new-registry-roles");
    fireEvent.change(screen.getByTestId("new-registry-roles"), { target: { value: "planner" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Review
    await screen.findByTestId("new-registry-review");
    // registryId appears in review
    expect(screen.getByTestId("review-new-registry-id")).toHaveTextContent("new-reg-001");
    fireEvent.click(screen.getByRole("button", { name: /Create registry/ }));
    await waitFor(() => {
      const postCall = calls.find((c) => c.method === "POST" && c.url === "/api/agentregistries");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      expect(body.registryId).toBe("new-reg-001");
      // No egress or allowlist field
      expect(body.egress).toBeUndefined();
      expect(body.allowlist).toBeUndefined();
    });
  });
});
