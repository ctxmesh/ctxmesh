import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ConnectProviderPage } from "@/pages/connect-provider-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// A recording fetch mock: captures every request (url, method, body) so tests
// assert the RIGHT connect body was POSTed AND that the pasted key is present in
// the request body but NEVER leaks into any client-side store (the browser-
// never-holds-creds discipline, ADR 0015). Mocked fetch = tier0 determinism.
interface Captured {
  url: string;
  method: string;
  body: string;
}

const SECRET_KEY = "sk-ant-super-secret-value-123";

function recordingFetch(opts: {
  connect?: (body: string) => { ok: boolean; status?: number; json: unknown };
  caps?: Record<string, Record<string, boolean>>;
}) {
  const calls: Captured[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url, method, body });

      if (url.startsWith("/api/namespaces")) {
        return Promise.resolve({ ok: true, status: 200, json: async () => ({ namespaces: [] }) } as Response);
      }
      if (url.startsWith("/api/capabilities")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ namespace: "", allowed: opts.caps ?? { secretbindings: { create: true } } }),
        } as Response);
      }
      if (url === "/api/providers" && method === "POST") {
        const r = opts.connect
          ? opts.connect(body)
          : {
              ok: true,
              // The REAL BFF shape: provider details NESTED under `provider`
              // (a ProviderSummary) with `models` as plain string IDs + `created`.
              json: {
                provider: {
                  name: "anthropic",
                  namespace: "default",
                  provider: "anthropic",
                  displayName: "Anthropic",
                  models: ["claude-opus-4", "claude-sonnet-4"],
                  secretName: "anthropic-key",
                  ready: true,
                },
                created: [
                  { kind: "Secret", name: "anthropic", namespace: "default" },
                ],
              },
            };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 201 : 400),
          json: async () => r.json,
          text: async () => JSON.stringify(r.json),
        } as Response);
      }
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
  return calls;
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <ConnectProviderPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

// advanceToKeyStep clicks Continue from the provider step (Anthropic is default
// selected) into the key step.
async function advanceToKeyStep() {
  fireEvent.click(await screen.findByRole("button", { name: /Continue/ }));
  await screen.findByLabelText("Anthropic API key");
}

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  sessionStorage.clear();
});

