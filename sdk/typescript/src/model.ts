/**
 * Model client — the OpenAI-compatible model gateway (M2/M8).
 * Parity with `sdk/python/src/ctxmesh/model.py`.
 *
 * `client.model.chat(model, messages, opts?)` POSTs to
 * `$MODEL_GATEWAY_URL/chat/completions` and returns the assistant's completion.
 *
 * Wire contract (model-gateway.md):
 *
 *     POST $MODEL_GATEWAY_URL/chat/completions
 *       { "model": <route>, "messages": [...], ...opts }
 *     -> 200 { "choices":[{"message":{"content": <text>}}],
 *              "usage":{"prompt_tokens","completion_tokens","total_tokens"} }
 *
 * A non-2xx (e.g. the budget proxy's 402 over-cap, or a 502 upstream) surfaces as an
 * EndpointError carrying the status. A guardrail-blocked 403 surfaces as
 * GuardrailBlockedError.
 */

import { CAPABILITY_HEADER, currentCapability } from "./_capability.js";
import { PlaneConfig } from "./config.js";
import { ConfigError, EndpointError, GuardrailBlockedError } from "./errors.js";

/** A model round-trip is a remote provider call — generous vs the localhost ops. */
const CHAT_TIMEOUT_MS = 60_000;

/** A parsed tool call from an assistant turn. */
export interface ToolCall {
  id: string;
  type: string;
  function: {
    name: string;
    arguments: string;
  };
}

/** Normalized usage block from the gateway response. */
export interface ChatUsage {
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
}

/**
 * The parsed result of `model.chat` — the completion text plus usage.
 *
 * `text` is the assistant message content (the common case a loop wants). On a
 * tool-calling turn the OpenAI wire sets `content: null` and carries a `tool_calls`
 * array instead; `text` is then `""` and `toolCalls` returns the calls. `usage`
 * is the normalized token counts when the gateway returned them. `raw` is the full
 * decoded response for callers that need more.
 */
export class ChatResponse {
  readonly text: string;
  readonly usage: ChatUsage;
  readonly model: string;
  readonly raw: Record<string, unknown>;

  constructor(
    text: string,
    usage: ChatUsage,
    model: string,
    raw: Record<string, unknown>,
  ) {
    this.text = text;
    this.usage = usage;
    this.model = model;
    this.raw = raw;
  }

  toString(): string {
    return this.text;
  }

  /** The raw assistant message object (`choices[0].message`), or `{}`. */
  get message(): Record<string, unknown> {
    return assistantMessage(this.raw);
  }

  /**
   * The assistant's `tool_calls` for this turn (`[]` when none).
   *
   * Each entry is `{id, type, function: {name, arguments: <json-string>}}` — the
   * exact shape the m14.2 tool-call mock emits on turn 1.
   */
  get toolCalls(): ToolCall[] {
    const msg = assistantMessage(this.raw);
    const calls = msg.tool_calls;
    if (Array.isArray(calls)) {
      return calls.filter((c): c is ToolCall => typeof c === "object" && c !== null);
    }
    return [];
  }

  /** True when this turn asked to call one or more tools. */
  get hasToolCalls(): boolean {
    return this.toolCalls.length > 0;
  }
}

/** Chat-completion calls against the model gateway. */
export class ModelClient {
  private readonly config: PlaneConfig;

  constructor(config: PlaneConfig) {
    this.config = config;
  }

  /**
   * POST a chat completion; return a `ChatResponse`.
   *
   * `model` is the gateway route (e.g. `gpt-4o-mini`); `messages` is the
   * OpenAI-style `[{role, content}, ...]` list; `opts` are extra body fields passed
   * through verbatim (`temperature`, `max_tokens`, …).
   *
   * Raises `ConfigError` when the gateway URL is not wired and `EndpointError`
   * (with `.status`) for a non-200 gateway response (budget 402, upstream 502, …).
   * A guardrail-blocked 403 raises `GuardrailBlockedError`.
   */
  async chat(
    model: string,
    messages: Array<Record<string, unknown>>,
    opts: Record<string, unknown> = {},
  ): Promise<ChatResponse> {
    const baseUrl = this.config.modelGatewayUrl;
    if (!baseUrl) {
      throw new ConfigError(
        "model gateway is not wired: MODEL_GATEWAY_URL is unset. The " +
          "launcher injects it in-pod; for offline use set it on the " +
          "PlaneConfig (agent.fromConfig).",
      );
    }
    if (!Array.isArray(messages)) {
      throw new ConfigError("model.chat expects messages as an array of {role,content} objects");
    }

    // `timeout` is a client-side transport concern — pop it out so it never leaks into
    // the request body.
    const bodyOpts = { ...opts };
    const rawTimeout = bodyOpts.timeout;
    delete bodyOpts.timeout;
    const timeoutMs =
      typeof rawTimeout === "number" && isFinite(rawTimeout) ? rawTimeout : CHAT_TIMEOUT_MS;

    const payload: Record<string, unknown> = { model, messages, ...bodyOpts };

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    let resp: Response;
    try {
      resp = await fetch(`${baseUrl}/chat/completions`, {
        method: "POST",
        headers: this.headers(),
        body: JSON.stringify(payload),
        signal: controller.signal,
      });
    } catch (err) {
      throw new EndpointError(
        `model gateway request failed: ${(err as Error).message}`,
      );
    } finally {
      clearTimeout(timer);
    }

    if (resp.status !== 200) {
      const body = await resp.text().catch(() => "");
      const err = new EndpointError(
        `model gateway returned ${resp.status}: ${body.slice(0, 200)}`,
        { status: resp.status, body },
      );
      raiseIfGuardrailBlocked(err);
      throw err;
    }

    const data: unknown = await resp.json();
    if (typeof data !== "object" || data === null) {
      throw new EndpointError(
        `model gateway returned a non-object body`,
        { status: 200 },
      );
    }
    const dataObj = data as Record<string, unknown>;

    const text = completionText(dataObj);
    const rawUsage = dataObj.usage;
    const usage = normalizeUsage(typeof rawUsage === "object" && rawUsage !== null ? rawUsage as Record<string, unknown> : {});
    const resolvedModel =
      typeof dataObj.model === "string" ? dataObj.model : model;

    return new ChatResponse(text, usage, resolvedModel, dataObj);
  }

