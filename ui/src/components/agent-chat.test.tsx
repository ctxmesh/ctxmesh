import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ChatPanel } from "@/components/agent-chat";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { DevModeContext } from "@/lib/dev-mode";
import { MCP_CONSENT_READY, MCP_CONSENT_TICKET, MCP_OAUTH_MESSAGE } from "@/lib/oauth-popup";
import {
  chunked,
  DESCENDANT_ANSWER,
  DESCENDANT_PAUSE_SEQ,
  descendantGolden,
  golden,
  GOLDEN_REQUIRES_ACTION_SEQ,
  GOLDEN_TEXT,
  RUN_DESCENDANT_PAUSED,
} from "@/test/run-events-golden";

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
  lastEventId?: string | null;
  signal?: AbortSignal;
}

// sseBody wraps SSE frame strings in a ReadableStream-like body (getReader) so the durable run
// event stream (openRunStream) drives deterministically — mirrors the Playground test helper.
// With `openUntil`, the body stays open after the frames, as the server's does for a run waiting
// in requires_action, until that signal aborts.
function sseBody(frames: string[], openUntil?: AbortSignal) {
  const enc = new TextEncoder();
  let i = 0;
  return {
    getReader() {
      return {
        read() {
          if (i < frames.length)
            return Promise.resolve({ value: enc.encode(frames[i++]), done: false });
          if (openUntil)
            return new Promise((_resolve, reject) => {
              const abort = () => reject(new DOMException("aborted", "AbortError"));
              if (openUntil.aborted) abort();
              else openUntil.addEventListener("abort", abort, { once: true });
            });
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
  // A stream per /events request, given the request's Last-Event-ID; `open` keeps it from closing.
  stream?: (lastEventId: string | null) => { frames: string[]; open?: boolean };
  details?: unknown[]; // successive GET /api/runs/{id} responses (consent → success)
  // Successive GET /api/runs/{id} responses per run id, for a turn that reads more than one run.
  detailsById?: Record<string, unknown[]>;
  createOk?: boolean;
  createStatus?: number;
  createJson?: unknown;
  // The authorization URL the direct consent begin returns.
  grantURL?: string;
}

function recordingFetch(run: RunMock = {}) {
  const calls: Captured[] = [];
  let detailIdx = 0;
  const byIdIdx: Record<string, number> = {};
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      const lastEventId = new Headers(init?.headers).get("Last-Event-ID");
      calls.push({ url: path, method, body, lastEventId, signal: init?.signal ?? undefined });
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
      if (path.endsWith("/events")) {
        const scripted = run.stream?.(lastEventId);
        return Promise.resolve({
          ok: true,
          status: 200,
          body: sseBody(
            scripted?.frames ??
              run.frames ?? [
                'event:message\ndata:{"output":"hi there"}\n\n',
                "event:state\ndata:succeeded\n\n",
              ],
            scripted?.open ? (init?.signal ?? undefined) : undefined,
          ),
        } as unknown as Response);
      }
      // POST /api/runs/{id}/resume — the consent connect-and-continue (no decision).
      if (path.startsWith("/api/runs/") && path.endsWith("/resume"))
        return j({ id: "run-1", status: "running" });
      // GET /api/runs/{id} — successive structured details (consent → success).
      const byId = run.detailsById?.[path.slice("/api/runs/".length)];
      if (path.startsWith("/api/runs/") && byId) {
        const id = path.slice("/api/runs/".length);
        const i = byIdIdx[id] ?? 0;
        byIdIdx[id] = i + 1;
        return j(byId[Math.min(i, byId.length - 1)]);
      }
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
          json: async () => ({ authorizationURL: run.grantURL ?? "https://as.example/authorize?x=1", state: "st" }),
          text: async () => "",
        } as Response);
      // Consent-ticket mint on an agent hostname (ADR 0040 amendment): 201 + the console page.
      if (path === "/api/mcp/oauth/ticket" && method === "POST")
        return Promise.resolve({
          ok: true,
          status: 201,
          json: async () => ({
            ticket: "tkt-secret-123",
            connectURL: "https://console.example.com/connect",
            expiresIn: 60,
          }),
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

function renderChat(
  devMode = false,
  audience: "operator" | "end-user" = "operator",
  modelRoute?: { status: string; reason: string; message: string },
) {
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
              modelRoute={modelRoute}
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
    // The server's bytes for the run's second leg up to its last token, with the stream still open:
    // what the turn shows can only have come from the tokens, since the run has not been re-read.
    const calls = recordingFetch({
      stream: () => ({ frames: chunked(golden(GOLDEN_REQUIRES_ACTION_SEQ, 9), 11), open: true }),
    });
    renderChat(false);
    await sendMessage("hi");

    await waitFor(() =>
      expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent(
        "MOCK_TOOL_OK echo_tool returned: FIXTURE_ECHO: ping Done.",
      ),
    );
    expect(screen.getByTestId("chat-turn-agent")).not.toHaveTextContent("{");
    expect(calls.filter((c) => c.url === "/api/runs/run-1" && c.method === "GET")).toHaveLength(0);
  });

  it("(b2) a closed stream finalizes to the run's message and trace", async () => {
    recordingFetch({
      stream: () => ({ frames: [golden(GOLDEN_REQUIRES_ACTION_SEQ)] }),
      details: [
        {
          id: "run-1",
          status: "succeeded",
          traceId: "trace-xyz",
          messages: [{ role: "assistant", content: GOLDEN_TEXT }],
        },
      ],
    });
    renderChat(false);
    await sendMessage("hi");

    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("FIXTURE_ECHO: ping");
    await waitFor(() => expect(screen.getByTestId("open-trace")).toBeInTheDocument());
  });

  // The server leaves the stream open while a run waits in requires_action, so the chat must act on
  // the `state: requires_action` frame itself. Both pauses below arrive on the server's own bytes.
  it("(f) a run that pauses for consent mid-stream shows Connect, and resuming continues after the pause", async () => {
    const popup = { closed: false } as Window;
    vi.stubGlobal("open", vi.fn(() => popup));
    const calls = recordingFetch({
      stream: (lastEventId) =>
        lastEventId === null
          ? { frames: chunked(golden(0, GOLDEN_REQUIRES_ACTION_SEQ), 13), open: true }
          : { frames: [golden(Number(lastEventId))] },
      details: [
        {
          id: "run-1",
          status: "requires_action",
          messages: [],
          requiresAction: { kind: "consent_required", servers: ["echo-mcp"] },
        },
        {
          id: "run-1",
          status: "succeeded",
          traceId: "trace-done",
          messages: [{ role: "assistant", content: GOLDEN_TEXT }],
        },
      ],
    });
    renderChat(false);
    await sendMessage("ping");

    const connect = await screen.findByTestId("connect-echo-mcp");
    expect(connect).toHaveTextContent("Connect echo-mcp");
    // The tokens streamed before the pause stay on the turn, and the chat closed the stream itself.
    expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent("Calling echo_tool.");
    const first = calls.filter((c) => c.url === "/api/runs/run-1/events");
    expect(first).toHaveLength(1);
    expect(first[0].lastEventId).toBeNull();
    expect(first[0].signal?.aborted).toBe(true);

    fireEvent.click(connect);
    await waitFor(() => expect(calls.find((c) => c.url === "/api/mcp/oauth/grant")).toBeDefined());
    window.dispatchEvent(
      new MessageEvent("message", {
        data: { type: MCP_OAUTH_MESSAGE, server: "echo-mcp", error: "" },
        origin: window.location.origin,
      }),
    );

    // The resumed run streams from after the pause; replaying it would end the turn on the spot.
    await waitFor(() =>
      expect(calls.filter((c) => c.url === "/api/runs/run-1/events")).toHaveLength(2),
    );
    expect(calls.filter((c) => c.url === "/api/runs/run-1/events")[1].lastEventId).toBe(
      String(GOLDEN_REQUIRES_ACTION_SEQ),
    );
    await waitFor(() => expect(screen.getByTestId("open-trace")).toHaveTextContent("trace-done"));
    expect(screen.queryByTestId("connect-echo-mcp")).not.toBeInTheDocument();
  });

  it("(g) a run that pauses for an approval mid-stream shows the hold", async () => {
    const calls = recordingFetch({
      stream: () => ({ frames: [golden(0, GOLDEN_REQUIRES_ACTION_SEQ)], open: true }),
      details: [
        {
          id: "run-1",
          status: "requires_action",
          messages: [],
          requiresAction: { kind: "approval", key: "tool:echo_tool", message: "approve the step" },
        },
      ],
    });
    renderChat(false);
    await sendMessage("ping");

    expect(await screen.findByTestId("chat-hold")).toHaveTextContent(
      "A person has to approve this before it goes on",
    );
    expect(screen.getByRole("link", { name: "Review the hold on run run-1" })).toBeInTheDocument();
    expect(calls.find((c) => c.url === "/api/runs/run-1/events")?.signal?.aborted).toBe(true);
  });

  // A supervisor that delegated waits on its sub-run, so when the sub-run pauses the supervisor's
  // stream only names it (`descendant-requires-action`). These replay the server's bytes for that,
  // and the paused sub-run's detail as its handler served it.
  const SUPERVISOR = "run-supervisor";
  const SUB = RUN_DESCENDANT_PAUSED.id;
  const supervisorDone = {
    id: SUPERVISOR,
    status: "succeeded",
    traceId: "trace-supervisor",
    messages: [{ role: "assistant", content: DESCENDANT_ANSWER }],
  };
  const supervisorWaiting = { id: SUPERVISOR, status: "waiting", messages: [] };
  const supervisorStream = (lastEventId: string | null) =>
    lastEventId === null
      ? { frames: chunked(descendantGolden(0, DESCENDANT_PAUSE_SEQ), 17), open: true }
      : { frames: [descendantGolden(Number(lastEventId))] };

  it("(h) a sub-agent's consent pause shows the same Connect, and connecting resumes the sub-run until the turn ends", async () => {
    const popup = { closed: false } as Window;
    vi.stubGlobal("open", vi.fn(() => popup));
    const calls = recordingFetch({
      createJson: { id: SUPERVISOR, status: "queued" },
      stream: supervisorStream,
      detailsById: {
        [SUPERVISOR]: [supervisorWaiting, supervisorDone],
        [SUB]: [RUN_DESCENDANT_PAUSED],
      },
    });
    renderChat(false);
    await sendMessage("research ctxmesh");

    const connect = await screen.findByTestId("connect-search-mcp");
    expect(connect).toHaveTextContent("Connect search-mcp");
    expect(screen.getByTestId("chat-consent")).toHaveTextContent("Connect your account to continue");
    expect(screen.getByTestId("chat-sub-agent-pause")).toHaveTextContent("It was asked by researcher");
    const first = calls.filter((c) => c.url === `/api/runs/${SUPERVISOR}/events`);
    expect(first).toHaveLength(1);
    expect(first[0].signal?.aborted).toBe(true);

    fireEvent.click(connect);
    await waitFor(() => expect(calls.find((c) => c.url === "/api/mcp/oauth/grant")).toBeDefined());
    // The grant is scoped to the agent being chatted with: its sub-runs share its trust boundary.
    expect(JSON.parse(calls.find((c) => c.url === "/api/mcp/oauth/grant")!.body)).toMatchObject({
      server: "search-mcp",
      agent: "scalekit-agent",
    });
    window.dispatchEvent(
      new MessageEvent("message", {
        data: { type: MCP_OAUTH_MESSAGE, server: "search-mcp", error: "" },
        origin: window.location.origin,
      }),
    );

    // The sub-run is the one resumed; the supervisor is followed on from after the pause.
    await waitFor(() =>
      expect(calls.find((c) => c.url === `/api/runs/${SUB}/resume` && c.method === "POST")).toBeDefined(),
    );
    expect(calls.find((c) => c.url === `/api/runs/${SUPERVISOR}/resume`)).toBeUndefined();
    expect(calls.filter((c) => c.url === "/api/runs" && c.method === "POST")).toHaveLength(1);
    await waitFor(() =>
      expect(calls.filter((c) => c.url === `/api/runs/${SUPERVISOR}/events`)).toHaveLength(2),
    );
    expect(calls.filter((c) => c.url === `/api/runs/${SUPERVISOR}/events`)[1].lastEventId).toBe(
      String(DESCENDANT_PAUSE_SEQ),
    );
    await waitFor(() => expect(screen.getByTestId("open-trace")).toHaveTextContent("trace-supervisor"));
    expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent(DESCENDANT_ANSWER);
    expect(screen.queryByTestId("connect-search-mcp")).not.toBeInTheDocument();
    expect(screen.queryByTestId("chat-sub-agent-pause")).not.toBeInTheDocument();
  });

  it("(i) a sub-agent's approval pause shows the hold, pointing at the sub-run", async () => {
    recordingFetch({
      createJson: { id: SUPERVISOR, status: "queued" },
      stream: supervisorStream,
      detailsById: {
        [SUPERVISOR]: [supervisorWaiting],
        [SUB]: [
          {
            ...RUN_DESCENDANT_PAUSED,
            requiresAction: { kind: "approval", key: "tool:search", message: "approve the search" },
          },
        ],
      },
    });
    renderChat(false);
    await sendMessage("research ctxmesh");

    expect(await screen.findByTestId("chat-hold")).toHaveTextContent(
      "A person has to approve this before it goes on",
    );
    expect(screen.getByRole("link", { name: `Review the hold on run ${SUB}` })).toBeInTheDocument();
    expect(screen.getByTestId("chat-sub-agent-pause")).toHaveTextContent("researcher");
  });

  it("(j) a sub-run that has already moved on asks nothing, and the turn follows the supervisor to its answer", async () => {
    const calls = recordingFetch({
      createJson: { id: SUPERVISOR, status: "queued" },
      stream: () => ({ frames: chunked(descendantGolden(), 17) }),
      detailsById: {
        [SUPERVISOR]: [supervisorDone],
        [SUB]: [{ ...RUN_DESCENDANT_PAUSED, status: "succeeded", requiresAction: undefined }],
      },
    });
    renderChat(false);
    await sendMessage("research ctxmesh");

    await waitFor(() => expect(screen.getByTestId("open-trace")).toHaveTextContent("trace-supervisor"));
    expect(screen.getByTestId("chat-turn-agent")).toHaveTextContent(DESCENDANT_ANSWER);
    expect(screen.queryByTestId("chat-consent")).not.toBeInTheDocument();
    expect(calls.filter((c) => c.url === `/api/runs/${SUPERVISOR}/events`)).toHaveLength(1);
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

  it("opens no window and leaves the chat in place for a non-http(s) authorization URL", async () => {
    for (const grantURL of ["javascript:alert(document.domain)", "data:text/html,<script>alert(1)</script>", "/relative"]) {
      for (const popupBlocked of [false, true]) {
        const openSpy = vi.fn(() => (popupBlocked ? null : ({ closed: false } as Window)));
        vi.stubGlobal("open", openSpy);
        recordingFetch({
          grantURL,
          details: [
            {
              id: "run-1",
              status: "requires_action",
              traceId: "trace-consent",
              messages: [{ role: "assistant", content: "please connect your account" }],
              requiresAction: { kind: "consent_required", servers: ["scalekit-mcp-server"] },
            },
          ],
        });
        const { unmount } = renderChat(false);
        await sendMessage("list orgs");
        const before = window.location.href;

        fireEvent.click(await screen.findByTestId("connect-scalekit-mcp-server"));
        expect(await screen.findByText(/sign-in address for this server is not valid/)).toBeInTheDocument();
        expect(openSpy).not.toHaveBeenCalled();
        expect(window.location.href).toBe(before);
        unmount();
        vi.unstubAllGlobals();
      }
    }
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

// A route the gateway does not serve yet fails every message, so Send waits for it; an edited route is
// still served by its previous version, so Send stays open (ADR 0151).
describe("ChatPanel — the agent's model route", () => {
  it("holds Send while a new route is not yet served, and says why", async () => {
    renderChat(false, "operator", { status: "False", reason: "NotYetServed", message: "m" });
    const input = await screen.findByTestId("chat-input");
    fireEvent.change(input, { target: { value: "hello" } });
    expect(screen.getByTestId("chat-send")).toBeDisabled();
    expect(screen.getByTestId("chat-route-note")).toHaveTextContent("loading this agent's model");
  });

  it("does not hold Send while an edited route rolls out", async () => {
    renderChat(false, "operator", { status: "False", reason: "GatewayRolling", message: "rolling" });
    const input = await screen.findByTestId("chat-input");
    fireEvent.change(input, { target: { value: "hello" } });
    expect(screen.getByTestId("chat-send")).toBeEnabled();
  });

  it("shows nothing when the route is served", async () => {
    renderChat(false, "operator", { status: "True", reason: "Served", message: "ok" });
    await screen.findByTestId("chat-input");
    expect(screen.queryByTestId("chat-route-note")).toBeNull();
  });
});

// On an agent hostname with a console origin configured, consent begins on the console (ADR 0040
// amendment): the chat mints a ticket, opens the console's /connect page, and answers that page's
// READY with the ticket, addressed to the console origin only.
describe("ChatPanel consent on an agent hostname", () => {
  const consoleOrigin = "https://console.example.com";
  let meta: HTMLMetaElement;

  beforeEach(() => {
    meta = document.createElement("meta");
    meta.name = "mcp-callback-origin";
    meta.content = consoleOrigin;
    document.head.appendChild(meta);
  });
  afterEach(() => {
    meta.remove();
    vi.unstubAllGlobals();
  });

  function message(data: unknown, origin: string, source: unknown): MessageEvent {
    const e = new MessageEvent("message", { data, origin });
    Object.defineProperty(e, "source", { value: source });
    return e;
  }

  async function reachConsent() {
    const calls = recordingFetch({
      details: [
        {
          id: "run-1",
          status: "requires_action",
          traceId: "trace-consent",
          messages: [{ role: "assistant", content: "please connect your account" }],
          requiresAction: { kind: "consent_required", servers: ["scalekit-mcp-server"] },
        },
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
    return { calls, connect: await screen.findByTestId("connect-scalekit-mcp-server") };
  }

  it("mints a ticket, opens /connect, and answers only the console page's ready", async () => {
    const popup = { closed: false, postMessage: vi.fn() };
    const openSpy = vi.fn((_url: string, _name?: string, _features?: string) => popup as unknown as Window);
    vi.stubGlobal("open", openSpy);
    const { calls, connect } = await reachConsent();

    fireEvent.click(connect);
    await waitFor(() => expect(openSpy).toHaveBeenCalled());
    const mint = calls.find((c) => c.url === "/api/mcp/oauth/ticket");
    expect(JSON.parse(mint!.body)).toEqual({
      server: "scalekit-mcp-server",
      namespace: "default",
      agent: "scalekit-agent",
    });
    expect(calls.find((c) => c.url === "/api/mcp/oauth/grant")).toBeUndefined();
    expect(openSpy.mock.calls[0][0]).toBe(`${consoleOrigin}/connect`);
    expect(String(openSpy.mock.calls[0][0])).not.toContain("tkt-secret-123");

    // A ready from any other origin, or from a window that is not the popup, gets nothing.
    window.dispatchEvent(message({ type: MCP_CONSENT_READY }, "https://evil.example", popup));
    window.dispatchEvent(message({ type: MCP_CONSENT_READY }, window.location.origin, popup));
    window.dispatchEvent(message({ type: MCP_CONSENT_READY }, consoleOrigin, {}));
    expect(popup.postMessage).not.toHaveBeenCalled();

    // The console page's ready gets the ticket, to the console origin exactly, once.
    window.dispatchEvent(message({ type: MCP_CONSENT_READY }, consoleOrigin, popup));
    window.dispatchEvent(message({ type: MCP_CONSENT_READY }, consoleOrigin, popup));
    expect(popup.postMessage).toHaveBeenCalledTimes(1);
    expect(popup.postMessage).toHaveBeenCalledWith(
      { type: MCP_CONSENT_TICKET, ticket: "tkt-secret-123" },
      consoleOrigin,
    );

    // The callback's relay from the console resumes the same run.
    window.dispatchEvent(
      message({ type: MCP_OAUTH_MESSAGE, server: "scalekit-mcp-server", error: "" }, consoleOrigin, popup),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/runs/run-1/resume" && c.method === "POST")).toBeDefined(),
    );
  });

  it("says so when the popup is blocked, and never redirects the chat away", async () => {
    vi.stubGlobal("open", vi.fn(() => null));
    const { connect } = await reachConsent();
    const before = window.location.href;

    fireEvent.click(connect);
    expect(await screen.findByText(/blocked the sign-in window/)).toBeInTheDocument();
    expect(window.location.href).toBe(before);
  });
});
