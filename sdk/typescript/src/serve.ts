/**
 * ctxmesh.serve — the agent serving scaffold (DX-3).
 * Parity with `sdk/python/src/ctxmesh/serve.py`.
 *
 * Every code-first agent used to hand-copy ~100 lines of HTTP handler and, worse, had to
 * *know* the launcher runtime contract by heart: the `POST /invoke` body shape, the
 * `/healthz`/`/readyz` probes, the `$AGENT_PORT` the launcher proxies to, the `traceparent`
 * capture that roots the trace under `agent.invoke`, the autonomous conversation-id mint, and
 * the SSE token envelope. Miss any one and the agent is subtly broken — most dangerously, a
 * hand-rolled loop that forgets `Client.requestScope` relays NO run capability, silently
 * downgrading every tool egress to org/public creds (the DX-2 bug).
 *
 * `serve` encodes all of that ONCE:
 *
 *     import { serve, type InvokeRequest } from "ctxmesh";
 *
 *     async function handle(req: InvokeRequest): Promise<string> {
 *       const answer = await req.client!.model.chat(req... );
 *       return answer.text;
 *     }
 *
 *     serve(handle);   // binds requestScope + trace, serves the contract
 *
 * * `serve(handler)` runs a CUSTOM loop: your `handler(req)` returns a `string` or a
 *   `ManagedResult`. `serve` parses the request, binds `Client.requestScope` (capability +
 *   granted approvals — the DX-2 fix) + roots the trace under the launcher's `agent.invoke`
 *   span, then renders the response envelope.
 * * `serve()` (no handler) runs the STOCK managed loop — the same behaviour the managed-agent
 *   image ships — reading its config from the environment.
 *
 * Streaming: when the caller sends `Accept: text/event-stream` the response is SSE and
 * `req.emitToken(text)` streams a `token` frame; when it does not, `emitToken` is a no-op and
 * the JSON envelope is returned. Either way your handler code is identical.
 *
 * A `node:http` server (stdlib, no framework — mirror Python's ThreadingHTTPServer + the
 * ADR-0002 "no heavy framework" rule).
 */

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";

import * as agentModule from "./agent.js";
import { Client } from "./client.js";
import { ApprovalRequiredError, ConsentRequiredError } from "./errors.js";
import { ManagedConfig, mintConversationId, runManagedLoop } from "./managed.js";
import type { ManagedResult } from "./managed.js";

export type { ManagedResult } from "./managed.js";

/** What a handler may return: a bare answer string, or a full ManagedResult. */
export type HandlerResult = string | ManagedResult;

/** A serve handler: given the parsed InvokeRequest, produce a HandlerResult (sync or async). */
export type Handler = (req: InvokeRequest) => HandlerResult | Promise<HandlerResult>;

/**
 * One parsed `POST /invoke` request handed to a Handler.
 *
 * Everything a custom loop needs, already unpacked and bound — so the handler is pure business
 * logic, never HTTP plumbing.
 */
export interface InvokeRequest {
  /** The user prompt (the `input` field of the request body). */
  input: string;
  /** The inbound request headers (the launcher injected `traceparent` + `X-Conversation-Id`). */
  headers: Record<string, string>;
  /** Approval keys the caller granted for this run (HITL resume, m32.4) — already bound. */
  approvals: string[];
  /** The conversation/thread id: the inbound `X-Conversation-Id`, or a minted per-run id. */
  conversationId?: string;
  /** The SDK client — its capability + approvals are already bound for the handler's life. */
  client?: Client;
  /** Stream a content delta as an SSE `token` frame. A no-op when the caller did not stream. */
  emitToken: (text: string) => void;
}

/** Parse the /invoke body into `{input, approvals}`. Tolerant: non-JSON is the raw prompt. */
export function parseBody(raw: Buffer | string): { input: string; approvals: string[] } {
  const approvals: string[] = [];
  const text = typeof raw === "string" ? raw : raw.toString("utf8");
  let body: unknown;
  try {
    body = JSON.parse(text || "{}");
  } catch {
    return { input: text, approvals };
  }
  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    return { input: String(body), approvals };
  }
  const obj = body as Record<string, unknown>;
  const rawApprovals = obj["approvals"];
  if (Array.isArray(rawApprovals)) {
    for (const a of rawApprovals) approvals.push(String(a));
  }
  return { input: String(obj["input"] ?? ""), approvals };
}

