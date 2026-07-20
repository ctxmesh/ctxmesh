import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AgentChatboxPage } from "@/pages/agent-chatbox-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// The standalone per-agent chatbox (m37): chrome-less, pinned to the agent in the URL,
// reusing the shared ChatPanel (same invoke + memory-threaded conversation).
function installFetch() {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (b: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: async () => b, text: async () => JSON.stringify(b) } as Response);
      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+/) && method === "GET")
        return j({
          name: "scalekit-agent",
          namespace: "default",
          ready: true,
          bindings: [{ kind: "tool", name: "t", server: "scalekit-mcp-server", detail: "list_env", ready: true }],
          versions: [],
          conditions: [],
        });
      if (url === "/api/invoke" && method === "POST")
        return j({ traceId: "t-1", response: '{"output":"hi there"}' });
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
  return calls;
}

function renderAt(path = "/chat/default/scalekit-agent") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="/chat/:ns/:name" element={<AgentChatboxPage />} />
              <Route path="/traces/:id" element={<div>trace page</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("AgentChatboxPage", () => {
  it("renders a chrome-less chatbox pinned to the agent from the URL", async () => {
    installFetch();
    renderAt();
    // The agent name from the URL + the shared ChatPanel (no console nav around it).
    expect(await screen.findByTestId("chatbox-agent")).toHaveTextContent("scalekit-agent");
    expect(await screen.findByTestId("chat-panel")).toBeInTheDocument();
    expect(screen.getByTestId("chat-input")).toBeInTheDocument();
  });

  it("sends a message → POST /api/invoke with the agent + a conversation id", async () => {
    const calls = installFetch();
    renderAt();
    const input = await screen.findByTestId("chat-input");
    fireEvent.change(input, { target: { value: "list environments" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/invoke" && c.method === "POST")).toBeDefined(),
    );
    const body = JSON.parse(calls.find((c) => c.url === "/api/invoke")!.body);
    expect(body.agent).toBe("scalekit-agent");
    expect(body.namespace).toBe("default");
    expect(body.conversationId).toBeTruthy();
  });

  it("pins to the agent passed as PROPS (host-pinned mode, m37.3) even with no URL params", async () => {
    installFetch();
    render(
      <MemoryRouter initialEntries={["/"]}>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <Routes>
                {/* At an agent's own hostname the app mounts the chatbox at "/" with the agent from
                    the injected <meta>, passed as props — there are no /:ns/:name URL params. */}
                <Route path="*" element={<AgentChatboxPage ns="default" name="scalekit-agent" />} />
              </Routes>
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByTestId("chatbox-agent")).toHaveTextContent("scalekit-agent");
    expect(await screen.findByTestId("chat-panel")).toBeInTheDocument();
  });

  it("shows a not-found error for a missing agent", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.startsWith("/api/capabilities"))
          return Promise.resolve({ ok: true, status: 200, json: async () => ({ allowed: {} }), text: async () => "" } as Response);
        if (url.startsWith("/api/namespaces"))
          return Promise.resolve({ ok: true, status: 200, json: async () => ({ namespaces: [] }), text: async () => "" } as Response);
        return Promise.resolve({ ok: false, status: 404, json: async () => ({ error: "not found" }), text: async () => "" } as Response);
      }),
    );
    renderAt("/chat/default/ghost");
    expect(await screen.findByRole("alert")).toHaveTextContent(/not found/i);
  });
});
