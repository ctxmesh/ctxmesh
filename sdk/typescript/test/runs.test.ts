/**
 * RunsClient contract tests (ADR 0034, m32.9) — parity with `sdk/python/tests/test_runs.py`.
 *
 * Drive `ctxmesh.RunsClient` against a tiny in-process BFF stub (a `node:http` server on an
 * ephemeral port, mirroring test/plane.ts), asserting it speaks the BFF run API: create → 202,
 * get, resume with a decision, cancel, SSE stream parsing, and the run() sugar's create+poll.
 */

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { EndpointError } from "../src/errors.js";
import { Run, RunEvent, RunsClient } from "../src/runs.js";

/** A recorded inbound request the stub captured, for assertions. */
interface Recorded {
  method: string;
  path: string;
  headers: Record<string, string>;
  body: string;
}

/** A canned BFF response: a JSON body at a status, or a raw SSE stream. */
type Reply =
  | { json: unknown; status?: number }
  | { sse: string[]; status?: number };

/**
 * A tiny BFF run-API stub. Each request pops the next queued reply (FIFO), records the request,
 * and either returns JSON or streams the given SSE lines.
 */
class BffStub {
  readonly requests: Recorded[] = [];
  private readonly replies: Reply[] = [];
  private server?: Server;
  private port = 0;

  queue(reply: Reply): this {
    this.replies.push(reply);
    return this;
  }

  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  /** The i-th recorded request (asserts it exists — keeps the tests free of `?`/`!` noise). */
  req(i: number): Recorded {
    const r = this.requests[i];
    if (!r) throw new Error(`no request recorded at index ${i}`);
    return r;
  }

  async start(): Promise<this> {
    this.server = createServer((req, res) => this.dispatch(req, res));
    await new Promise<void>((resolve) => {
      this.server!.listen(0, "127.0.0.1", () => {
        this.port = (this.server!.address() as AddressInfo).port;
        resolve();
      });
    });
    return this;
  }

  async stop(): Promise<void> {
    if (!this.server) return;
    await new Promise<void>((resolve, reject) => {
      this.server!.close((err) => (err ? reject(err) : resolve()));
    });
    this.server = undefined;
  }

  private dispatch(req: IncomingMessage, res: ServerResponse): void {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const headers: Record<string, string> = {};
      for (const [k, v] of Object.entries(req.headers)) {
        if (typeof v === "string") headers[k.toLowerCase()] = v;
      }
      this.requests.push({
        method: req.method ?? "GET",
        path: (req.url ?? "/").split("?")[0]!,
        headers,
        body: Buffer.concat(chunks).toString("utf8"),
      });

      const reply = this.replies.shift();
      if (!reply) {
        res.writeHead(500);
        res.end('{"error":"no queued reply"}');
        return;
      }
      if ("sse" in reply) {
        res.writeHead(reply.status ?? 200, { "Content-Type": "text/event-stream" });
        res.end(reply.sse.join("\n") + "\n");
        return;
      }
      const payload = JSON.stringify(reply.json);
      res.writeHead(reply.status ?? 200, { "Content-Type": "application/json" });
      res.end(payload);
    });
  }
}

let bff: BffStub;

beforeEach(async () => {
  bff = await new BffStub().start();
});

afterEach(async () => {
  await bff.stop();
});

describe("RunsClient.create", () => {
  it("POSTs a run to /api/runs with the bearer token and parses the id/status", async () => {
    bff.queue({ status: 202, json: { id: "run-1", status: "queued" } });
    const runs = new RunsClient(bff.baseUrl + "/", { token: "tok-abc" });

    const run = await runs.create({
      agent: "researcher",
      input: { input: "hi" },
      conversationId: "c1",
    });

    expect(run.id).toBe("run-1");
    expect(run.status).toBe("queued");
    const rec = bff.req(0);
    expect([rec.method, rec.path]).toEqual(["POST", "/api/runs"]);
    expect(rec.headers["authorization"]).toBe("Bearer tok-abc");
    expect(JSON.parse(rec.body)).toEqual({
      agent: "researcher",
      input: { input: "hi" },
      conversationId: "c1",
    });
  });
});

describe("RunsClient.get", () => {
  it("parses messages + requiresAction and reports non-terminal", async () => {
    bff.queue({
      json: {
        id: "run-1",
        status: "requires_action",
        traceId: "tr-1",
        messages: [{ role: "assistant", content: "awaiting" }],
        requiresAction: { kind: "approval", key: "send-email", message: "Send it?" },
      },
    });

    const run = await new RunsClient(bff.baseUrl).get("run-1");
    expect(run.status).toBe("requires_action");
    expect(run.traceId).toBe("tr-1");
    expect(run.requiresAction?.kind).toBe("approval");
    expect(run.requiresAction?.key).toBe("send-email");
    expect(run.messages[0]!.content).toBe("awaiting");
    expect(run.isTerminal).toBe(false);
    expect(bff.req(0).path).toBe("/api/runs/run-1");
  });
});