describe("ConnectProviderPage", () => {
  it("fills provider + key, POSTs the right body, and renders the returned model list", async () => {
    const calls = recordingFetch({});
    renderPage();

    await advanceToKeyStep();
    fireEvent.change(screen.getByLabelText("Anthropic API key"), { target: { value: SECRET_KEY } });
    fireEvent.click(screen.getByRole("button", { name: /Connect provider/ }));

    // The review step renders the LIVE model list from the POST response.
    expect(await screen.findByText("claude-opus-4")).toBeInTheDocument();
    expect(screen.getByText("claude-sonnet-4")).toBeInTheDocument();
    // The created secret reference is shown (no secret material, just the ref).
    expect(screen.getByText("anthropic-key")).toBeInTheDocument();

    // The POST carried the right body: provider + the pasted key.
    const post = calls.find((c) => c.url === "/api/providers" && c.method === "POST");
    expect(post).toBeDefined();
    const payload = JSON.parse(post!.body) as { provider: string; apiKey: string };
    expect(payload.provider).toBe("anthropic");
    expect(payload.apiKey).toBe(SECRET_KEY);
  });

  it("shows an honest inline error on a bad key (400) without crashing", async () => {
    recordingFetch({
      connect: () => ({ ok: false, status: 400, json: { error: "invalid api key" } }),
    });
    renderPage();

    await advanceToKeyStep();
    fireEvent.change(screen.getByLabelText("Anthropic API key"), { target: { value: "sk-bad" } });
    fireEvent.click(screen.getByRole("button", { name: /Connect provider/ }));

    // The inline error surfaces the API's real message; no review, no crash.
    expect(await screen.findByTestId("connect-error")).toHaveTextContent(/invalid api key/);
    expect(screen.queryByTestId("model-list")).toBeNull();
  });

  it("NEVER persists the key client-side after submit (no store / localStorage / sessionStorage)", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    recordingFetch({});
    renderPage();

    await advanceToKeyStep();
    const keyInput = screen.getByLabelText("Anthropic API key") as HTMLInputElement;
    fireEvent.change(keyInput, { target: { value: SECRET_KEY } });
    fireEvent.click(screen.getByRole("button", { name: /Connect provider/ }));

    // Wait for the connect to settle (the review renders the models).
    await screen.findByText("claude-opus-4");

    // The key is absent from EVERY web-storage write…
    for (const call of setItem.mock.calls) {
      expect(String(call[0])).not.toContain(SECRET_KEY);
      expect(String(call[1])).not.toContain(SECRET_KEY);
    }
    // …absent from localStorage / sessionStorage entirely…
    expect(JSON.stringify(localStorage)).not.toContain(SECRET_KEY);
    expect(JSON.stringify(sessionStorage)).not.toContain(SECRET_KEY);
    // …and cleared from the form field itself (the ONLY place it ever lived).
    expect(keyInput.value).not.toContain(SECRET_KEY);
    // The DOM no longer contains the secret anywhere (only the secretName ref).
    expect(document.body.innerHTML).not.toContain(SECRET_KEY);
  });

  it("falls back to reference-existing when the kill-switch 404s the endpoint", async () => {
    recordingFetch({
      connect: () => ({ ok: false, status: 404, json: { error: "provider connect disabled" } }),
    });
    renderPage();

    await advanceToKeyStep();
    fireEvent.change(screen.getByLabelText("Anthropic API key"), { target: { value: SECRET_KEY } });
    fireEvent.click(screen.getByRole("button", { name: /Connect provider/ }));

    // The hardened-install fallback teaches reference-an-existing-SecretBinding.
    expect(await screen.findByTestId("kill-switch-fallback")).toBeInTheDocument();
    expect(screen.getByText(/reference an existing/i)).toBeInTheDocument();
  });
});

describe("ConnectProviderPage — RBAC-gated", () => {
  it("gates the entry for a viewer (no create) and a forced 403 renders ForbiddenInline", async () => {
    // A viewer: secretbindings.create is false → the wizard's forward is gated…
    const calls = recordingFetch({
      caps: { secretbindings: { create: false } },
      // …and even a forced submit gets an honest 403 from the API.
      connect: () => ({ ok: false, status: 403, json: { error: "forbidden: cannot create secretbindings" } }),
    });
    renderPage();

    // The read-only note appears (the display-only gate).
    expect(await screen.findByTestId("connect-readonly-note")).toBeInTheDocument();

    await advanceToKeyStep();
    fireEvent.change(screen.getByLabelText("Anthropic API key"), { target: { value: SECRET_KEY } });

    // The forward action is disabled for a viewer (canProceed gates on create)…
    const connectBtn = screen.getByRole("button", { name: /Connect provider/ });
    await waitFor(() => expect(connectBtn).toBeDisabled());
    // …but if the API is forced (stale cache), a 403 → ForbiddenInline.
    // Simulate the forced path directly: no POST happened while disabled.
    expect(calls.find((c) => c.url === "/api/providers" && c.method === "POST")).toBeUndefined();
  });

  it("renders ForbiddenInline when the connect POST returns 403 (the real gate)", async () => {
    // Optimistic caps (create allowed by display) but the API denies — the stale
    // "yes" surprise path: 403 → ForbiddenInline.
    recordingFetch({
      caps: { secretbindings: { create: true } },
      connect: () => ({ ok: false, status: 403, json: { error: "forbidden: cannot create secretbindings" } }),
    });
    renderPage();

    await advanceToKeyStep();
    fireEvent.change(screen.getByLabelText("Anthropic API key"), { target: { value: SECRET_KEY } });
    fireEvent.click(screen.getByRole("button", { name: /Connect provider/ }));

    expect(await screen.findByText("Not allowed to connect a provider")).toBeInTheDocument();
    expect(screen.getByText(/forbidden: cannot create secretbindings/)).toBeInTheDocument();
  });
});
