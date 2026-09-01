import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AddMcpPage } from "@/pages/add-mcp-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Mocked fetch (tier0 determinism): the add-MCP wizard POSTs /api/mcpservers,
// which probes + discovers tools. Tests assert the right body, the discovered
// tools render, the bearer key never leaks client-side, a probe failure teaches,
// and the pending-approval state renders when the API says so.
interface Captured {
  url: string;
  method: string;
  body: string;
}

const BEARER = "mcp-bearer-secret-token-xyz";

function recordingFetch(opts: {
  add?: (body: string) => { ok: boolean; status?: number; json: unknown };
  /**
   * Make the POST itself REJECT — the transport failing, not the server
   * answering. `fetch` rejects with a TypeError when the request never
   * completes (offline, DNS, the BFF down), and that is a different truth from
   * any status code: nothing was sent to the MCP server at all.
   */
  addRejects?: Error;
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
          json: async () => ({ namespace: "", allowed: opts.caps ?? { agentregistries: { create: true } } }),
        } as Response);
      }
      if (url === "/api/mcpservers" && method === "POST" && opts.addRejects) {
        return Promise.reject(opts.addRejects);
      }
      if (url === "/api/mcpservers" && method === "POST") {
        const r = opts.add
          ? opts.add(body)
          : {
              ok: true,
              json: {
                name: "acme-support",
                tools: [
                  { name: "search_docs", description: "Full-text search over docs" },
                  { name: "create_ticket", description: "Open a support ticket" },
                ],
                approvalStatus: "approved",
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
            <AddMcpPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

async function fillServer(opts: { name?: string; url?: string; key?: string } = {}) {
  fireEvent.change(await screen.findByLabelText("Name"), {
    target: { value: opts.name ?? "acme-support" },
  });
  fireEvent.change(screen.getByLabelText(/MCP server URL/), {
    target: { value: opts.url ?? "https://mcp.acme.dev/sse" },
  });
  if (opts.key !== undefined) {
    fireEvent.change(screen.getByLabelText(/Bearer key/), { target: { value: opts.key } });
  }
}

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  sessionStorage.clear();
});

describe("AddMcpPage", () => {
  it("posts URL + key and renders the discovered tools", async () => {
    const calls = recordingFetch({});
    renderPage();

    await fillServer({ key: BEARER });
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    // The review step renders the discovered tools (now in the catalog).
    expect(await screen.findByText("search_docs")).toBeInTheDocument();
    expect(screen.getByText("create_ticket")).toBeInTheDocument();

    // The POST carried the URL + the bearer key.
    const post = calls.find((c) => c.url === "/api/mcpservers" && c.method === "POST");
    expect(post).toBeDefined();
    const payload = JSON.parse(post!.body) as { url: string; apiKey: string; name: string };
    expect(payload.url).toBe("https://mcp.acme.dev/sse");
    expect(payload.apiKey).toBe(BEARER);
    expect(payload.name).toBe("acme-support");
  });

  it("open-server mode (none) probes with no apiKey", async () => {
    const calls = recordingFetch({});
    renderPage();
    // Explicitly choose the open (no-auth) server mode.
    fireEvent.change(screen.getByTestId("mcp-auth-mode"), { target: { value: "none" } });
    await fillServer({}); // name + url only; the key field is hidden in "none" mode
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    expect(await screen.findByText("search_docs")).toBeInTheDocument();
    const post = calls.find((c) => c.url === "/api/mcpservers" && c.method === "POST");
    expect(post).toBeDefined();
    const payload = JSON.parse(post!.body) as Record<string, unknown>;
    expect(payload).not.toHaveProperty("apiKey");
    expect(payload).not.toHaveProperty("authType");
    expect(payload.url).toBe("https://mcp.acme.dev/sse");
  });

  it("never persists the bearer key client-side after submit", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    recordingFetch({});
    renderPage();

    await fillServer({ key: BEARER });
    const keyInput = screen.getByLabelText(/Bearer key/) as HTMLInputElement;
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));
    await screen.findByText("search_docs");

    for (const call of setItem.mock.calls) {
      expect(String(call[0])).not.toContain(BEARER);
      expect(String(call[1])).not.toContain(BEARER);
    }
    expect(JSON.stringify(localStorage)).not.toContain(BEARER);
    expect(JSON.stringify(sessionStorage)).not.toContain(BEARER);
    expect(keyInput.value).not.toContain(BEARER);
    expect(document.body.innerHTML).not.toContain(BEARER);
  });

  it("shows a teaching error on a probe failure (422)", async () => {
    recordingFetch({
      add: () => ({ ok: false, status: 422, json: { error: "MCP handshake failed: connection refused" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    const err = await screen.findByTestId("probe-error");
    expect(err).toHaveTextContent(/MCP handshake failed/);
    expect(err).toHaveTextContent(/reachable MCP endpoint/i);
    // No tools discovered → no review list.
    expect(screen.queryByTestId("tool-list")).toBeNull();
  });

  it("renders the pending-approval state when the API says the tools are pending (hardened)", async () => {
    recordingFetch({
      add: () => ({
        ok: true,
        json: {
          name: "acme-support",
          tools: [{ name: "refund", description: "Issue a refund" }],
          approvalStatus: "pending",
        },
      }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    expect(await screen.findByText("refund")).toBeInTheDocument();
    expect(screen.getByTestId("pending-approval-note")).toBeInTheDocument();
    expect(screen.getByText("pending approval")).toBeInTheDocument();
  });

  it("falls back gracefully when the kill-switch 404s the endpoint", async () => {
    recordingFetch({
      add: () => ({ ok: false, status: 404, json: { error: "byo-mcp disabled" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    expect(await screen.findByTestId("kill-switch-fallback")).toBeInTheDocument();
  });
});

// A probe that RAN and was refused, a probe that NEVER RAN, and a probe this
// install cannot run are three different truths (M151 §7). Collapsing them into
// one "couldn't reach that server" sends a user to fix a URL that was never
// tried — so each gets its own surface, and only the refusal blames a field.
describe("AddMcpPage — the three ways a probe ends", () => {
  it("a REFUSED probe blames the server field and quotes what it said", async () => {
    recordingFetch({
      add: () => ({ ok: false, status: 422, json: { error: "MCP handshake failed: connection refused" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    await screen.findByTestId("probe-error");
    // The address is the thing that has to change, and the field says so.
    const urlField = screen.getByLabelText(/MCP server URL/);
    expect(urlField).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(/didn.t complete the MCP handshake/i)).toBeInTheDocument();
    // The other two states are NOT rendered.
    expect(screen.queryByTestId("probe-unreachable")).toBeNull();
    expect(screen.queryByTestId("probe-unsupported")).toBeNull();
  });

  it("a probe that NEVER RAN is a different state, and blames no field", async () => {
    // fetch rejects — the request never completed, so nothing reached the server.
    recordingFetch({ addRejects: new TypeError("Failed to fetch") });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    const note = await screen.findByTestId("probe-unreachable");
    expect(note).toHaveTextContent(/never ran/i);
    expect(note).toHaveTextContent(/nothing was sent to the server/i);
    // It is NOT the refusal surface, and the server is not accused.
    expect(screen.queryByTestId("probe-error")).toBeNull();
    expect(screen.getByLabelText(/MCP server URL/)).not.toHaveAttribute("aria-invalid");
    expect(screen.queryByTestId("tool-list")).toBeNull();
  });

  it("a 501 is a calm 'not wired here' note, never an error", async () => {
    recordingFetch({
      add: () => ({ ok: false, status: 501, json: { error: "mcp discovery not implemented" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    const note = await screen.findByTestId("probe-unsupported");
    expect(note).toHaveTextContent(/isn.t wired up on this install/i);
    // role="note", never role="alert" — nothing is broken.
    expect(note.querySelector("[role=note]")).not.toBeNull();
    expect(note.querySelector("[role=alert]")).toBeNull();
    expect(screen.queryByTestId("probe-error")).toBeNull();
    expect(screen.getByLabelText(/MCP server URL/)).not.toHaveAttribute("aria-invalid");
  });

  it("a URL that could never be probed is caught in the field, before a round trip", async () => {
    const calls = recordingFetch({});
    renderPage();

    await fillServer({ url: "mcp.acme.dev" }); // no scheme — not an absolute http(s) URL
    expect(screen.getByLabelText(/MCP server URL/)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(/absolute http:\/\/ or https:\/\/ address/i)).toBeInTheDocument();
    // Forward is gated, so no request is spent proving what the field knows.
    expect(screen.getByRole("button", { name: /Probe \+ discover/ })).toBeDisabled();
    expect(calls.find((c) => c.url === "/api/mcpservers" && c.method === "POST")).toBeUndefined();
  });

  it("editing the server retires a stale verdict", async () => {
    recordingFetch({
      add: () => ({ ok: false, status: 422, json: { error: "MCP handshake failed" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));
    await screen.findByTestId("probe-error");

    // The verdict described the URL that was submitted, not this one.
    fireEvent.change(screen.getByLabelText(/MCP server URL/), {
      target: { value: "https://mcp.acme.dev/other" },
    });
    expect(screen.queryByTestId("probe-error")).toBeNull();
    expect(screen.getByLabelText(/MCP server URL/)).not.toHaveAttribute("aria-invalid");
  });
});

describe("AddMcpPage — RBAC-gated", () => {
  it("gates the entry for a viewer and a forced 403 renders ForbiddenInline", async () => {
    recordingFetch({
      caps: { agentregistries: { create: true } },
      add: () => ({ ok: false, status: 403, json: { error: "forbidden: cannot create toolregistries" } }),
    });
    renderPage();

    await fillServer();
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    expect(await screen.findByText("Not allowed to add an MCP server")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/forbidden: cannot/)).toBeNull();
  });

  it("shows the read-only note for a viewer (no create on agentregistries)", async () => {
    recordingFetch({ caps: { agentregistries: { create: false } } });
    renderPage();
    expect(await screen.findByTestId("add-mcp-readonly-note")).toBeInTheDocument();
  });
});

// OAuth 2.1 MCP connect flow (m17.9)
// ──────────────────────────────────
// Invariants:
//   1. A 202 response → the SPA redirects the browser to the authorizationURL.
//   2. No token appears anywhere in state, the DOM, localStorage, or sessionStorage.
//   3. The POST body carries authType: "oauth" (no apiKey).
//   4. The redirecting state is rendered briefly (testid "oauth-redirecting").
//   5. The key-auth path (existing m14.9 tests) is UNAFFECTED.
describe("AddMcpPage — OAuth flow", () => {
  const AUTH_URL = "https://oauth.acme.dev/authorize?client_id=abc&state=xyz123";

  function recordingFetchOAuth(opts: {
    status?: number;
    json?: unknown;
    caps?: Record<string, Record<string, boolean>>;
  } = {}) {
    const calls: Captured[] = [];
    // Mock window.location.href assignment so we can assert the redirect target.
    // jsdom doesn't support navigation so we capture it via Object.defineProperty.
    let capturedHref = "";
    Object.defineProperty(window, "location", {
      configurable: true,
      value: {
        ...window.location,
        get href() { return capturedHref; },
        set href(v: string) { capturedHref = v; },
      },
    });

    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const body = typeof init?.body === "string" ? init.body : "";
        calls.push({ url, method, body });

        if (url.startsWith("/api/namespaces"))
          return Promise.resolve({ ok: true, status: 200, json: async () => ({ namespaces: [] }) } as Response);
        if (url.startsWith("/api/capabilities"))
          return Promise.resolve({
            ok: true, status: 200,
            json: async () => ({ namespace: "", allowed: opts.caps ?? { agentregistries: { create: true } } }),
          } as Response);
        if (url === "/api/mcpservers" && method === "POST") {
          const status = opts.status ?? 202;
          const json = opts.json ?? { authorizationURL: AUTH_URL, state: "xyz123" };
          return Promise.resolve({ ok: status < 400, status, json: async () => json } as Response);
        }
        return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
      }),
    );
    return { calls, getHref: () => capturedHref };
  }

  async function fillServerOAuth(opts: { name?: string; url?: string } = {}) {
    fireEvent.change(await screen.findByLabelText("Name"), {
      target: { value: opts.name ?? "oauth-mcp" },
    });
    fireEvent.change(screen.getByLabelText(/MCP server URL/), {
      target: { value: opts.url ?? "https://mcp.acme.dev/sse" },
    });
    // Switch to OAuth auth mode
    fireEvent.change(screen.getByTestId("mcp-auth-mode"), {
      target: { value: "oauth" },
    });
  }

  afterEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
    sessionStorage.clear();
    // Restore window.location
    Object.defineProperty(window, "location", {
      configurable: true,
      value: window.location,
    });
  });

  it("on 202 response, redirects window.location to the authorizationURL", async () => {
    const { calls, getHref } = recordingFetchOAuth();
    renderPage();

    await fillServerOAuth();
    fireEvent.click(screen.getByRole("button", { name: /Connect via OAuth/ }));

    // The redirecting state is rendered
    expect(await screen.findByTestId("oauth-redirecting")).toBeInTheDocument();

    // The redirect target is the authorization URL
    await waitFor(() => {
      expect(getHref()).toBe(AUTH_URL);
    });

    // The POST body carried authType: "oauth" and NO apiKey
    const post = calls.find((c) => c.url === "/api/mcpservers" && c.method === "POST");
    expect(post).toBeDefined();
    const payload = JSON.parse(post!.body) as Record<string, unknown>;
    expect(payload.authType).toBe("oauth");
    expect(payload).not.toHaveProperty("apiKey");
    expect(payload.name).toBe("oauth-mcp");
  });

  it("degrades to an inline error (no redirect, no login bounce) on a bad authorizationURL", async () => {
    // A 202 with an empty/invalid authorizationURL must NOT trigger window.location
    // (which would remount the SPA and bounce to /login) — the m18.7 guard.
    const { getHref } = recordingFetchOAuth({ status: 202, json: { authorizationURL: "", state: "x" } });
    renderPage();

    await fillServerOAuth();
    fireEvent.click(screen.getByRole("button", { name: /Connect via OAuth/ }));

    expect(
      await screen.findByText(/did not return a valid OAuth authorization URL/i),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("oauth-redirecting")).not.toBeInTheDocument();
    expect(getHref()).toBe("");
  });

  it("no token appears in state, DOM, localStorage, or sessionStorage after OAuth redirect", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    recordingFetchOAuth({ json: { authorizationURL: AUTH_URL, state: "xyz123" } });
    renderPage();

    await fillServerOAuth();
    fireEvent.click(screen.getByRole("button", { name: /Connect via OAuth/ }));
    await screen.findByTestId("oauth-redirecting");

    // No token in storage
    for (const call of setItem.mock.calls) {
      expect(String(call[0])).not.toMatch(/token/i);
      expect(String(call[1])).not.toMatch(/access_token|id_token|refresh_token/i);
    }
    expect(JSON.stringify(localStorage)).not.toMatch(/access_token|id_token|refresh_token/);
    expect(JSON.stringify(sessionStorage)).not.toMatch(/access_token|id_token|refresh_token/);

    // The DOM shows the authorization URL (for manual navigation) but never a
    // token — the auth URL is the redirect target, not a credential.
    expect(document.body.innerHTML).not.toMatch(/access_token|id_token|refresh_token/);
  });

  it("shows error state when OAuth init fails (e.g. 422)", async () => {
    recordingFetchOAuth({ status: 422, json: { error: "OAuth not configured for this server" } });
    renderPage();

    await fillServerOAuth();
    fireEvent.click(screen.getByRole("button", { name: /Connect via OAuth/ }));

    expect(await screen.findByTestId("probe-error")).toBeInTheDocument();
    expect(screen.getByTestId("probe-error")).toHaveTextContent(/OAuth not configured/);
  });

  it("key-auth (m14.9) path is unaffected when authMode is key (backward compat)", async () => {
    const calls = recordingFetch({});
    renderPage();

    // Default auth mode is "key" — do NOT switch to OAuth
    await fillServer({ key: BEARER });
    fireEvent.click(screen.getByRole("button", { name: /Probe \+ discover/ }));

    expect(await screen.findByText("search_docs")).toBeInTheDocument();
    const post = calls.find((c) => c.url === "/api/mcpservers" && c.method === "POST");
    expect(post).toBeDefined();
    const payload = JSON.parse(post!.body) as Record<string, unknown>;
    expect(payload.apiKey).toBe(BEARER);
    expect(payload.authType).toBeUndefined();
  });
});
