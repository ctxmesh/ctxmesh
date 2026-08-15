/**
 * Client for the run-oriented execution contract (ADR 0034, m32.9).
 * Parity with `sdk/python/src/ctxmesh/runs.py`.
 *
 * A {@link RunsClient} drives the BFF run API — create a durable run, poll it, stream its events,
 * resume it (consent connected / approval granted-or-denied), or cancel it. It is the programmatic
 * counterpart to the console chat: an app, CLI, or another service creates and follows runs the same
 * way the UI does.
 *
 * Runs are **caller-authenticated** (ADR 0011): the client sends a bearer token, exactly like the
 * console. This is deliberately NOT the in-pod launcher plane (memory/tools/model) — those are
 * localhost and unauthenticated; a run is a control-plane resource. So `RunsClient` is a STANDALONE
 * client (constructed with the console/BFF origin + a token), not an attribute of `Client`.
 *
 *     const runs = new RunsClient("https://console.example", { token: myToken });
 *     const run = await runs.create({ agent: "researcher", input: { input: "summarise Q3" } });
 *     for await (const event of runs.stream(run.id)) {
 *       if (event.kind === "token") process.stdout.write(event.data);
 *     }
 *     const final = await runs.get(run.id);
 */

import { EndpointError } from "./errors.js";

/** Terminal run statuses (mirror internal/run.Status): no further transition, so run() stops polling. */
const TERMINAL = new Set(["succeeded", "failed", "cancelled", "expired"]);

/** SSE read timeout (ms, FUNC-7): a run can idle between events for a slow model turn or while parked
 * at requires_action — far longer than a normal localhost op — so the stream read waits generously. */
const STREAM_READ_TIMEOUT_MS = 120_000;

/** A run's current state (a subset of the BFF run DTO the client needs). */
export class Run {
  readonly id: string;
  readonly status: string;
  /** The assistant turns so far ([{role, content}]); the final answer is the last one on success. */
  readonly messages: Array<Record<string, string>>;
  /** When status == requires_action, what the run is waiting on: {kind, servers?, key?, message?}. */
  readonly requiresAction?: Record<string, unknown>;
  readonly traceId: string;
  readonly error: string;

  constructor(init: {
    id: string;
    status: string;
    messages?: Array<Record<string, string>>;
    requiresAction?: Record<string, unknown>;
    traceId?: string;
    error?: string;
  }) {
    this.id = init.id;
    this.status = init.status;
    this.messages = init.messages ?? [];
    this.requiresAction = init.requiresAction;
    this.traceId = init.traceId ?? "";
    this.error = init.error ?? "";
  }

  get isTerminal(): boolean {
    return TERMINAL.has(this.status);
  }

  static fromJson(obj: Record<string, unknown>): Run {
    const rawMessages = obj["messages"];
    const rawRequiresAction = obj["requiresAction"];
    return new Run({
      id: String(obj["id"] ?? ""),
      status: String(obj["status"] ?? ""),
      messages: Array.isArray(rawMessages) ? (rawMessages as Array<Record<string, string>>) : [],
      requiresAction:
        typeof rawRequiresAction === "object" && rawRequiresAction !== null
          ? (rawRequiresAction as Record<string, unknown>)
          : undefined,
      traceId: String(obj["traceId"] ?? ""),
      error: String(obj["error"] ?? ""),
    });
  }
}

/** One event off a run's SSE stream: a monotonic seq, a kind, and its data payload. */
export interface RunEvent {
  seq: number;
  kind: string;
  data: string;
}

/** Options passed when constructing a {@link RunsClient}. */
export interface RunsClientOptions {
  token?: string;
}

/** Options for {@link RunsClient.create} / {@link RunsClient.run}. */
export interface CreateRunOptions {
  agent: string;
  input: unknown;
  namespace?: string;
  conversationId?: string;
}

/** Options for {@link RunsClient.run} — create + block to terminal. */
export interface RunOptions extends CreateRunOptions {
  /** Poll interval in ms while waiting for a terminal/requires_action state (default 250). */
  pollIntervalMs?: number;
  /** Overall timeout in ms before giving up (default 120_000). */
  timeoutMs?: number;
}

