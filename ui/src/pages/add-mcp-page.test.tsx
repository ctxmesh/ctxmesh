import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
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
    expect(screen.getByText(/forbidden: cannot create toolregistries/)).toBeInTheDocument();
  });

  it("shows the read-only note for a viewer (no create on agentregistries)", async () => {
    recordingFetch({ caps: { agentregistries: { create: false } } });
    renderPage();
    expect(await screen.findByTestId("add-mcp-readonly-note")).toBeInTheDocument();
  });
});