/**
 * Return a minted per-run conversation id (m33.5) when the caller supplied NO session, else
 * `undefined` — so an inbound `X-Conversation-Id` takes precedence. Case-insensitive.
 */
function autonomousConversationId(headers: Record<string, string>): string | undefined {
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === "x-conversation-id" && (value ?? "").trim()) {
      return undefined;
    }
  }
  return mintConversationId();
}

/** The /invoke response envelope (identical shape to every stock agent). */
export function envelope(agentName: string, result: HandlerResult): Record<string, unknown> {
  const managed: ManagedResult =
    typeof result === "string"
      ? { output: result, steps: 1, toolsCalled: [], consentRequired: [] }
      : result;
  const body: Record<string, unknown> = {
    agent: agentName,
    output: managed.output,
    steps: managed.steps,
    tools_called: [...managed.toolsCalled],
    consent_required: [...managed.consentRequired],
  };
  if (managed.approvalRequired) body["approval_required"] = managed.approvalRequired;
  if (managed.guardrailBlocked) body["guardrail_blocked"] = managed.guardrailBlocked;
  if (managed.handoff) body["handoff"] = managed.handoff;
  return body;
}

/**
 * Parse one /invoke, bind the run scope + trace, run *handler*, and return the envelope.
 *
 * The pure core of `serve` (no sockets), so the contract is unit-testable. It binds
 * `Client.requestScope` (capability + approvals — DX-2) AND roots the trace under
 * `agent.invoke` around the handler call. An `ApprovalRequiredError`/`ConsentRequiredError`
 * thrown by the handler becomes a normal HITL/consent envelope (200), not a 5xx — mirrors
 * Python. Any other error propagates (the HTTP layer maps it to a 502).
 */
export async function processInvoke(
  client: Client,
  handler: Handler,
  agentName: string,
  rawBody: Buffer | string,
  headers: Record<string, string>,
  opts: { onToken?: (text: string) => void } = {},
): Promise<Record<string, unknown>> {
  const { input, approvals } = parseBody(rawBody);
  const conversationId = autonomousConversationId(headers);
  const req: InvokeRequest = {
    input,
    headers: { ...headers },
    approvals,
    conversationId,
    client,
    emitToken: opts.onToken ?? (() => undefined),
  };

  // requestScope binds capability + approvals + trace.requestContext for the handler's life.
  const result = await client.requestScope(headers, approvals, async () => {
    try {
      return { kind: "ok" as const, value: await handler(req) };
    } catch (err) {
      // An ApprovalRequiredError/ConsentRequiredError from a CUSTOM handler is a normal HITL
      // /consent outcome (mirror Python) — the stock managed loop already turns these into a
      // ManagedResult, but a hand-rolled handler that throws them gets the same envelope.
      if (err instanceof ApprovalRequiredError) {
        return {
          kind: "ok" as const,
          value: {
            output: `Awaiting approval: ${err.summary}`,
            steps: 1,
            toolsCalled: [],
            consentRequired: [],
            approvalRequired: { key: err.key, summary: err.summary },
          } satisfies ManagedResult,
        };
      }
      if (err instanceof ConsentRequiredError) {
        return {
          kind: "ok" as const,
          value: {
            output: `consent required for ${err.server}`,
            steps: 1,
            toolsCalled: [],
            consentRequired: err.server ? [err.server] : [],
          } satisfies ManagedResult,
        };
      }
      throw err;
    }
  });

  return envelope(agentName, result.value);
}

/** The default handler: the stock config-driven tool-calling loop (`runManagedLoop`). */
function managedHandler(config: ManagedConfig): Handler {
  return (req: InvokeRequest): Promise<ManagedResult> =>
    runManagedLoop(req.client!, config, req.input, {
      headers: req.headers,
      approvals: req.approvals,
      conversationId: req.conversationId,
      onToken: req.emitToken,
    });
}

// ── the HTTP adapter ────────────────────────────────────────────────────────────

function sendJson(res: ServerResponse, code: number, body: Record<string, unknown>): void {
  const payload = Buffer.from(JSON.stringify(body));
  res.writeHead(code, {
    "Content-Type": "application/json",
    "Content-Length": String(payload.length),
  });
  res.end(payload);
}

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return chunks.length ? Buffer.concat(chunks) : Buffer.from("{}");
}