/** A thin, dependency-free client over the BFF run API (ADR 0034). */
export class RunsClient {
  private readonly base: string;
  private readonly token?: string;

  constructor(baseUrl: string, opts: RunsClientOptions = {}) {
    if (!baseUrl) {
      throw new Error("RunsClient requires a baseUrl (the console/BFF origin)");
    }
    this.base = baseUrl.replace(/\/+$/, "");
    this.token = opts.token;
  }

  // -- lifecycle ---------------------------------------------------------------------------

  /** Create + start a run (POST /api/runs → 202). Returns the run id + initial status. */
  async create(opts: CreateRunOptions): Promise<Run> {
    const body: Record<string, unknown> = { agent: opts.agent, input: opts.input };
    if (opts.namespace) body["namespace"] = opts.namespace;
    if (opts.conversationId) body["conversationId"] = opts.conversationId;
    const data = await this.request("POST", "/api/runs", { body, expect: [200, 202] });
    return Run.fromJson(data);
  }

  /** Fetch a run's current state (GET /api/runs/{id}). */
  async get(runId: string): Promise<Run> {
    const data = await this.request("GET", `/api/runs/${encodeURIComponent(runId)}`, {
      expect: [200],
    });
    return Run.fromJson(data);
  }

  /**
   * Resume a run paused in requires_action (POST /api/runs/{id}/resume).
   *
   * `decision` is for human-in-the-loop approval (m32.4): `"approve"` (the default effect) or
   * `"deny"` (→ cancelled). The consent path needs no decision — the user connected their account,
   * so resume just re-invokes.
   */
  async resume(runId: string, opts: { decision?: string } = {}): Promise<Run> {
    const body = opts.decision ? { decision: opts.decision } : undefined;
    const data = await this.request("POST", `/api/runs/${encodeURIComponent(runId)}/resume`, {
      body,
      expect: [200, 202],
    });
    return Run.fromJson(data);
  }

  /** Cancel a non-terminal run (POST /api/runs/{id}/cancel → cancelled). */
  async cancel(runId: string): Promise<Run> {
    const data = await this.request("POST", `/api/runs/${encodeURIComponent(runId)}/cancel`, {
      expect: [200],
    });
    return Run.fromJson(data);
  }

  // -- streaming ---------------------------------------------------------------------------

