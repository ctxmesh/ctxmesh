import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ChatPanel } from "@/components/agent-chat";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { DevModeContext } from "@/lib/dev-mode";
import { MCP_OAUTH_MESSAGE } from "@/lib/oauth-popup";

// ChatPanel converges on DURABLE runs (ADR 0093): each cluster chat turn is createRun →
// openRunStream → finalize (getRun), so it becomes a first-class, observable run — instead of
// the old synchronous /api/invoke that created no durable run. These tests pin that:
//   (a) a cluster turn calls createRun (not invoke) with {input:{input:text}, conversationId}
//       and finalizes the traceId onto the turn;
//   (b) streamed tokens render live;
//   (c) a consent_required finalize shows the Connect CTA → clicking connect RESUMES the same
//       run (resumeRun), never a new createRun;
//   (d) dev-mode keeps the old /api/invoke path (no cluster run store, ADR 0093 §2);
//   (e) a forbidden create → the ForbiddenInline turn state.

interface Captured {
  url: string;
  method: string;
  body: string;
}

// sseBody wraps SSE frame strings in a ReadableStream-like body (getReader) so the durable run
// event stream (openRunStream) drives deterministically — mirrors the Playground test helper.
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

// RunMock shapes the durable run: the SSE frames its /events stream emits, the structured detail
// GET /api/runs/{id} returns (per call, so a consent-then-success sequence can be scripted), and
// the create outcome (for the forbidden case).
interface RunMock {
  frames?: string[];
  details?: unknown[]; // successive GET /api/runs/{id} responses (consent → success)
  createOk?: boolean;
  createStatus?: number;
  createJson?: unknown;
}

function recordingFetch(run: RunMock = {}) {
  const calls: Captured[] = [];
  let detailIdx = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });
      const j = (b: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: async () => b, text: async () => JSON.stringify(b) } as Response);

      if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (path.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: { agentdeployments: { create: true } } });

      // Durable run: create → stream → finalize.
      if (path === "/api/runs" && method === "POST") {
        const ok = run.createOk ?? true;
        return Promise.resolve({
          ok,
          status: run.createStatus ?? (ok ? 202 : 403),
          json: async () => run.createJson ?? { id: "run-1", status: "queued" },
          text: async () => JSON.stringify(run.createJson ?? { error: "denied" }),
        } as Response);
      }
      if (path.endsWith("/events"))
        return Promise.resolve({
          ok: true,
          status: 200,
          body: sseBody(
            run.frames ?? [
              'event:message\ndata:{"output":"hi there"}\n\n',
              "event:state\ndata:succeeded\n\n",
            ],
          ),
        } as unknown as Response);
      // POST /api/runs/{id}/resume — the consent connect-and-continue (no decision).
      if (path.startsWith("/api/runs/") && path.endsWith("/resume"))
        return j({ id: "run-1", status: "running" });
      // GET /api/runs/{id} — successive structured details (consent → success).
      if (path.startsWith("/api/runs/")) {
        const details = run.details ?? [
          {
            id: "run-1",
            status: "succeeded",
            traceId: "trace-xyz",
            messages: [{ role: "assistant", content: '{"output":"hi there"}' }],
          },
        ];
        const detail = details[Math.min(detailIdx, details.length - 1)];
        detailIdx += 1;
        return j(detail);
      }
      // Inline-consent begin (ADR 0031): 202 + an authorization URL to pop open.
      if (path === "/api/mcp/oauth/grant")
        return Promise.resolve({
          ok: true,
          status: 202,
          json: async () => ({ authorizationURL: "https://as.example/authorize?x=1", state: "st" }),
          text: async () => "",
        } as Response);
      // The dev-mode fallback path.
      if (path === "/api/invoke" && method === "POST")
        return j({ traceId: "dev-trace", response: '{"output":"dev answer"}', consentRequired: undefined });
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}), text: async () => "" } as Response);
    }),
  );
  return calls;
}