function lowercaseHeaders(req: IncomingMessage): Record<string, string> {
  const headers: Record<string, string> = {};
  for (const [k, v] of Object.entries(req.headers)) {
    if (typeof v === "string") headers[k.toLowerCase()] = v;
    else if (Array.isArray(v)) headers[k.toLowerCase()] = v.join(", ");
  }
  return headers;
}

/**
 * Build the `node:http` request-handler bound to *handler* — the thin adapter over
 * `processInvoke`. Exported so a test can drive the contract on an ephemeral port.
 */
export function makeRequestHandler(
  client: Client,
  handler: Handler,
  agentName: string,
): (req: IncomingMessage, res: ServerResponse) => void {
  return (req: IncomingMessage, res: ServerResponse): void => {
    const method = req.method ?? "GET";
    const path = (req.url ?? "/").split("?")[0];

    if (method === "GET") {
      if (path === "/healthz" || path === "/readyz") {
        sendJson(res, 200, { status: "ok" });
      } else {
        sendJson(res, 404, { error: "not found" });
      }
      return;
    }

    if (method !== "POST" || path !== "/invoke") {
      sendJson(res, 404, { error: "not found" });
      return;
    }

    const headers = lowercaseHeaders(req);
    const wantsStream = (headers["accept"] ?? "").includes("text/event-stream");

    void (async () => {
      const raw = await readBody(req);
      if (wantsStream) {
        await stream(res, client, handler, agentName, raw, headers);
        return;
      }
      try {
        const body = await processInvoke(client, handler, agentName, raw, headers);
        sendJson(res, 200, body);
      } catch (err) {
        // Report, never crash the server (mirror Python's 502 branch).
        sendJson(res, 502, { agent: agentName, error: (err as Error).message });
      }
    })();
  };
}

/** Stream token frames then a terminal `done`/`error` frame (m32.7). */
async function stream(
  res: ServerResponse,
  client: Client,
  handler: Handler,
  agentName: string,
  raw: Buffer,
  headers: Record<string, string>,
): Promise<void> {
  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
  });

  const emit = (obj: Record<string, unknown>): void => {
    res.write(`data: ${JSON.stringify(obj)}\n\n`);
  };

  try {
    const body = await processInvoke(client, handler, agentName, raw, headers, {
      onToken: (text: string) => emit({ type: "token", text }),
    });
    body["type"] = "done";
    emit(body);
  } catch (err) {
    // Report as an error frame, never crash.
    emit({ type: "error", agent: agentName, error: (err as Error).message });
  } finally {
    res.end();
  }
}

/** Options for `serve`. */
export interface ServeOptions {
  client?: Client;
  agentName?: string;
  port?: number;
  /** Bind address — defaults to all interfaces (the pod's only listener, fronted by the launcher). */
  host?: string;
}

/**
 * Serve an agent over the launcher runtime contract — the one call a code-first agent needs.
 *
 * *handler* is your loop (`handler(req) -> string | ManagedResult`); omit it to run the stock
 * managed loop (config from the environment). *client* defaults to `agent.fromEnv()` (fails fast
 * with `NotInPodError` off-plane). *agentName* / *port* default to `$AGENT_NAME` / `$AGENT_PORT`.
 * Returns the started `Server` (blocks the process serving, like Python's `serve_forever`).
 *
 * Endpoints: `POST /invoke` (`{input, approvals?}` → the envelope, or SSE when
 * `Accept: text/event-stream`), `GET /healthz`/`/readyz` → 200.
 */
export function serve(handler?: Handler, opts: ServeOptions = {}): Server {
  const client = opts.client ?? agentModule.fromEnv();
  const boundHandler = handler ?? managedHandler(ManagedConfig.fromEnv());
  const agentName = opts.agentName ?? process.env["AGENT_NAME"] ?? "agent";
  const port = opts.port ?? Number.parseInt(process.env["AGENT_PORT"] ?? "8080", 10);
  const host = opts.host ?? "0.0.0.0";

  const server = createServer(makeRequestHandler(client, boundHandler, agentName));
  server.listen(port, host, () => {
    // Start-up banner, mirror Python's print.
    console.log(`ctxmesh agent ${JSON.stringify(agentName)} listening on :${port}`);
  });
  return server;
}
