/**
 * The SDK against the launcher contract (sdk/launcher-routes.json).
 *
 * The fake is generated from the contract, whose goldens cmd/launcher proves against the real
 * handlers. It 404s anything the launcher does not serve and records every request that breaks a
 * route's schema, so a client calling the wrong path or sending the wrong field fails here.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  CAPABILITY_HEADER,
  ConfigError,
  EndpointError,
  PlaneConfig,
  agent,
  capabilityScope,
  makeRunContext,
  type Client,
} from "../src/index.js";
import {
  DelegateStub,
  DiscoveryStub,
  FeedbackStub,
  MemoryStub,
  MeshStub,
} from "../src/testing.js";
import { CONTRACT, FakeLauncher, isJsonContentType, validate, type FakeGolden } from "./generated/fake-launcher.js";

const CAPABILITY = "probe-run-capability";
const SYNTHETIC_TOOL_MODES = new Set(["delegate", "handoff", "knowledge"]);

let fake: FakeLauncher;
let client: Client;
const savedEnv: Record<string, string | undefined> = {};

function setEnv(k: string, v: string): void {
  if (!(k in savedEnv)) savedEnv[k] = process.env[k];
  process.env[k] = v;
}

beforeEach(async () => {
  fake = await FakeLauncher.start();
  // Skills gate on the injected refs the SDK reads from the environment.
  setEnv("SKILL_REFS", "deploy@sha256:641afbe10b79fadbeae77a9aaf1af023e6ccb4bc3d523a640f5717bc6ad5c15b");
  client = agent.fromConfig(
    PlaneConfig.forTest({
      memoryBaseUrl: fake.baseUrl,
      discoveryBaseUrl: fake.baseUrl,
      feedbackBaseUrl: fake.baseUrl,
      meshBaseUrl: fake.baseUrl,
      delegateBaseUrl: fake.baseUrl,
      toolsJsonPath: "/nonexistent/tools.json",
      run: makeRunContext({ agentName: "probe-agent", conversationId: "conv-1" }),
      memoryWired: true,
      longtermWired: true,
      feedbackWired: true,
      meshWired: true,
      knowledgeEnabled: true,
      delegateEnabled: true,
    }),
  );
});

afterEach(async () => {
  await fake.stop();
  for (const [k, v] of Object.entries(savedEnv)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
    delete savedEnv[k];
  }
});

function body(key: string): Record<string, unknown> {
  return fake.golden(key).request.body as Record<string, unknown>;
}

function answer(key: string): unknown {
  return fake.golden(key).response.body;
}

function withCapability<T>(fn: () => Promise<T>): Promise<T> {
  return capabilityScope({ [CAPABILITY_HEADER]: CAPABILITY }, fn);
}

/** Every call the SDK owes, keyed by the contract route it must reach. */
function calls(): Record<string, () => Promise<unknown>> {
  return {
    "GET /memory/{conversationId}": () => client.memory.get("conv-1"),
    "PUT /memory/{conversationId}": () =>
      client.memory.put(fake.golden("PUT /memory/{conversationId}").request.body as unknown[], "conv-2"),
    "POST /memory/{conversationId}/append": () =>
      client.memory.append(fake.golden("POST /memory/{conversationId}/append").request.body, "conv-3"),
    "GET /memory/{conversationId}/search": () => client.memory.search("metric", "conv-1"),
    "POST /memory/agent/remember": () => {
      const b = body("POST /memory/agent/remember");
      return client.memory.remember(b["content"] as string, b["tags"] as Record<string, string>);
    },
    "POST /memory/agent/search": () => client.memory.searchAgent("units", 3),
    "POST /knowledge/search": () => client.knowledge.search("When do deploys go out?", "handbook", 3),
    "GET /skills": () => client.skills.list(),
    "POST /skills/load": () => client.skills.load("deploy"),
    "ANY /feedback": () => {
      const b = body("ANY /feedback");
      return client.feedback.score(b["traceId"] as string, b["name"] as string, b["value"] as number, b["comment"] as string);
    },
    "POST /a2a/{targetAgent}": () => client.mesh.call("peer-agent", { question: "ping" }),
    "POST /delegate": () =>
      withCapability(() => client.tools.delegate("researcher", "Summarise the quarterly report", "1", "call-1")),
    "POST /handoff": () =>
      withCapability(() => client.tools.handoff("billing", "The customer is asking about an invoice.", true)),
    "GET /tools": () => client.tools.list(),
  };
}