function renderChat(devMode = false, audience: "operator" | "end-user" = "operator") {
  const onTraced = vi.fn();
  const utils = render(
    <MemoryRouter>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <DevModeContext.Provider value={devMode}>
            <ChatPanel
              ns="default"
              name="scalekit-agent"
              ready
              memoryBound
              onTraced={onTraced}
              audience={audience}
            />
          </DevModeContext.Provider>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
  return { onTraced, ...utils };
}

async function sendMessage(text: string) {
  const input = await screen.findByTestId("chat-input");
  fireEvent.change(input, { target: { value: text } });
  fireEvent.click(screen.getByTestId("chat-send"));
}

afterEach(() => vi.restoreAllMocks());

describe("ChatPanel (durable runs, ADR 0093)", () => {
  it("(a) a cluster turn calls createRun (not invoke) with the threaded conversationId and finalizes the traceId", async () => {
    const calls = recordingFetch();
    renderChat(false);
    await sendMessage("list environments");

    // The turn goes through the durable create path — NOT the synchronous /invoke.
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/runs" && c.method === "POST")).toBeDefined(),
    );
    expect(calls.find((c) => c.url === "/api/invoke")).toBeUndefined();
    const body = JSON.parse(calls.find((c) => c.url === "/api/runs")!.body);
    expect(body.agent).toBe("scalekit-agent");
    expect(body.namespace).toBe("default");
    expect(body.input).toEqual({ input: "list environments" });
    expect(body.conversationId).toBeTruthy();

    // It finalizes: the agent turn shows the answer + a trace link (from getRun's traceId).
    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("hi there");
    await waitFor(() => expect(screen.getByTestId("open-trace")).toBeInTheDocument());
    expect(screen.getByTestId("open-trace")).toHaveTextContent("trace-xyz");
  });

  it("(b) streams tokens live into the agent turn (the UX upgrade over wait-for-full-response)", async () => {
    recordingFetch({
      frames: [
        "event:token\ndata:Hel\n\n",
        "event:token\ndata:lo\n\n",
        "event:state\ndata:succeeded\n\n",
      ],
      details: [
        {
          id: "run-1",
          status: "succeeded",
          traceId: "trace-xyz",
          messages: [{ role: "assistant", content: "Hello" }],
        },
      ],
    });
    renderChat(false);
    await sendMessage("hi");

    // The streamed tokens accumulate into the turn, and it finalizes to the clean message + trace.
    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("Hello");
    await waitFor(() => expect(screen.getByTestId("open-trace")).toBeInTheDocument());
  });

  it("(c) a consent_required finalize shows Connect → clicking connect RESUMES the same run (not a new createRun)", async () => {
    const popup = { closed: false } as Window;
    const openSpy = vi.fn(() => popup);
    vi.stubGlobal("open", openSpy);

    const calls = recordingFetch({
      details: [
        // First finalize: the run pauses at consent_required.
        {
          id: "run-1",
          status: "requires_action",
          traceId: "trace-consent",
          messages: [{ role: "assistant", content: "please connect your account" }],
          requiresAction: { kind: "consent_required", servers: ["scalekit-mcp-server"] },
        },
        // Second finalize (after resume): the run completes.
        {
          id: "run-1",
          status: "succeeded",
          traceId: "trace-done",
          messages: [{ role: "assistant", content: '{"output":"connected + done"}' }],
        },
      ],
    });
    renderChat(false);
    await sendMessage("list orgs");

    // The consent CTA surfaces for the named server (ADR 0031).
    const connectBtn = await screen.findByTestId("connect-scalekit-mcp-server");
    expect(connectBtn).toHaveTextContent("Connect scalekit-mcp-server");

    const createsBefore = calls.filter((c) => c.url === "/api/runs" && c.method === "POST").length;
    expect(createsBefore).toBe(1); // exactly one create so far — the turn is ONE run

    fireEvent.click(connectBtn);
    await waitFor(() => expect(calls.find((c) => c.url === "/api/mcp/oauth/grant")).toBeDefined());

    // The popup reports back → the turn RESUMES the same run (resumeRun), NOT a second createRun.
    window.dispatchEvent(
      new MessageEvent("message", {
        data: { type: MCP_OAUTH_MESSAGE, server: "scalekit-mcp-server", error: "" },
        origin: window.location.origin,
      }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/runs/run-1/resume" && c.method === "POST")).toBeDefined(),
    );
    // No SECOND create — connect-and-continue stays one run (the ADR 0093 shift).
    expect(calls.filter((c) => c.url === "/api/runs" && c.method === "POST").length).toBe(createsBefore);

    // The resumed run streams to completion.
    await waitFor(() => expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent("connected + done"));
  });

  it("(d) dev-mode keeps the old synchronous /api/invoke path (no cluster run store, ADR 0093 §2)", async () => {
    const calls = recordingFetch();
    renderChat(true); // devMode
    await sendMessage("hi there");

    // In dev-mode the turn uses /api/invoke — the durable run routes are never hit.
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/invoke" && c.method === "POST")).toBeDefined(),
    );
    expect(calls.find((c) => c.url === "/api/runs")).toBeUndefined();
    const body = JSON.parse(calls.find((c) => c.url === "/api/invoke")!.body);
    expect(body.agent).toBe("scalekit-agent");
    expect(body.input).toEqual({ input: "hi there" });
    expect(body.conversationId).toBeTruthy();
    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("dev answer");
  });

  it("(e) a forbidden create renders the ForbiddenInline turn state (viewer can't run)", async () => {
    recordingFetch({
      createOk: false,
      createStatus: 403,
      createJson: { error: "forbidden: not allowed to read the requested agent" },
    });
    renderChat(false);
    await sendMessage("hi");

    // A 403 create renders the explain-and-suggest ForbiddenInline primitive — and never the
    // raw BFF RBAC string, nor a generic error turn.
    expect(await screen.findByText("Not allowed to run this agent")).toBeInTheDocument();
    expect(screen.queryByText(/forbidden: not allowed/)).toBeNull();
    expect(screen.queryByTestId("chat-turn-error")).toBeNull();
  });
});

