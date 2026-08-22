import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AgentChatboxPage } from "@/pages/agent-chatbox-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// sseBody wraps SSE frame strings in a ReadableStream-like body (getReader) so the durable
// run event stream (openRunStream) can be driven deterministically — mirrors the Playground
// test's helper. ChatPanel now converges on durable runs (ADR 0093), so the chatbox goes
// through createRun → stream → getRun on a real cluster (no devmode probe → default cluster).
function sseBody(frames: string[]) {
  const enc = new TextEncoder();
  let i = 0;
  return {
    getReader() {
      return {
        read() {
          if (i < frames.length)
            return Promise.resolve({ value: enc.encode(frames[i++]), done: false });
          return Promise.resolve({ value: undefined, done: true });
        },
        releaseLock() {},
      };
    },
  };
}

// The standalone per-agent chatbox (m37): chrome-less, pinned to the agent in the URL,
// reusing the shared ChatPanel (durable runs + memory-threaded conversation, ADR 0093).
function installFetch() {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      calls.push({ url: path, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (b: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: async () => b, text: async () => JSON.stringify(b) } as Response);
      if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (path.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
      if (path.match(/\/api\/agents\/[^/]+\/[^/]+/) && method === "GET")
        return j({
          name: "scalekit-agent",
          namespace: "default",
          ready: true,
          bindings: [{ kind: "tool", name: "t", server: "scalekit-mcp-server", detail: "list_env", ready: true }],
          versions: [],
          conditions: [],
        });
      // Durable run path (ADR 0093): create → stream → finalize.
      if (path === "/api/runs" && method === "POST")
        return j({ id: "run-1", status: "queued" });
      if (path.endsWith("/events"))
        return Promise.resolve({
          ok: true,
          status: 200,
          body: sseBody([
            'event:message\ndata:{"output":"hi there"}\n\n',
            "event:state\ndata:succeeded\n\n",
          ]),
        } as unknown as Response);
      if (path.startsWith("/api/runs/"))
        return j({
          id: "run-1",
          status: "succeeded",
          traceId: "t-1",
          messages: [{ role: "assistant", content: '{"output":"hi there"}' }],
        });
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

  it("sends a message → durable POST /api/runs with the agent + a conversation id (ADR 0093)", async () => {
    const calls = installFetch();
    renderAt();
    const input = await screen.findByTestId("chat-input");
    fireEvent.change(input, { target: { value: "list environments" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    // On a real cluster (no devmode) the chat converges on the durable run path — createRun,
    // NOT the synchronous /invoke — so every chat turn is a first-class observable run.
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/runs" && c.method === "POST")).toBeDefined(),
    );
    expect(calls.find((c) => c.url === "/api/invoke")).toBeUndefined();
    const body = JSON.parse(calls.find((c) => c.url === "/api/runs")!.body);
    expect(body.agent).toBe("scalekit-agent");
    expect(body.namespace).toBe("default");
    expect(body.input).toEqual({ input: "list environments" });
    expect(body.conversationId).toBeTruthy();
    // The turn finalizes with the streamed answer + the run's trace link.
    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("hi there");
    await waitFor(() => expect(screen.getByTestId("open-trace")).toBeInTheDocument());
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