describe("the SDK reaches every launcher route the contract declares", () => {
  it("covers each route, leaves waived ones alone, and breaks no route's schema", async () => {
    const failures: string[] = [];
    for (const [key, call] of Object.entries(calls())) {
      await call().catch((e: unknown) => failures.push(`${key}: ${String(e)}`));
    }
    expect(failures).toEqual([]);
    for (const key of fake.routeKeys()) {
      const why = fake.waived(key);
      if (why) {
        expect(fake.isCovered(key), `${key} is waived for TypeScript (${why}) but the SDK reaches it now: delete the waiver`).toBe(false);
      } else {
        expect(fake.isCovered(key), `the launcher serves ${key} and no SDK call reached it`).toBe(true);
      }
    }
    expect(fake.violations()).toEqual([]);
  });

  it("waives exactly /amp and /events, the routes it has no call for", () => {
    const waived = fake.routeKeys().filter((k) => fake.waived(k));
    expect(waived.sort()).toEqual(["GET /events", "POST /amp/{targetAgent}"]);
  });
});

describe("responses decode as the launcher sends them", () => {
  it("returns the golden bodies unchanged", async () => {
    expect(await client.memory.get("conv-1")).toEqual(answer("GET /memory/{conversationId}"));
    expect(await client.memory.search("metric", "conv-1")).toEqual(answer("GET /memory/{conversationId}/search"));
    expect(await client.memory.searchAgent("units", 3)).toEqual(
      (answer("POST /memory/agent/search") as { results: unknown }).results,
    );
    expect(await client.knowledge.search("When do deploys go out?", "handbook", 3)).toEqual(
      (answer("POST /knowledge/search") as { results: unknown }).results,
    );
    expect(await client.skills.list()).toEqual((answer("GET /skills") as { skills: unknown }).skills);
    expect(await client.skills.load("deploy")).toBe((answer("POST /skills/load") as { body: string }).body);
    expect(await client.mesh.call("peer-agent", { question: "ping" })).toEqual(answer("POST /a2a/{targetAgent}"));
    expect(
      await withCapability(() => client.tools.delegate("researcher", "Summarise the quarterly report", "1", "call-1")),
    ).toEqual(answer("POST /delegate"));
    expect(await withCapability(() => client.tools.handoff("billing", "over to you", true))).toEqual(
      answer("POST /handoff"),
    );
    const manifest = answer("GET /tools") as { tools: Array<Record<string, unknown>> };
    const tools = (await client.tools.list()).filter((t) => !SYNTHETIC_TOOL_MODES.has(t.mode));
    expect(tools.map((t) => ({ name: t.name, mode: t.mode, endpoint: t.endpoint, transport: t.transport, description: t.description }))).toEqual(
      manifest.tools.map((t) => ({ description: "", ...t })),
    );
    fake.assertConforms();
  });

  it("relays the run capability on delegate and handoff", async () => {
    await withCapability(() => client.tools.delegate("researcher", "x", "1", "call-1"));
    await withCapability(() => client.tools.handoff("billing", "", true));
    for (const r of fake.received) {
      expect(r.headers[CAPABILITY_HEADER.toLowerCase()], r.route).toBe(CAPABILITY);
    }
    fake.assertConforms();
  });
});

describe("refusals surface the launcher's answer", () => {
  it("raises the status for an HTTP refusal", async () => {
    fake.useGolden("POST /knowledge/search", "refuses-a-knowledge-base-not-granted");
    fake.useGolden("ANY /feedback", "refuses-a-score-without-a-trace");
    fake.useGolden("POST /a2a/{targetAgent}", "refuses-a-target-that-is-not-a-dns-label");
    fake.useGolden("GET /memory/{conversationId}", "refuses-an-id-with-a-separator");

    const status = async (p: Promise<unknown>) => ((await p.catch((e: unknown) => e)) as EndpointError).status;
    expect(await status(client.knowledge.search("salaries", "payroll", 3))).toBe(403);
    expect(await status(client.feedback.score("t", "helpfulness", 1))).toBe(400);
    expect(await status(client.mesh.call("peer-agent", {}))).toBe(400);
    expect(await status(client.memory.get("conv-1"))).toBe(400);
  });

  it("surfaces an unattached skill as an error naming the 403", async () => {
    fake.useGolden("POST /skills/load", "refuses-a-skill-not-attached");
    const err = await client.skills.load("payroll").catch((e: unknown) => e);
    // The skills client raises ConfigError with the status in its message, not EndpointError.
    expect(err).toBeInstanceOf(ConfigError);
    expect(String(err)).toContain("403");
  });

  it("returns a delegation or handoff refusal as ok=false, not a throw", async () => {
    fake.useGolden("POST /delegate", "refuses-without-a-run-capability");
    fake.useGolden("POST /handoff", "refuses-a-target-outside-the-roster");
    const d = await withCapability(() => client.tools.delegate("researcher", "x", "1", "call-1"));
    const h = await withCapability(() => client.tools.handoff("stranger"));
    expect(d["ok"]).toBe(false);
    expect(d["error"]).toBeTruthy();
    expect(h["ok"]).toBe(false);
    expect(h["error"]).toBeTruthy();
  });
});

