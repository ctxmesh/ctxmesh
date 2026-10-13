// The TypeScript SDK's conformance probe: drives the BUILT SDK's public API against a real
// launcher, one call per contract route, and prints "PASS|FAIL|MISSING <route>[: reason]" for each.
// hack/sdk-conformance runs it and checks every PASS against what the launcher actually received.
import { readFileSync } from "node:fs";

const contract = JSON.parse(readFileSync(process.env.CTXMESH_PROBE_CONTRACT, "utf8"));
const capability = process.env.CTXMESH_PROBE_CAPABILITY ?? "";
const SYNTHETIC_TOOL_MODES = new Set(["delegate", "handoff", "knowledge"]);

function line(text) {
  process.stdout.write(text.replace(/\s*\n\s*/g, " ") + "\n");
}

let sdk;
let client;
let setupError;
try {
  sdk = await import(new URL("../../../../sdk/typescript/dist/esm/index.js", import.meta.url).href);
  // From the env the launcher publishes, not explicit URLs: that env is part of what is checked.
  // TOOLS_JSON_PATH is pointed away so GET /tools cannot pass on the cold-start file fallback.
  client = sdk.agent.fromEnv({ ...process.env, TOOLS_JSON_PATH: "/nonexistent/ctxmesh-probe/tools.json" });
} catch (err) {
  setupError = err;
}

function withCapability(fn) {
  return sdk.capabilityScope({ [sdk.CAPABILITY_HEADER]: capability }, fn);
}

function segment(path, i) {
  return path.replace(/^\/+|\/+$/g, "").split("/")[i] ?? "";
}

function isZero(v) {
  return (
    v === null || v === undefined || v === "" || v === false || v === 0 ||
    (typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0)
  );
}

// within: every value the SDK decoded must appear, equal, at the same place in what the launcher
// sent; arrays must have the same length. A field the SDK does not model is not its concern.
function within(got, want, at) {
  if (Array.isArray(got)) {
    if (!Array.isArray(want) || want.length !== got.length) return at;
    for (let i = 0; i < got.length; i++) {
      const p = within(got[i], want[i], `${at}[${i}]`);
      if (p) return p;
    }
    return "";
  }
  if (got !== null && typeof got === "object") {
    if (want === null || typeof want !== "object" || Array.isArray(want)) return at;
    for (const [k, v] of Object.entries(got)) {
      if (isZero(v) && !(k in want)) continue;
      const p = within(v, want[k], `${at}.${k}`);
      if (p) return p;
    }
    return "";
  }
  return got === want ? "" : at;
}

// The route being probed: its golden response body and its sdkResult paths.
const current = { body: undefined, required: [] };

function same(got, want) {
  return !within(got, want, "$") && !within(want, got, "$");
}

// carry: got holds want's value at a sdkResult path ("[]" descends into each element).
function carry(got, want, segs, at) {
  if (segs.length === 0) return same(got, want) ? "" : at;
  const each = segs[0].endsWith("[]");
  const key = each ? segs[0].slice(0, -2) : segs[0];
  if (key) {
    if (got === null || typeof got !== "object" || !(key in got)) return `${at}.${key}`;
    [got, want, at] = [got[key], (want ?? {})[key], `${at}.${key}`];
  }
  if (!each) return carry(got, want, segs.slice(1), at);
  if (!Array.isArray(got) || !Array.isArray(want) || got.length !== want.length) return at;
  for (let i = 0; i < want.length; i++) {
    const p = carry(got[i], want[i], segs.slice(1), `${at}[${i}]`);
    if (p) return p;
  }
  return "";
}