describe("RunsClient.resume", () => {
  it("sends the decision body", async () => {
    bff.queue({ json: { id: "run-1", status: "cancelled" } });
    const run = await new RunsClient(bff.baseUrl).resume("run-1", { decision: "deny" });

    expect(run.status).toBe("cancelled");
    const rec = bff.req(0);
    expect([rec.method, rec.path]).toEqual(["POST", "/api/runs/run-1/resume"]);
    expect(JSON.parse(rec.body)).toEqual({ decision: "deny" });
  });

  it("omits the body when no decision is given (the consent path)", async () => {
    bff.queue({ status: 202, json: { id: "run-1", status: "running" } });
    await new RunsClient(bff.baseUrl).resume("run-1");
    expect(bff.req(0).body).toBe("");
  });
});

describe("RunsClient.cancel", () => {
  it("POSTs to the cancel endpoint", async () => {
    bff.queue({ json: { id: "run-1", status: "cancelled" } });
    const run = await new RunsClient(bff.baseUrl).cancel("run-1");
    expect(run.status).toBe("cancelled");
    expect([bff.req(0).method, bff.req(0).path]).toEqual([
      "POST",
      "/api/runs/run-1/cancel",
    ]);
  });
});

describe("RunsClient.stream", () => {
  it("parses SSE frames into RunEvents", async () => {
    // A realistic BFF SSE stream: two token frames, a message, then the terminal state.
    bff.queue({
      sse: [
        "id:1", "event:token", "data:Hel", "",
        "id:2", "event:token", "data:lo", "",
        "id:3", "event:message", "data:Hello", "",
        "id:4", "event:state", "data:succeeded", "",
      ],
    });

    const events: RunEvent[] = [];
    for await (const event of new RunsClient(bff.baseUrl).stream("run-1")) {
      events.push(event);
    }

    expect(events).toEqual([
      { seq: 1, kind: "token", data: "Hel" },
      { seq: 2, kind: "token", data: "lo" },
      { seq: 3, kind: "message", data: "Hello" },
      { seq: 4, kind: "state", data: "succeeded" },
    ]);
    // Accept header + no Last-Event-ID when fromSeq defaults to 0.
    expect(bff.req(0).headers["accept"]).toBe("text/event-stream");
    expect(bff.req(0).headers["last-event-id"]).toBeUndefined();
  });

  it("sends the Last-Event-ID cursor when resuming from a seq", async () => {
    bff.queue({ sse: ["id:5", "event:state", "data:succeeded", ""] });
    const events: RunEvent[] = [];
    for await (const event of new RunsClient(bff.baseUrl).stream("run-1", 4)) {
      events.push(event);
    }
    expect(events).toEqual([{ seq: 5, kind: "state", data: "succeeded" }]);
    expect(bff.req(0).headers["last-event-id"]).toBe("4");
  });

  it("throws EndpointError on a non-200 stream response", async () => {
    bff.queue({ status: 404, json: { error: "no such run" } });
    const iter = new RunsClient(bff.baseUrl).stream("missing");
    await expect(iter.next()).rejects.toBeInstanceOf(EndpointError);
  });
});

describe("RunsClient.run (create + poll sugar)", () => {
  it("creates then polls to a terminal state", async () => {
    bff.queue({ status: 202, json: { id: "run-1", status: "queued" } })
      .queue({ json: { id: "run-1", status: "running" } })
      .queue({
        json: {
          id: "run-1",
          status: "succeeded",
          messages: [{ role: "assistant", content: "done" }],
        },
      });

    const final = await new RunsClient(bff.baseUrl).run({
      agent: "a",
      input: { input: "x" },
      pollIntervalMs: 0,
    });
    expect(final.status).toBe("succeeded");
    expect(final.messages[final.messages.length - 1]!.content).toBe("done");
    // 1 create + 2 gets.
    expect(bff.requests.map((r) => r.method)).toEqual(["POST", "GET", "GET"]);
  });

  it("returns as soon as the run needs an action", async () => {
    bff.queue({ status: 202, json: { id: "run-1", status: "queued" } }).queue({
      json: {
        id: "run-1",
        status: "requires_action",
        requiresAction: { kind: "consent_required", servers: ["gh"] },
      },
    });

    const run = await new RunsClient(bff.baseUrl).run({
      agent: "a",
      input: {},
      pollIntervalMs: 0,
    });
    expect(run.status).toBe("requires_action");
    expect(run.requiresAction?.servers).toEqual(["gh"]);
  });

  it("rejects with EndpointError once the timeout elapses", async () => {
    bff.queue({ status: 202, json: { id: "run-1", status: "queued" } }).queue({
      json: { id: "run-1", status: "running" },
    });
    await expect(
      new RunsClient(bff.baseUrl).run({
        agent: "a",
        input: {},
        pollIntervalMs: 0,
        timeoutMs: 0,
      }),
    ).rejects.toBeInstanceOf(EndpointError);
  });
});

describe("RunsClient — construction + non-2xx", () => {
  it("requires a baseUrl", () => {
    expect(() => new RunsClient("")).toThrow();
  });

  it("surfaces a non-2xx run-API response as EndpointError carrying the status", async () => {
    bff.queue({ status: 500, json: { error: "boom" } });
    const runs = new RunsClient(bff.baseUrl);
    await expect(runs.get("run-1")).rejects.toMatchObject({ status: 500 });
  });

  it("exposes isTerminal on Run", () => {
    expect(new Run({ id: "r", status: "succeeded" }).isTerminal).toBe(true);
    expect(new Run({ id: "r", status: "running" }).isTerminal).toBe(false);
  });
});