describe("the generated fake refuses what the launcher refuses", () => {
  it("404s an unknown path, 405s a wrong method, and records a bad body", async () => {
    const unknown = await fetch(`${fake.baseUrl}/memory/agent/forget`, { method: "POST", body: "{}" });
    expect(unknown.status).toBe(404);
    const wrongMethod = await fetch(`${fake.baseUrl}/skills/load`);
    expect(wrongMethod.status).toBe(405);
    await fetch(`${fake.baseUrl}/memory/agent/remember`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: "a fact" }),
    });
    const v = fake.violations();
    expect(v.some((x) => x.includes("missing required property \"content\""))).toBe(true);
    expect(v.some((x) => x.includes("property \"text\" is not in the contract"))).toBe(true);
    expect(() => fake.respond("POST /skills/load", 418)).toThrow();
    expect(() => fake.respond("POST /skills/load", 200, { content: "wrong key" })).toThrow();
  });
});

// ── the public offline fakes (ctxmesh/testing) against the contract ─────────────
//
// The stubs in src/testing.ts are public API, written by hand, so they could drift from the
// launcher the same way the old test fakes did. Each one is driven with the success golden of every
// route it serves, and its answer is held to the contract.

/** What these tests need of a public stub. BaseStub's `this`-typed routes keep subclasses from widening to it. */
interface Stub {
  readonly baseUrl: string;
  start(): Promise<unknown>;
  stop(): Promise<void>;
}

async function sendGolden(base: string, key: string, g: FakeGolden): Promise<Response> {
  const route = CONTRACT.routes.find((r) => r.key === key)!;
  const method = g.request.method ?? route.servedMethod;
  const qs = g.request.query ? `?${new URLSearchParams(g.request.query).toString()}` : "";
  const reqBody =
    g.request.bodyText !== undefined
      ? g.request.bodyText
      : g.request.body !== undefined
        ? JSON.stringify(g.request.body)
        : undefined;
  return fetch(`${base}${g.request.path}${qs}`, {
    method,
    headers: { "Content-Type": "application/json", ...(g.request.headers ?? {}) },
    body: reqBody,
  });
}

async function conformance(stub: Stub, key: string): Promise<string[]> {
  const route = CONTRACT.routes.find((r) => r.key === key)!;
  const resp = await sendGolden(stub.baseUrl, key, route.goldens[0]!);
  const text = await resp.text();
  const decl = route.responses[String(resp.status)];
  if (!decl) return [`${key}: answered ${resp.status}, which the launcher never sends`];
  const out: string[] = [];
  if (!text) {
    if (decl.contentType && route.goldens[0]!.response.body !== undefined) out.push(`${key}: answered with no body`);
    return out;
  }
  if (!decl.contentType) return [`${key}: answered ${resp.status} with a body the launcher never sends`];
  const ct = resp.headers.get("content-type") ?? "";
  if (isJsonContentType(decl.contentType)) {
    if (!isJsonContentType(ct)) out.push(`${key}: Content-Type is ${JSON.stringify(ct)}, the launcher sends ${decl.contentType}`);
    let v: unknown;
    try {
      v = JSON.parse(text);
    } catch {
      return [...out, `${key}: the body is not JSON`];
    }
    if (decl.schema) out.push(...validate(decl.schema, v, `${key} body`));
  }
  return out;
}

describe("the public offline fakes answer within the contract", () => {
  const conforming: Array<[string, () => Stub, string[]]> = [
    ["MemoryStub", () => new MemoryStub(), [
      "GET /memory/{conversationId}",
      "PUT /memory/{conversationId}",
      "POST /memory/{conversationId}/append",
      "GET /memory/{conversationId}/search",
    ]],
    ["FeedbackStub", () => new FeedbackStub(), ["ANY /feedback"]],
    ["DelegateStub", () => new DelegateStub(), ["POST /delegate", "POST /handoff"]],
    ["DiscoveryStub", () => new DiscoveryStub(), ["GET /tools"]],
  ];
  for (const [name, make, keys] of conforming) {
    it(`${name} conforms on ${keys.join(", ")}`, async () => {
      const stub = make();
      await stub.start();
      try {
        const problems: string[] = [];
        for (const key of keys) problems.push(...(await conformance(stub, key)));
        expect(problems).toEqual([]);
      } finally {
        await stub.stop();
      }
    });
  }

  // Known gap, kept visible: MeshStub answers JSON with no Content-Type, where the launcher relays
  // the peer's (application/json for every golden). it.fails turns red the day the stub is fixed.
  it.fails("MeshStub conforms on the AMP routes (it sends JSON without a Content-Type)", async () => {
    const stub = await new MeshStub().start();
    try {
      const problems = [
        ...(await conformance(stub, "POST /a2a/{targetAgent}")),
        ...(await conformance(stub, "POST /amp/{targetAgent}")),
      ];
      expect(problems).toEqual([]);
    } finally {
      await stub.stop();
    }
  });
});