function expectJSON(what, got, want) {
  const normalised = JSON.parse(JSON.stringify(got ?? null));
  const at = within(normalised, want, "$");
  if (at) {
    throw new Error(
      `${what} decoded as ${JSON.stringify(normalised)}, but the launcher sent ${JSON.stringify(want)} (differs at ${at})`,
    );
  }
  // within compares only what the SDK kept, so it passes an SDK that dropped a field; the route's
  // sdkResult names the fields every SDK must carry.
  if (want !== current.body) return;
  for (const path of current.required) {
    const missed = path === "$" ? (same(normalised, want) ? "" : "$") : carry(normalised, want, path.split("."), "$");
    if (missed) {
      throw new Error(
        `${what} decoded as ${JSON.stringify(normalised)}, which does not carry ${path} with the launcher's value ` +
          `(at ${missed}); the launcher sent ${JSON.stringify(want)}`,
      );
    }
  }
}

const calls = {
  "POST /a2a/{targetAgent}": async (g) => {
    expectJSON("reply", await client.mesh.call(segment(g.request.path, 1), g.request.body), g.response.body);
  },
  "POST /delegate": async (g) => {
    const b = g.request.body;
    const d = await withCapability(() => client.tools.delegate(b.subAgent, b.input, b.step, b.callId));
    expectJSON("delegation", d, g.response.body);
  },
  "ANY /feedback": async (g) => {
    const b = g.request.body;
    await client.feedback.score(b.traceId, b.name, b.value, b.comment);
  },
  "POST /handoff": async (g) => {
    const b = g.request.body;
    const h = await withCapability(() => client.tools.handoff(b.targetAgent, b.message, b.includeHistory !== false));
    expectJSON("handoff", h, g.response.body);
  },
  "POST /knowledge/search": async (g) => {
    const b = g.request.body;
    const results = await client.knowledge.search(b.query, b.knowledgeBase, b.topK);
    expectJSON("chunks", { results }, g.response.body);
  },
  "POST /memory/agent/remember": async (g) => {
    await client.memory.remember(g.request.body.content, g.request.body.tags);
  },
  "POST /memory/agent/search": async (g) => {
    const results = await client.memory.searchAgent(g.request.body.query, g.request.body.topK);
    expectJSON("facts", { results }, g.response.body);
  },
  "GET /memory/{conversationId}": async (g) => {
    expectJSON("entries", await client.memory.get(segment(g.request.path, 1)), g.response.body);
  },
  "PUT /memory/{conversationId}": async (g) => {
    await client.memory.put(g.request.body, segment(g.request.path, 1));
  },
  "POST /memory/{conversationId}/append": async (g) => {
    await client.memory.append(g.request.body, segment(g.request.path, 1));
  },
  "GET /memory/{conversationId}/search": async (g) => {
    const entries = await client.memory.search(g.request.query?.q ?? "", segment(g.request.path, 1));
    expectJSON("entries", entries, g.response.body);
  },
  "GET /skills": async (g) => {
    expectJSON("skills", { skills: await client.skills.list() }, g.response.body);
  },
  "POST /skills/load": async (g) => {
    expectJSON("skill", { body: await client.skills.load(g.request.body.name) }, g.response.body);
  },
  "GET /tools": async (g) => {
    // The SDK adds synthetic tools (delegate_to, handoff_to, knowledge_search) the launcher never
    // serves; the manifest is what came from the route.
    const tools = (await client.tools.list())
      .filter((t) => !SYNTHETIC_TOOL_MODES.has(t.mode))
      .map((t) => ({
        name: t.name,
        mode: t.mode,
        endpoint: t.endpoint,
        transport: t.transport,
        description: t.description,
        inputSchema: t.inputSchema,
      }));
    expectJSON("manifest", { tools }, g.response.body);
  },
};

for (const route of contract.routes) {
  const key = `${route.method} ${route.path}`;
  if (setupError) {
    line(`FAIL ${key}: the SDK does not load or configure from the launcher's env: ${setupError}`);
    continue;
  }
  const call = calls[key];
  if (!call) {
    line(`MISSING ${key}: the TypeScript SDK has no public call for it`);
    continue;
  }
  try {
    current.body = route.goldens[0].response.body;
    current.required = route.sdkResult ?? [];
    await call(route.goldens[0]);
    line(`PASS ${key}`);
  } catch (err) {
    line(`FAIL ${key}: ${err?.message ?? err}`);
  }
}
