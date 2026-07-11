import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { SecretBindingsPage } from "@/pages/secret-bindings-page";
import { SecretBindingDetailPage, NewSecretBindingPage } from "@/pages/secret-binding-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Helpers --------------------------------------------------------------------

const DEFAULT_DETAIL = {
  name: "oai-key",
  namespace: "default",
  backend: "kubernetes",
  secretRef: { name: "my-oai-secret", key: "apiKey" },
  phase: "Resolved",
  ready: true,
};

function installFetch(opts: {
  caps?: Record<string, Record<string, boolean>>;
  bindings?: (qs: URLSearchParams) => { ok: boolean; status?: number; body: unknown };
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
        return j({ namespace: "", allowed: opts.caps ?? { secretbindings: { create: true, update: true, delete: true } } });
      if (url.startsWith("/api/secretbindings") && method === "GET" && !url.includes("/default/")) {
        const qs = new URLSearchParams(url.split("?")[1] ?? "");
        const r = opts.bindings?.(qs) ?? { ok: true, body: { items: [], nextCursor: "" } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 500));
      }
      if (url === "/api/secretbindings" && method === "POST") {
        const r = opts.create ?? { ok: true, body: { ...DEFAULT_DETAIL, name: "new-binding" } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 422));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.update ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 422));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "DELETE") {
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
    <MemoryRouter initialEntries={["/secrets"]}>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <Routes>
            <Route path="secrets" element={<SecretBindingsPage />} />
          </Routes>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
}