  private headers(): Record<string, string> {
    // The gateway is OpenAI-compatible and expects a bearer token; the launcher
    // injects the master key in-pod. When absent (offline/mock) we send a placeholder.
    const key = this.config.modelGatewayKey || "sk-agent-engine";
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Authorization: `Bearer ${key}`,
    };
    // Relay the invoking user's run capability (ADR 0030 §3, m66.7) on the model call
    // — lets the launcher's gateway proxy enforce per-user OBO rate/abuse limits.
    const capability = currentCapability();
    if (capability) {
      headers[CAPABILITY_HEADER] = capability;
    }
    return headers;
  }
}

// ── helpers ──────────────────────────────────────────────────────────────────

function assistantMessage(data: Record<string, unknown>): Record<string, unknown> {
  const choices = data.choices;
  if (!Array.isArray(choices) || choices.length === 0) return {};
  const first = choices[0];
  if (typeof first !== "object" || first === null) return {};
  const message = (first as Record<string, unknown>).message;
  return typeof message === "object" && message !== null
    ? (message as Record<string, unknown>)
    : {};
}

function firstChoice(data: Record<string, unknown>): Record<string, unknown> {
  const choices = data.choices;
  if (!Array.isArray(choices) || choices.length === 0) {
    throw new EndpointError("model gateway response has no choices");
  }
  const first = choices[0];
  if (typeof first !== "object" || first === null) {
    throw new EndpointError("model gateway choice is not an object");
  }
  return first as Record<string, unknown>;
}

function completionText(data: Record<string, unknown>): string {
  const first = firstChoice(data);
  const message = first.message;
  if (typeof message === "object" && message !== null) {
    const msg = message as Record<string, unknown>;
    const content = msg.content;
    if (typeof content === "string") return content;
    // A tool-calling turn: content is null, tool_calls carries the intent.
    if (msg.tool_calls) return "";
  }
  // Some gateways/streamed shapes put the text on `text`; accept that too.
  if (typeof first.text === "string") return first.text;
  throw new EndpointError("model gateway choice has no message content");
}

function normalizeUsage(raw: Record<string, unknown>): ChatUsage {
  return {
    promptTokens: typeof raw.prompt_tokens === "number" ? raw.prompt_tokens : 0,
    completionTokens: typeof raw.completion_tokens === "number" ? raw.completion_tokens : 0,
    totalTokens: typeof raw.total_tokens === "number" ? raw.total_tokens : 0,
  };
}

/**
 * Inspect `exc` and, when it is a guardrail_blocked 403, raise GuardrailBlockedError.
 *
 * The launcher's guardrail proxy (M66, ADR 0059 §8) returns HTTP 403 with a typed body:
 *   {"error": {"type": "guardrail_blocked", "detector": "…", "scan_point": "…"}}
 */
function raiseIfGuardrailBlocked(exc: EndpointError): void {
  if (exc.status !== 403 || !exc.body) return;
  let data: unknown;
  try {
    data = JSON.parse(exc.body);
  } catch {
    return;
  }
  if (typeof data !== "object" || data === null) return;
  const errObj = (data as Record<string, unknown>).error;
  if (typeof errObj !== "object" || errObj === null) return;
  const errData = errObj as Record<string, unknown>;
  if (errData.type !== "guardrail_blocked") return;
  const detector = typeof errData.detector === "string" ? errData.detector : "";
  const scanPoint = typeof errData.scan_point === "string" ? errData.scan_point : "";
  throw new GuardrailBlockedError(
    `blocked by guardrail policy: detector=${JSON.stringify(detector)} scan_point=${JSON.stringify(scanPoint)}`,
    { detector, scanPoint: scanPoint, status: 403, body: exc.body },
  );
}