// ── M151 §6.1 A10 / §7 A10 — what the surface says, and to whom ─────────────
//
// The chatbox is served at an agent's OWN hostname as well as inside the
// console, and the person at that door is not an operator. These pin the two
// halves of that: governance is rendered as governance (the hold hue, the
// §5.26 spine) rather than as a bubble or an amber warning, and operator
// vocabulary — run ids, trace ids, raw server strings — never crosses over.

describe("ChatPanel — governance and audience (M151 A10)", () => {
  it("renders an approval pause as a HELD gate, not a failure — with a way into the run for an operator", async () => {
    recordingFetch({
      details: [
        {
          id: "run-1",
          status: "requires_action",
          traceId: "trace-hold",
          messages: [{ role: "assistant", content: '{"output":"That needs sign-off."}' }],
          requiresAction: { kind: "approval", message: "Issue a $42.00 refund" },
        },
      ],
    });
    renderChat(false);
    await sendMessage("refund order 4021");

    const hold = await screen.findByTestId("chat-hold");
    // It is HELD: nothing is lost, and it is not drawn as an error.
    expect(hold).toHaveTextContent(/A person has to approve this before it goes on/i);
    expect(hold).toHaveTextContent(/held, not failed/i);
    expect(screen.queryByTestId("chat-turn-error")).toBeNull();
    // The turn wears the hold tag, and an operator is given the run to open.
    // The Badge recipe uppercases in CSS, so the DOM carries the sentence case.
    expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent("Held");
    expect(screen.getByRole("link", { name: /Review the hold/i })).toHaveAttribute(
      "href",
      "/runs/run-1",
    );
  });

  it("at an agent's own door the same hold offers NO console link (that route does not exist there)", async () => {
    recordingFetch({
      details: [
        {
          id: "run-1",
          status: "requires_action",
          traceId: "trace-hold",
          messages: [{ role: "assistant", content: '{"output":"That needs sign-off."}' }],
          requiresAction: { kind: "approval", message: "Issue a $42.00 refund" },
        },
      ],
    });
    renderChat(false, "end-user");
    await sendMessage("refund order 4021");

    const hold = await screen.findByTestId("chat-hold");
    expect(hold).toHaveTextContent(/held, not failed/i);
    expect(screen.queryByRole("link", { name: /Review the hold/i })).toBeNull();
    // Nor the trace link, nor a run id anywhere on the surface.
    expect(screen.queryByTestId("open-trace")).toBeNull();
    expect(document.body.textContent).not.toContain("trace-hold");
  });

  it("a failed turn stays IN the transcript with a way to try again (§7 A10)", async () => {
    const calls = recordingFetch({
      createOk: false,
      createStatus: 500,
      createJson: { error: "the run store refused the create" },
    });
    renderChat(false);
    await sendMessage("how long does a refund take?");

    await screen.findByTestId("chat-turn-error");
    // The earlier turn is still on screen — the transcript is never blanked.
    expect(screen.getByTestId("chat-turn-user")).toHaveTextContent(
      "how long does a refund take?",
    );
    const createsBefore = calls.filter((c) => c.url === "/api/runs" && c.method === "POST").length;
    fireEvent.click(screen.getByText("Try again"));
    await waitFor(() =>
      expect(
        calls.filter((c) => c.url === "/api/runs" && c.method === "POST").length,
      ).toBe(createsBefore + 1),
    );
  });

  it("the server's raw words reach an operator and not the person at the assistant's door", async () => {
    recordingFetch({
      createOk: false,
      createStatus: 500,
      createJson: { error: "boom: the run store refused the create" },
    });
    renderChat(false, "end-user");
    await sendMessage("hi");

    const turn = await screen.findByTestId("chat-turn-error");
    expect(turn).toHaveTextContent(/didn't get an answer/i);
    expect(turn).toHaveTextContent(/Nothing above has changed/i);
    expect(document.body.textContent).not.toContain("the run store refused the create");
  });
});