function renderDetail(path = "/secrets/default/oai-key") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="secrets/:ns/:name" element={<SecretBindingDetailPage />} />
              <Route path="secrets" element={<div>secrets list</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function renderCreate() {
  return render(
    <MemoryRouter initialEntries={["/secrets/new"]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="secrets/new" element={<NewSecretBindingPage />} />
              <Route path="secrets/:ns/:name" element={<div>detail</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

// List tests -----------------------------------------------------------------

describe("SecretBindingsPage — list", () => {
  it("renders teaching empty state when BFF returns no items", async () => {
    installFetch({ bindings: () => ({ ok: true, body: { items: [], nextCursor: "" } }) });
    renderList();
    expect(await screen.findByText("No secret bindings yet")).toBeInTheDocument();
  });

  it("renders secretRef.name/key — never a value field", async () => {
    installFetch({
      bindings: () => ({
        ok: true,
        body: {
          items: [DEFAULT_DETAIL],
          nextCursor: "",
        },
      }),
    });
    renderList();
    // The ref is shown
    expect(await screen.findByText("my-oai-secret/apiKey")).toBeInTheDocument();
    // There must be no input with type=password or any "value" label
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(screen.queryByLabelText(/secret value/i)).toBeNull();
    expect(screen.queryByText(/credential/i)).toBeNull();
  });

  it("paginates with cursor", async () => {
    const calls = installFetch({
      bindings: (qs) => {
        const cursor = qs.get("cursor") ?? "";
        if (!cursor) return { ok: true, body: { items: [{ ...DEFAULT_DETAIL, name: "sb-0" }], nextCursor: "c1" } };
        return { ok: true, body: { items: [{ ...DEFAULT_DETAIL, name: "sb-1" }], nextCursor: "" } };
      },
    });
    renderList();
    expect(await screen.findByText("sb-0")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Next page/ }));
    expect(await screen.findByText("sb-1")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("cursor=c1"))).toBe(true);
  });

  it("renders 403 as forbidden variant, not empty list", async () => {
    installFetch({ bindings: () => ({ ok: false, status: 403, body: { error: "forbidden: cannot list secretbindings" } }) });
    renderList();
    expect(await screen.findByText(/forbidden: cannot list secretbindings/)).toBeInTheDocument();
  });
});

// RBAC tests -----------------------------------------------------------------

describe("SecretBindingsPage — RBAC-aware row actions", () => {
  const oneSb = () => ({
    ok: true,
    body: { items: [DEFAULT_DETAIL], nextCursor: "" },
  });

  it("viewer sees no edit or delete buttons", async () => {
    installFetch({ caps: { secretbindings: { create: false, update: false, delete: false } }, bindings: () => oneSb() });
    renderList();
    await screen.findByText("oai-key");
    expect(screen.queryByTestId("edit-oai-key")).toBeNull();
    expect(screen.queryByTestId("delete-oai-key")).toBeNull();
  });

  it("caller with update+delete sees both buttons", async () => {
    installFetch({ caps: { secretbindings: { create: true, update: true, delete: true } }, bindings: () => oneSb() });
    renderList();
    await screen.findByText("oai-key");
    expect(screen.getByTestId("edit-oai-key")).toBeInTheDocument();
    expect(screen.getByTestId("delete-oai-key")).toBeInTheDocument();
  });
});

// SECURITY: no-value invariant tests -----------------------------------------

describe("SecretBinding — NO VALUE field in UI (security invariant)", () => {
  it("detail page shows ref + status but NO value/data input or display", async () => {
    installFetch({});
    renderDetail();
    await screen.findByTestId("secret-detail-page");
    // Shows the ref
    expect(screen.getByTestId("secret-ref-name")).toHaveTextContent("my-oai-secret");
    expect(screen.getByTestId("secret-ref-key")).toHaveTextContent("apiKey");
    // No-value note is shown
    expect(screen.getByTestId("no-value-note")).toBeInTheDocument();
    // No password input or value field
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(screen.queryByLabelText(/value/i)).toBeNull();
    expect(screen.queryByLabelText(/credential/i)).toBeNull();
    // The detail panel exists but does not contain "value" fields
    const panel = screen.getByTestId("secret-detail-panel");
    expect(panel.querySelector("[data-testid=secret-ref-value]")).toBeNull();
  });

  it("edit wizard form shows only secretRef.name and key — no value field", async () => {
    installFetch({});
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("no-value-form-note");
    // The no-value notice is present
    expect(screen.getByTestId("no-value-form-note")).toBeInTheDocument();
    // secretRef name and key fields
    expect(screen.getByTestId("edit-sb-secret-name")).toBeInTheDocument();
    expect(screen.getByTestId("edit-sb-secret-key")).toBeInTheDocument();
    // NO password input or credential-value field
    expect(document.querySelector("input[type=password]")).toBeNull();
    // No input dedicated to a credential value (distinct from labels that say "not the value")
    expect(screen.queryByLabelText(/^(secret )?value$/i)).toBeNull();
    expect(screen.queryByLabelText(/credential/i)).toBeNull();
    // No input element with a "value" or "secret-data" testid
    expect(document.querySelector("input[data-testid*=value]")).toBeNull();
    expect(document.querySelector("input[data-testid*=secret-data]")).toBeNull();
  });

  it("create wizard shows secretRef fields but no value input", async () => {
    installFetch({});
    renderCreate();
    // Navigate to the ref step (wizard Next button is labeled "Continue")
    fireEvent.change(screen.getByTestId("new-sb-name"), { target: { value: "my-binding" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("no-value-create-note");
    // Notice is shown
    expect(screen.getByTestId("no-value-create-note")).toBeInTheDocument();
    // Name and key inputs exist
    expect(screen.getByTestId("new-sb-secret-name")).toBeInTheDocument();
    expect(screen.getByTestId("new-sb-secret-key")).toBeInTheDocument();
    // No value input
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(document.querySelector("[data-testid*=secret-value]")).toBeNull();
  });

  it("POST body from create carries only secretRef (no value field)", async () => {
    const calls = installFetch({ create: { ok: true, body: { ...DEFAULT_DETAIL, name: "new-binding" } } });
    renderCreate();
    // Step 1: identity
    fireEvent.change(screen.getByTestId("new-sb-name"), { target: { value: "new-binding" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Step 2: ref
    await screen.findByTestId("new-sb-secret-name");
    fireEvent.change(screen.getByTestId("new-sb-secret-name"), { target: { value: "my-secret" } });
    fireEvent.change(screen.getByTestId("new-sb-secret-key"), { target: { value: "apiKey" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Review
    await screen.findByTestId("new-secret-review");
    fireEvent.click(screen.getByRole("button", { name: /Create binding/i }));
    await waitFor(() => {
      const postCall = calls.find((c) => c.method === "POST" && c.url === "/api/secretbindings");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      // Has ref — never a value
      expect(body.secretRef.name).toBe("my-secret");
      expect(body.secretRef.key).toBe("apiKey");
      expect(body.value).toBeUndefined();
      expect(body.secretRef.value).toBeUndefined();
      expect(body.data).toBeUndefined();
      expect(body.credential).toBeUndefined();
    });
  });

  it("PUT body from edit carries only secretRef (no value field)", async () => {
    const calls = installFetch({ update: { ok: true, body: DEFAULT_DETAIL } });
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("edit-sb-secret-name");
    fireEvent.change(screen.getByTestId("edit-sb-secret-name"), { target: { value: "updated-secret" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("secret-edit-review");
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() => {
      const putCall = calls.find((c) => c.method === "PUT" && c.url.includes("/api/secretbindings/"));
      expect(putCall).toBeDefined();
      const body = JSON.parse(putCall!.body);
      expect(body.secretRef.name).toBe("updated-secret");
      expect(body.value).toBeUndefined();
      expect(body.secretRef.value).toBeUndefined();
    });
  });
});

// Delete tests ---------------------------------------------------------------

describe("SecretBindingDetailPage — delete", () => {
  it("typed-name delete calls DELETE on confirm", async () => {
    const calls = installFetch({ remove: { ok: true } });
    renderDetail("/secrets/default/oai-key?delete=1");
    await screen.findByTestId("secret-detail-page");
    // ConfirmDialog uses placeholder matching the confirmText
    fireEvent.change(screen.getByPlaceholderText("oai-key"), { target: { value: "oai-key" } });
    fireEvent.click(screen.getByRole("button", { name: /Delete binding/i }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === "DELETE" && c.url.includes("/api/secretbindings/default/oai-key"))).toBe(true);
    });
  });
});