  /**
   * Stream a run's events over SSE (GET /api/runs/{id}/events), resumable from a cursor.
   *
   * An async iterator yielding {@link RunEvent} (`state` / `message` / `token` / `step`) as they
   * arrive; the iterator ends when the stream closes (the run reached a terminal state). Pass
   * `fromSeq` (a Last-Event-ID cursor) to replay only events after a reconnect.
   */
  async *stream(runId: string, fromSeq = 0): AsyncGenerator<RunEvent, void, void> {
    const headers = this.headers();
    headers["Accept"] = "text/event-stream";
    if (fromSeq) headers["Last-Event-ID"] = String(fromSeq);

    const controller = new AbortController();
    // A long read timeout (FUNC-7): a run can idle between events; the timer is refreshed on every
    // chunk so a live-but-idle stream never trips it, while a genuinely dead connection still aborts.
    let timer = setTimeout(() => controller.abort(), STREAM_READ_TIMEOUT_MS);

    let resp: Response;
    try {
      resp = await fetch(this.url(`/api/runs/${encodeURIComponent(runId)}/events`), {
        method: "GET",
        headers,
        signal: controller.signal,
      });
    } catch (err) {
      clearTimeout(timer);
      throw new EndpointError(`run event stream request failed: ${(err as Error).message}`);
    }

    if (resp.status !== 200 || !resp.body) {
      clearTimeout(timer);
      const bodyText = await resp.text().catch(() => "");
      throw new EndpointError(
        `run event stream returned ${resp.status}: ${bodyText.slice(0, 200)}`,
        { status: resp.status, body: bodyText },
      );
    }

    const decoder = new TextDecoder();
    let buffer = "";
    let seq = 0;
    let kind = "";
    let data: string | null = null;

    const flush = (): RunEvent | null => {
      if (data === null) return null;
      const event: RunEvent = { seq, kind: kind || "message", data };
      seq = 0;
      kind = "";
      data = null;
      return event;
    };

    try {
      // WHATWG ReadableStream is async-iterable on Node 22.
      for await (const chunk of resp.body as unknown as AsyncIterable<Uint8Array>) {
        clearTimeout(timer);
        timer = setTimeout(() => controller.abort(), STREAM_READ_TIMEOUT_MS);
        buffer += decoder.decode(chunk, { stream: true });
        let idx: number;
        while ((idx = buffer.indexOf("\n")) >= 0) {
          let line = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 1);
          if (line.endsWith("\r")) line = line.slice(0, -1);

          if (line === "") {
            const event = flush();
            if (event) yield event;
            continue;
          }
          const colon = line.indexOf(":");
          const field = colon >= 0 ? line.slice(0, colon) : line;
          let value = colon >= 0 ? line.slice(colon + 1) : "";
          if (value.startsWith(" ")) value = value.slice(1);
          if (field === "id") {
            seq = intOr(value, 0);
          } else if (field === "event") {
            kind = value;
          } else if (field === "data") {
            data = data === null ? value : data + "\n" + value;
          }
        }
      }
      // A trailing event not terminated by a blank line.
      const trailing = flush();
      if (trailing) yield trailing;
    } catch (err) {
      throw new EndpointError(`run event stream read failed: ${(err as Error).message}`);
    } finally {
      clearTimeout(timer);
    }
  }

  // -- sync sugar --------------------------------------------------------------------------

  /**
   * Create a run and BLOCK until it reaches a terminal state (or requires_action), then return it —
   * the sugar over create + poll. A `requires_action` run is returned so the caller can resume it.
   * Rejects with {@link EndpointError} on timeout.
   */
  async run(opts: RunOptions): Promise<Run> {
    const pollIntervalMs = opts.pollIntervalMs ?? 250;
    const timeoutMs = opts.timeoutMs ?? 120_000;
    let run = await this.create(opts);
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      run = await this.get(run.id);
      if (run.isTerminal || run.status === "requires_action") {
        return run;
      }
      if (Date.now() >= deadline) {
        throw new EndpointError(`run ${run.id} did not finish within ${timeoutMs}ms`);
      }
      await sleep(pollIntervalMs);
    }
  }

  // -- internals ---------------------------------------------------------------------------

  private async request(
    method: string,
    path: string,
    opts: { body?: unknown; expect: number[] },
  ): Promise<Record<string, unknown>> {
    const headers = this.headers();
    const init: RequestInit = { method, headers };
    if (opts.body !== undefined) {
      init.body = JSON.stringify(opts.body);
    }
    let resp: Response;
    try {
      resp = await fetch(this.url(path), init);
    } catch (err) {
      throw new EndpointError(`run API request failed: ${(err as Error).message}`);
    }
    if (!opts.expect.includes(resp.status)) {
      const bodyText = await resp.text().catch(() => "");
      throw new EndpointError(
        `run API ${method} ${path} returned ${resp.status}: ${bodyText.slice(0, 200)}`,
        { status: resp.status, body: bodyText },
      );
    }
    const parsed: unknown = await resp.json().catch(() => ({}));
    return typeof parsed === "object" && parsed !== null
      ? (parsed as Record<string, unknown>)
      : {};
  }

  private url(path: string): string {
    return this.base + path;
  }

  private headers(): Record<string, string> {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    return headers;
  }
}

function intOr(value: string, fallback: number): number {
  const n = Number.parseInt(value, 10);
  return Number.isNaN(n) ? fallback : n;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    if (ms <= 0) {
      setImmediate(resolve);
    } else {
      setTimeout(resolve, ms);
    }
  });
}
