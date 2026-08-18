/**
 * Memory client — typed sugar over the launcher's :2998 memory endpoint (M5).
 * Parity with `sdk/python/src/ctxmesh/memory.py`.
 *
 * Wire contract (state-layer.md, "The :2998 launcher memory endpoint"):
 *
 *     GET    /memory/{conversationId}          -> JSON array (empty [] if none)
 *     PUT    /memory/{conversationId}          <- JSON-array body (replace)      204
 *     POST   /memory/{conversationId}/append   <- one JSON value (append)        204
 *     GET    /memory/{conversationId}/search?q= -> JSON array (substring match)
 *
 * Long-term (agent-scope, ADR 0045, gated `longtermWired`):
 *
 *     POST   /memory/agent/remember            <- {content, tags?}              202
 *     POST   /memory/agent/search              <- {query, topK, threshold}      200
 */

import { CAPABILITY_HEADER, currentCapability } from "./_capability.js";
import { PlaneConfig } from "./config.js";
import { ConfigError, EndpointError } from "./errors.js";

/** Mirrors the launcher's maxConversationID (state-layer.md). */
const MAX_CONVERSATION_ID = 128;

/**
 * Client-side mirror of the :2998 contract's conversationId rules.
 * Throws ConfigError on an id the endpoint would reject.
 */
function validateConversationId(convId: string): void {
  if (!convId) {
    throw new ConfigError(
      "no conversationId: pass one to the memory call, or set it on the " +
        "client via client.withConversation(id) / CONVERSATION_ID env",
    );
  }
  if (convId.length > MAX_CONVERSATION_ID) {
    throw new ConfigError(`conversationId too long (max ${MAX_CONVERSATION_ID})`);
  }
  for (const ch of convId) {
    const code = ch.codePointAt(0) ?? 0;
    if (ch === "/" || ch === ":" || /\s/.test(ch) || code < 0x20 || code === 0x7f) {
      throw new ConfigError(`conversationId contains disallowed character ${JSON.stringify(ch)}`);
    }
  }
}

/** Perform an HTTP request, returning the response or throwing EndpointError on non-2xx. */
async function httpRequest(
  method: string,
  url: string,
  opts: {
    body?: string;
    headers?: Record<string, string>;
    expect: number[];
    timeoutMs?: number;
  },
): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? 10_000);
  let resp: Response;
  try {
    resp = await fetch(url, {
      method,
      headers: opts.headers,
      body: opts.body,
      signal: controller.signal,
    });
  } catch (err) {
    throw new EndpointError(
      `memory request ${method} ${url} failed: ${(err as Error).message}`,
    );
  } finally {
    clearTimeout(timer);
  }
  if (!opts.expect.includes(resp.status)) {
    const body = await resp.text().catch(() => "");
    throw new EndpointError(
      `memory endpoint returned ${resp.status}: ${body.slice(0, 200)}`,
      { status: resp.status, body },
    );
  }
  return resp;
}

/** Session-memory operations against the launcher's :2998 endpoint. */
export class MemoryClient {
  private readonly config: PlaneConfig;
  private readonly conversationId: string;

  constructor(config: PlaneConfig, conversationId = "") {
    this.config = config;
    this.conversationId = conversationId || config.run.conversationId;
  }

  private requireWired(): void {
    if (!this.config.memoryWired) {
      throw new ConfigError(
        "memory is not wired for this agent: the launcher did not inject " +
          "MEMORY_PORT/MEMORY_BACKEND_ADDR (no MemoryBinding). Bind memory " +
          "to the agent to use client.memory.*",
      );
    }
  }

  private url(conversationId: string | undefined, suffix = ""): string {
    const convId = conversationId ?? this.conversationId;
    validateConversationId(convId);
    // encodeURIComponent is defence-in-depth on top of validation.
    return `${this.config.memoryBaseUrl}/memory/${encodeURIComponent(convId)}${suffix}`;
  }

  /**
   * Headers for a SESSION-memory call. Attaches the run capability (like long-term memory) so the
   * launcher can user-scope per-user session memory (M98, EU1a, ADR 0080). Harmless when the agent
   * is not perUser: the launcher strips the capability before forwarding to the state-layer proxy
   * either way, and only derives X-Memory-User from it when perUser is on.
   */
  private sessionHeaders(extra?: Record<string, string>): Record<string, string> {
    const headers: Record<string, string> = { ...(extra ?? {}) };
    const cap = currentCapability();
    if (cap) {
      headers[CAPABILITY_HEADER] = cap;
    }
    return headers;
  }

  /** GET the full conversation context as a list of entries (empty if none). */
  async get(conversationId?: string): Promise<unknown[]> {
    this.requireWired();
    const resp = await httpRequest("GET", this.url(conversationId), {
      headers: this.sessionHeaders(),
      expect: [200],
    });
    const data: unknown = await resp.json();
    return Array.isArray(data) ? data : [];
  }

  /** PUT (replace) the whole conversation context with a JSON array. */
  async put(entries: unknown[], conversationId?: string): Promise<void> {
    this.requireWired();
    if (!Array.isArray(entries)) {
      throw new ConfigError("memory.put expects an array of entries (the JSON-array body)");
    }
    await httpRequest("PUT", this.url(conversationId), {
      body: JSON.stringify(entries),
      headers: this.sessionHeaders({ "Content-Type": "application/json" }),
      expect: [204],
    });
  }

  /**
   * POST-append one JSON value to the conversation context.
   *
   * messageId (m33.4): the per-hop id to attribute a message entry to. When set it
   * rides X-Message-Id; absent, the endpoint mints one.
   */
  async append(entry: unknown, conversationId?: string, messageId?: string): Promise<void> {
    this.requireWired();
    const extra: Record<string, string> = { "Content-Type": "application/json" };
    if (messageId) {
      extra["X-Message-Id"] = messageId;
    }
    await httpRequest("POST", this.url(conversationId, "/append"), {
      body: JSON.stringify(entry),
      headers: this.sessionHeaders(extra),
      expect: [204],
    });
  }

  /** GET entries matching query (v1 = naive substring; empty q = all). */
  async search(query = "", conversationId?: string): Promise<unknown[]> {
    this.requireWired();
    const url = this.url(conversationId, "/search") + `?q=${encodeURIComponent(query)}`;
    const resp = await httpRequest("GET", url, {
      headers: this.sessionHeaders(),
      expect: [200],
    });
    const data: unknown = await resp.json();
    return Array.isArray(data) ? data : [];
  }

  // ── Long-term (agent-scope) memory (ADR 0045) ──────────────────────────────
  // Persists ACROSS conversations and is retrieved by MEANING (embeddings), unlike
  // the conversation memory above. Both methods relay the run capability header
  // for per-user scoping (the launcher verifies+hashes).

  private requireLongterm(): void {
    if (!this.config.longtermWired) {
      throw new ConfigError(
        "long-term memory is not enabled for this agent: set " +
          "spec.longTermMemory.enabled (the launcher injects " +
          "MEMORY_LONGTERM_ENABLED). Use remember / searchAgent only when bound.",
      );
    }
  }

  private longtermHeaders(): Record<string, string> {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    const cap = currentCapability();
    if (cap) {
      headers[CAPABILITY_HEADER] = cap;
    }
    // traceparent relay is wired in m77.4 alongside the real capability binding.
    return headers;
  }

  /**
   * Store a long-term memory (ADR 0045). Returns immediately; the launcher embeds
   * and persists it in the background (best-effort — never blocks).
   */
  async remember(content: string, tags?: Record<string, string>): Promise<void> {
    this.requireLongterm();
    if (!content || !content.trim()) {
      throw new ConfigError("memory.remember requires non-empty content");
    }
    const body: Record<string, unknown> = { content };
    if (tags) {
      body.tags = tags;
    }
    await httpRequest("POST", `${this.config.memoryBaseUrl}/memory/agent/remember`, {
      body: JSON.stringify(body),
      headers: this.longtermHeaders(),
      expect: [202],
    });
  }

  /**
   * Semantically retrieve the agent's most relevant long-term memories (ADR 0045).
   *
   * Returns a ranked list of `{content: string, score: number}` (score = cosine
   * similarity in [0,1]); empty when nothing clears `threshold`.
   */
  async searchAgent(
    query: string,
    topK = 5,
    threshold = 0.0,
  ): Promise<Array<Record<string, unknown>>> {
    this.requireLongterm();
    const body = { query, topK, threshold };
    const resp = await httpRequest("POST", `${this.config.memoryBaseUrl}/memory/agent/search`, {
      body: JSON.stringify(body),
      headers: this.longtermHeaders(),
      expect: [200],
    });
    const data: unknown = await resp.json();
    if (typeof data === "object" && data !== null && "results" in data) {
      const results = (data as Record<string, unknown>).results;
      return Array.isArray(results) ? (results as Array<Record<string, unknown>>) : [];
    }
    return [];
  }
}
