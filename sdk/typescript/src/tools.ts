/**
 * Tools client — the discovery sidecar (:2999) + MCP tool invocation (M4).
 * Parity with `sdk/python/src/ctxmesh/tools.py`.
 *
 * `list()` fetches the live manifest from the discovery sidecar
 * (`GET localhost:2999/tools`), falling back to the mounted `tools.json`
 * (cold-start backing) when the sidecar is unreachable — the same precedence the
 * raw agent uses (mcp-tools.md "Agent consumption").
 *
 * `call(name, args, opts?)` looks the tool up in the live manifest, then invokes it
 * over its MCP `streamable-http` endpoint. We speak the MCP wire directly over
 * `fetch`/`node:http` rather than pulling the heavyweight `mcp` package in as a
 * runtime dep — the SDK stays lean.
 *
 * **Catalog name vs MCP tool name.** The discovery manifest name is the
 * ToolRegistry catalog key (e.g. `word-count`, hyphen), which is NOT necessarily the
 * name the MCP server exposes the tool under (e.g. `word_count`, underscore — a
 * FastMCP function name). `call` must discover the real MCP name from the server:
 * it does the handshake, runs `tools/list`, resolves the catalog name to a server
 * tool name (exact → hyphen/underscore-normalized → sole-tool fallback), and only
 * then calls with the resolved name. (Carrying the MCP name in the manifest is a
 * phase-2 M4 item; the SDK resolves it at call time for now.)
 */

import * as fs from "node:fs";
import { APPROVAL_HEADER, currentApprovalVoucher } from "./_approval.js";
import { CAPABILITY_HEADER, currentCapability } from "./_capability.js";
import { RECORD_HEADER, currentRecordRunId } from "./_record.js";
import { PlaneConfig } from "./config.js";
import {
  ApprovalRequiredError,
  ConfigError,
  ConsentRequiredError,
  EndpointError,
} from "./errors.js";
import type { KnowledgeClient } from "./knowledge.js";

// ── synthetic tool names ─────────────────────────────────────────────────────

/** The synthetic sub-agent-delegation tool (M64, ADR 0057). */
export const DELEGATE_TOOL_NAME = "delegate_to";

/** The synthetic knowledge-retrieval tool (M68, ADR 0061 Fork 3). */
export const KNOWLEDGE_SEARCH_TOOL_NAME = "knowledge_search";

/** The synthetic HANDOFF (transfer-of-control) tool (M67, ADR 0060 §5). */
export const HANDOFF_TOOL_NAME = "handoff_to";

// ── constants ────────────────────────────────────────────────────────────────

/** MCP protocol version the SDK negotiates. */
const MCP_PROTOCOL_VERSION = "2025-03-26";

/** Manifest fetch timeout — cheap localhost GET but generous for a waking pod. */
const MANIFEST_TIMEOUT_MS = 2_000;

/** Tool-call timeout — a remote MCP round-trip may be slower than a localhost op. */
const TOOL_CALL_TIMEOUT_MS = 30_000;

/** Delegate sub-run timeout (synchronous; sub-agent may do multiple round-trips). */
const DELEGATE_TIMEOUT_MS = 600_000;

/** Handoff is not awaited — it just relays to the BFF. */
const HANDOFF_TIMEOUT_MS = 30_000;

/**
 * The spawn-tree position headers (mirrors internal/bff/invoke.go). Relayed on a /delegate call so
 * the launcher's depth gate (L7 suspension is depth-0 only) + spawn guard key on the AUTHORITATIVE
 * root rather than defaulting to depth 0 / root "" for every SDK-driven delegation.
 */
export const SPAWN_ROOT_HEADER = "X-Ctxmesh-Spawn-Root";
export const SPAWN_DEPTH_HEADER = "X-Ctxmesh-Spawn-Depth";

/** The manifest returned when a managed agent has NO tools bound. */
const EMPTY_MANIFEST: Record<string, unknown> = { tools: [] };

// ── Tool model ───────────────────────────────────────────────────────────────

/**
 * One entry of the discovery manifest (mcp-tools.md manifest shape).
 *
 * `inputSchema` is the tool's argument JSON Schema as the manifest carries it.
 * It is `null` when the manifest omits it (a curated/legacy entry with no captured
 * schema); the loop then falls back to a permissive object-parameters schema.
 *
 * `description` is the tool's human-readable description as the manifest carries it.
 * `""` when the manifest omits it; the loop then synthesises a generic description.
 */
export class Tool {
  readonly name: string;
  readonly mode: string;
  readonly endpoint: string;
  readonly transport: string;
  readonly inputSchema: Record<string, unknown> | null;
  readonly description: string;

  constructor(opts: {
    name: string;
    mode: string;
    endpoint: string;
    transport: string;
    inputSchema?: Record<string, unknown> | null;
    description?: string;
  }) {
    this.name = opts.name;
    this.mode = opts.mode;
    this.endpoint = opts.endpoint;
    this.transport = opts.transport;
    this.inputSchema = opts.inputSchema ?? null;
    this.description = opts.description ?? "";
  }

  /**
   * Construct a Tool from a manifest dict entry.
   *
   * The manifest carries `inputSchema` verbatim as a JSON object. Anything that is
   * not a JSON object (absent, null, or a malformed non-object) is treated as "no
   * schema" so the loop takes the permissive fallback rather than handing the model
   * a schema it can't use.
   */
  static fromDict(d: Record<string, unknown>): Tool {
    const rawSchema = d["inputSchema"];
    const inputSchema =
      typeof rawSchema === "object" && rawSchema !== null && !Array.isArray(rawSchema)
        ? (rawSchema as Record<string, unknown>)
        : null;
    const rawDesc = d["description"];
    const description = typeof rawDesc === "string" ? rawDesc : "";
    return new Tool({
      name: typeof d["name"] === "string" ? d["name"] : "",
      mode: typeof d["mode"] === "string" ? d["mode"] : "",
      endpoint: typeof d["endpoint"] === "string" ? d["endpoint"] : "",
      transport: typeof d["transport"] === "string" ? d["transport"] : "",
      inputSchema,
      description,
    });
  }
}

// ── synthetic tool builders ──────────────────────────────────────────────────

function delegateEnabled(): boolean {
  return (process.env["DELEGATE_ENABLED"] ?? "").trim().toLowerCase() === "true";
}

function delegateRoster(): Array<{ name: string; description: string }> {
  const raw = (process.env["DELEGATE_ROSTER"] ?? "").trim();
  if (!raw) return [];
  let data: unknown;
  try {
    data = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(data)) return [];
  const out: Array<{ name: string; description: string }> = [];
  for (const entry of data) {
    if (
      typeof entry === "object" &&
      entry !== null &&
      "name" in entry &&
      (entry as { name?: unknown }).name
    ) {
      const e = entry as Record<string, unknown>;
      out.push({
        name: String(e["name"]),
        description: String(e["description"] ?? ""),
      });
    }
  }
  return out;
}

function buildDelegateTool(): Tool {
  const roster = delegateRoster();
  const names = roster.map((r) => r.name);
  const listing =
    roster.map((r) => `- ${r.name}: ${r.description}`).join("\n") ||
    "(configured on the AgentTeam)";
  const description =
    "Delegate a subtask to a sub-agent on your team and wait for its result. Use it to break a " +
    "complex task into pieces handled by the right specialist, then combine their answers. " +
    `Available sub-agents:\n${listing}`;

  const subAgentSchema: Record<string, unknown> = {
    type: "string",
    description: "The roster member to delegate to.",
  };
  if (names.length > 0) subAgentSchema["enum"] = names;

  const schema: Record<string, unknown> = {
    type: "object",
    properties: {
      sub_agent: subAgentSchema,
      task: {
        type: "string",
        description: "The subtask to hand to the sub-agent, in plain language.",
      },
    },
    required: ["sub_agent", "task"],
  };

  return new Tool({
    name: DELEGATE_TOOL_NAME,
    mode: "delegate",
    endpoint: "http://127.0.0.1:2994/delegate",
    transport: "http",
    inputSchema: schema,
    description,
  });
}

function buildHandoffTool(): Tool {
  const roster = delegateRoster();
  const names = roster.map((r) => r.name);
  const listing =
    roster.map((r) => `- ${r.name}: ${r.description}`).join("\n") ||
    "(configured on the AgentTeam)";
  const description =
    "Hand off the ENTIRE conversation to another agent on your team and END your turn. Use it " +
    "when another specialist should take over the conversation with the user from here — this " +
    "is a TRANSFER, not a delegation: you do NOT get a result back and you do NOT continue " +
    "after calling it. The target agent continues talking with the user directly. " +
    `Available agents:\n${listing}`;

  const targetSchema: Record<string, unknown> = {
    type: "string",
    description: "The roster member to hand the conversation off to.",
  };
  if (names.length > 0) targetSchema["enum"] = names;

  const schema: Record<string, unknown> = {
    type: "object",
    properties: {
      target_agent: targetSchema,
      message: {
        type: "string",
        description:
          "An optional handoff note for the receiving agent (why you are transferring, " +
          "what is needed next). By default the full conversation history transfers " +
          "automatically; set include_history=false to hand off with THIS message as a " +
          "SUMMARY instead (the receiving agent then starts from your summary rather than " +
          "replaying the whole thread — use it for long conversations).",
      },
      include_history: {
        type: "boolean",
        description:
          "Whether the receiving agent replays the full conversation history on the transfer " +
          "turn (default true). Set false to hand off with `message` as a summary and skip the " +
          "full-history replay — cheaper for a long conversation.",
      },
    },
    required: ["target_agent"],
  };

  return new Tool({
    name: HANDOFF_TOOL_NAME,
    mode: "handoff",
    endpoint: "http://127.0.0.1:2994/handoff",
    transport: "http",
    inputSchema: schema,
    description,
  });
}

function knowledgeEnabled(): boolean {
  return (process.env["KNOWLEDGE_BASE_ENABLED"] ?? "").trim().toLowerCase() === "true";
}

function knowledgeRosterNames(): string[] {
  const raw = (process.env["KNOWLEDGE_BASES"] ?? "").trim();
  if (!raw) return [];
  let data: unknown;
  try {
    data = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(data)) return [];
  const out: string[] = [];
  for (const entry of data) {
    if (
      typeof entry === "object" &&
      entry !== null &&
      "name" in entry &&
      (entry as { name?: unknown }).name
    ) {
      out.push(String((entry as Record<string, unknown>)["name"]));
    }
  }
  return out;
}

function buildKnowledgeSearchTool(): Tool {
  const names = knowledgeRosterNames();
  let kbDescription: string;
  if (names.length > 0) {
    const corporaListing = names.map((n) => `- ${n}`).join("\n");
    kbDescription =
      `The knowledge base to search. Available corpora:\n${corporaListing}\n` +
      "Omit to use the default when only one corpus is available.";
  } else {
    kbDescription = "The knowledge base to search (configured on the AgentDeployment).";
  }
  let description =
    "Search a knowledge base for relevant passages and return them with source provenance " +
    "(documentRef, chunkIndex) for citation. Use this to retrieve grounding context mid-loop " +
    "before answering questions that require factual recall from a corpus. ALWAYS cite the " +
    "documentRef when you use retrieved content in your answer. ";
  if (names.length > 0) {
    description += `Available corpora: ${names.join(", ")}.`;
  }

  const schema: Record<string, unknown> = {
    type: "object",
    properties: {
      query: {
        type: "string",
        description: "The search query — a natural-language question or phrase.",
      },
      knowledge_base: {
        type: "string",
        description: kbDescription,
      },
      top_k: {
        type: "integer",
        description: "Maximum number of results to return (default 10).",
        default: 10,
      },
    },
    required: ["query"],
  };

  return new Tool({
    name: KNOWLEDGE_SEARCH_TOOL_NAME,
    mode: "knowledge",
    endpoint: "", // dispatched locally via KnowledgeClient, not over MCP
    transport: "internal",
    inputSchema: schema,
    description,
  });
}

// ── ToolsClient ───────────────────────────────────────────────────────────────

/**
 * Tool discovery + invocation against the localhost plane.
 * Exposed as `client.tools` on the SDK Client.
 */
export class ToolsClient {
  private readonly config: PlaneConfig;
  private readonly _knowledge?: KnowledgeClient;

  constructor(config: PlaneConfig, knowledge?: KnowledgeClient) {
    this.config = config;
    this._knowledge = knowledge;
  }

  // ── discovery ──────────────────────────────────────────────────────────────

  /**
   * Return the live tool manifest (sidecar first, tools.json fallback).
   *
   * A team supervisor/member with a roster also gets the synthetic `delegate_to`
   * (M64) + `handoff_to` (M67) tools alongside its MCP tools. An agent with
   * knowledge bases bound (M68) also gets `knowledge_search` when
   * KNOWLEDGE_BASE_ENABLED=true and the KNOWLEDGE_BASES roster is non-empty.
   */
  async list(): Promise<Tool[]> {
    const manifest = await this._fetchManifest();
    const rawTools = manifest["tools"];
    const tools: Tool[] = Array.isArray(rawTools)
      ? rawTools.map((t) => Tool.fromDict(t as Record<string, unknown>))
      : [];

    if (delegateEnabled()) {
      tools.push(buildDelegateTool());
      tools.push(buildHandoffTool());
    }
    if (knowledgeEnabled() && knowledgeRosterNames().length > 0) {
      tools.push(buildKnowledgeSearchTool());
    }
    return tools;
  }

  private async _fetchManifest(): Promise<Record<string, unknown>> {
    const url = `${this.config.discoveryBaseUrl}/tools`;

    // Try the live discovery sidecar first.
    try {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), MANIFEST_TIMEOUT_MS);
      let resp: Response;
      try {
        resp = await fetch(url, { signal: controller.signal });
      } finally {
        clearTimeout(timer);
      }
      if (resp.ok) {
        const data: unknown = await resp.json();
        if (typeof data === "object" && data !== null) {
          return data as Record<string, unknown>;
        }
      }
    } catch {
      // Sidecar unreachable — fall through to the durable backing file.
    }

    // Durable backing: the ConfigMap mount (cold-start only).
    const toolsJsonPath = this.config.toolsJsonPath;
    let raw: string;
    try {
      raw = fs.readFileSync(toolsJsonPath, "utf8");
    } catch (err) {
      const nodeErr = err as NodeJS.ErrnoException;
      if (nodeErr.code === "ENOENT") {
        // Neither a discovery sidecar (localhost:2999) NOR a tools.json is
        // present. For a managed agent with NO tools bound this is the
        // EXPECTED state, not a failure: the controller injects the discovery
        // sidecar + tools ConfigMap only when the agent has bindings, so a
        // zero-tools agent has nothing to discover. Return an empty manifest
        // so the managed loop advertises no tools and answers like a plain
        // chat agent. A tools-bound agent always has the ConfigMap mounted, so
        // this branch never silently drops its tools.
        return EMPTY_MANIFEST;
      }
      // A tools.json that EXISTS but cannot be read (bad mount) is a
      // genuinely broken manifest — surface it loudly rather than silently
      // running tool-less.
      throw new EndpointError(
        `tool manifest unavailable: ${url} was unreachable and ` +
          `${toolsJsonPath} could not be read`,
      );
    }

    let data: unknown;
    try {
      data = JSON.parse(raw);
    } catch {
      // A tools.json that exists but is malformed JSON is a genuinely broken
      // manifest — surface it loudly (mirrors Python's OSError/JSONDecodeError branch).
      throw new EndpointError(
        `tool manifest unavailable: ${url} was unreachable and ` +
          `${toolsJsonPath} could not be read`,
      );
    }

    if (typeof data === "object" && data !== null) {
      return data as Record<string, unknown>;
    }

    throw new EndpointError(
      `tool manifest unavailable: ${url} was unreachable and ` +
        `${toolsJsonPath} could not be read`,
    );
  }

  private async _find(name: string): Promise<Tool> {
    const tools = await this.list();
    const found = tools.find((t) => t.name === name);
    if (!found) {
      throw new ConfigError(`tool ${JSON.stringify(name)} is not in the current manifest`);
    }
    return found;
  }

  // ── invocation ────────────────────────────────────────────────────────────

  /**
   * Invoke a bound MCP tool by its catalog name; return its result.
   *
   * `name` is the discovery-manifest catalog key. The client resolves the
   * endpoint from the manifest, then resolves the real MCP tool name via the
   * server's `tools/list` (catalog names may differ from MCP names) before
   * issuing `tools/call`. The tool's text result is returned parsed as JSON when
   * it is a JSON document, else as the raw string.
   *
   * `opts.timeout` is a per-tool-call timeout in ms (default: 30s). The managed
   * loop's per-turn resilience supplies it.
   */
  async call(
    name: string,
    args: Record<string, unknown> = {},
    opts: { timeout?: number } = {},
  ): Promise<unknown> {
    const tool = await this._find(name);

    // knowledge_search is dispatched locally to the KnowledgeClient.
    if (tool.mode === "knowledge" && tool.transport === "internal") {
      return this._dispatchKnowledgeSearch(args);
    }

    if (!tool.endpoint) {
      throw new ConfigError(`tool ${JSON.stringify(name)} has no endpoint in the manifest`);
    }

    const rawText = await mcpCallTool(tool.endpoint, name, args, opts.timeout);
    try {
      return JSON.parse(rawText);
    } catch {
      return rawText;
    }
  }

  private async _dispatchKnowledgeSearch(args: Record<string, unknown>): Promise<unknown> {
    if (!this._knowledge) {
      throw new ConfigError(
        "knowledge_search: knowledge client is not wired into ToolsClient",
      );
    }
    const query = typeof args["query"] === "string" ? args["query"] : "";
    const kb = typeof args["knowledge_base"] === "string" ? args["knowledge_base"] : undefined;
    const topK = typeof args["top_k"] === "number" ? args["top_k"] : 10;
    return this._knowledge.search(query, kb, topK);
  }

  /**
   * Delegate a subtask to a roster sub-agent via the launcher-local endpoint (M64, ADR 0057).
   *
   * The launcher applies the spawn guard, starts the sub-agent as a durable SUB-RUN,
   * waits for it to finish, and returns `{ok, answer, error}`. The invoking user's run
   * capability is relayed so the sub-run acts on-behalf-of the SAME user (no re-consent).
   * `step` + `callId` are the idempotency key. A denial or failure comes back as `ok=false`
   * (an outcome the model reads), never an exception.
   */
  async delegate(
    subAgent: string,
    task: string,
    step?: string,
    callId?: string,
    opts: { suspend?: boolean; spawnRoot?: string; spawnDepth?: number } = {},
  ): Promise<Record<string, unknown>> {
    if (!this.config.delegateEnabled) {
      throw new ConfigError(
        "delegate is not enabled for this agent: DELEGATE_ENABLED is not set. " +
          "Add a team roster to the AgentDeployment to use tools.delegate.",
      );
    }
    const url = `${this.config.delegateBaseUrl}/delegate`;
    // `suspend` (L7, ADR 0091) asks the launcher, at depth 0, for a durable-suspend SIGNAL (resolved
    // endpoint, no spawn/await) instead of a blocking await. An older launcher ignores the flag and
    // blocks — the managed loop detects the missing `suspend` and threads the result inline.
    const payload: Record<string, unknown> = { subAgent, input: task, step, callId };
    if (opts.suspend) payload["suspend"] = true;
    const body = JSON.stringify(payload);
    const headers = mcpHeaders(undefined);
    headers["Content-Type"] = "application/json";
    // Relay the spawn-tree position (m108.6): the launcher's depth gate + guard key are otherwise
    // blind to SDK-driven delegations (they default to depth 0 / root ""). Only relayed when known.
    if (opts.spawnDepth !== undefined && opts.spawnDepth >= 0) {
      headers[SPAWN_DEPTH_HEADER] = String(opts.spawnDepth);
    }
    if (opts.spawnRoot) headers[SPAWN_ROOT_HEADER] = opts.spawnRoot;

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), DELEGATE_TIMEOUT_MS);
    let resp: Response;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers,
        body,
        signal: controller.signal,
      });
    } catch (err) {
      throw new EndpointError(`delegate request failed: ${(err as Error).message}`);
    } finally {
      clearTimeout(timer);
    }

    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new EndpointError(
        `delegate endpoint returned ${resp.status}: ${text.slice(0, 200)}`,
        { status: resp.status, body: text },
      );
    }

    const data: unknown = await resp.json();
    if (typeof data === "object" && data !== null) {
      return data as Record<string, unknown>;
    }
    return { ok: false, error: "malformed delegate response" };
  }

  /**
   * Hand the conversation off to a roster member via the launcher-local endpoint (M67).
   *
   * This is a TRANSFER, not a delegation: the launcher terminates this run and creates a
   * NEW run for the target agent on the SAME conversation. There is NO await and NO result
   * to consume — the target continues with the end user. Returns
   * `{ok, runId?, sourceRun?, handedOffTo?, error?}`. A refusal comes back as `ok=false`.
   */
  async handoff(
    targetAgent: string,
    message = "",
    includeHistory = true,
  ): Promise<Record<string, unknown>> {
    const url = `${this.config.delegateBaseUrl}/handoff`;
    // include_history (m83.6) defaults true (the target replays the full conversation history on the
    // transfer turn — today's behavior). false ⇒ hand off with `message` as a summary; the target
    // skips the full-history replay on that first turn.
    const body = JSON.stringify({ targetAgent, message, includeHistory });
    const headers = mcpHeaders(undefined);
    headers["Content-Type"] = "application/json";

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), HANDOFF_TIMEOUT_MS);
    let resp: Response;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers,
        body,
        signal: controller.signal,
      });
    } catch (err) {
      throw new EndpointError(`handoff request failed: ${(err as Error).message}`);
    } finally {
      clearTimeout(timer);
    }

    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new EndpointError(
        `handoff endpoint returned ${resp.status}: ${text.slice(0, 200)}`,
        { status: resp.status, body: text },
      );
    }

    const data: unknown = await resp.json();
    if (typeof data === "object" && data !== null) {
      return data as Record<string, unknown>;
    }
    return { ok: false, error: "malformed handoff response" };
  }
}

// ── minimal MCP streamable-http client ────────────────────────────────────────
//
// streamable-http transport (mcp-tools.md): the client POSTs JSON-RPC to the
// endpoint. The server responds either application/json or a text/event-stream
// SSE frame ("data: <json>"). The sequence is:
//   1. POST initialize        -> result + an Mcp-Session-Id response header
//   2. POST notifications/initialized (notification; no id, no response body)
//   3. POST tools/list        -> the server's real tool names (to resolve the
//                                catalog name -> the MCP name)
//   4. POST tools/call        -> the tool result (with the resolved MCP name)

function mcpHeaders(sessionId: string | undefined): Record<string, string> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    // A streamable-http server may reply with either representation.
    Accept: "application/json, text/event-stream",
  };
  if (sessionId) {
    headers["Mcp-Session-Id"] = sessionId;
  }
  // Relay the invoking user's run capability (ADR 0030 §3) on every tool-call
  // egress so the egress sidecar can resolve THAT user's credential.
  const capability = currentCapability();
  if (capability) {
    headers[CAPABILITY_HEADER] = capability;
  }
  // Relay the approval VOUCHER (ADR 0074 §3, m82.4) on every tool-call egress so the egress sidecar
  // forwards a require-approval tool the human GRANTED. Bound per-request in AsyncLocalStorage
  // (requestScope) from the resumed run's inbound X-Ctxmesh-Approval header; absent ⇒ no granted
  // require-approval tool (the sidecar returns 403 approval_required). The voucher is bound to one
  // {run, tool}; relaying it on every call is safe (a mismatched tool just gets the sidecar's 403).
  const voucher = currentApprovalVoucher();
  if (voucher) {
    headers[APPROVAL_HEADER] = voucher;
  }
  // Relay the record-mode capture toggle (M78, ADR 0071 §1/C1) on every tool-call
  // egress — the SAME request-scoped signal the model relay attaches (model.ts). It
  // lets the egress sidecar capture this call's tool I/O (pre-injection request +
  // verbatim upstream response) into the run's replay fixture (TOOL channel). Absent
  // ⇒ a non-recorded run — omit it, capture nothing.
  const recordRunId = currentRecordRunId();
  if (recordRunId) {
    headers[RECORD_HEADER] = recordRunId;
  }
  return headers;
}

function raiseIfConsentRequired(exc: EndpointError): void {
  if (exc.status !== 403 || !exc.body) return;
  let parsed: unknown;
  try {
    parsed = JSON.parse(exc.body);
  } catch {
    return;
  }
  if (
    typeof parsed === "object" &&
    parsed !== null &&
    (parsed as Record<string, unknown>)["error"] === "consent_required"
  ) {
    const server = String((parsed as Record<string, unknown>)["server"] ?? "");
    throw new ConsentRequiredError(
      `consent required: connect your account for MCP server ${JSON.stringify(server)}`,
      { server, status: exc.status, body: exc.body },
    );
  }
}

/**
 * Re-throw *exc* as an ApprovalRequiredError when it is the egress sidecar's structured
 * `approval_required` (ADR 0074 §3, m82.4) — a 403 whose JSON body carries
 * `{"error":"approval_required","tool":...,"run":...}`; otherwise return so the caller re-throws the
 * original error. String-free detection — keys on the status + machine-readable code.
 *
 * This makes the WIRE the enforcement point for require-approval even inside the managed loop: a
 * require-approval tool that reaches egress WITHOUT a valid voucher pauses the run for a human (the
 * same requires_action outcome `pauseForApproval` produces). A CUSTOM loop that ignores the throw is
 * simply denied — the floor. The key mirrors the managed loop's `tool:<name>` so an approval resolves
 * the SAME decision point on resume.
 */
function raiseIfApprovalRequired(exc: EndpointError): void {
  if (exc.status !== 403 || !exc.body) return;
  let parsed: unknown;
  try {
    parsed = JSON.parse(exc.body);
  } catch {
    return;
  }
  if (
    typeof parsed === "object" &&
    parsed !== null &&
    (parsed as Record<string, unknown>)["error"] === "approval_required"
  ) {
    const tool = String((parsed as Record<string, unknown>)["tool"] ?? "");
    throw new ApprovalRequiredError(`approval required for tool ${JSON.stringify(tool)}`, {
      key: `tool:${tool}`,
      summary: `Approve tool ${JSON.stringify(tool)}?`,
    });
  }
}

/**
 * Resolve the absolute POST URL for an MCP endpoint, following a same-origin 307
 * redirect exactly once (FastMCP/Starlette 307 /mcp → /mcp/).
 *
 * Cross-origin redirects are refused — never re-POST a body cross-origin.
 */
async function resolveEndpoint(
  endpoint: string,
  body: string,
  sessionId: string | undefined,
  timeoutMs: number,
): Promise<{ url: string; resp: Response }> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  let resp: Response;
  try {
    resp = await fetch(endpoint, {
      method: "POST",
      headers: mcpHeaders(sessionId),
      body,
      redirect: "manual", // we handle 307 ourselves to guard cross-origin
      signal: controller.signal,
    });
  } catch (err) {
    throw new EndpointError(`MCP request failed: ${(err as Error).message}`);
  } finally {
    clearTimeout(timer);
  }

  if (resp.status === 307 || resp.status === 308) {
    const location = resp.headers.get("location");
    if (!location) {
      throw new EndpointError(`MCP redirect (${resp.status}) from ${endpoint} has no Location`);
    }
    // Refuse cross-origin redirects.
    const srcOrigin = new URL(endpoint).origin;
    const dstOrigin = new URL(location, endpoint).origin;
    if (srcOrigin !== dstOrigin) {
      throw new EndpointError(
        `MCP cross-origin redirect refused: ${endpoint} → ${location} (cross-origin)`,
      );
    }
    const redirectUrl = new URL(location, endpoint).toString();
    const controller2 = new AbortController();
    const timer2 = setTimeout(() => controller2.abort(), timeoutMs);
    let resp2: Response;
    try {
      resp2 = await fetch(redirectUrl, {
        method: "POST",
        headers: mcpHeaders(sessionId),
        body,
        redirect: "manual",
        signal: controller2.signal,
      });
    } catch (err) {
      throw new EndpointError(`MCP request (after redirect) failed: ${(err as Error).message}`);
    } finally {
      clearTimeout(timer2);
    }
    return { url: redirectUrl, resp: resp2 };
  }

  return { url: endpoint, resp };
}

/**
 * POST one JSON-RPC message; return (parsed-result, session-id-header).
 *
 * `expectBody` — when false, the notification response (202 or empty) is accepted
 * without parsing the body.
 */
async function mcpPost(
  endpoint: string,
  payload: Record<string, unknown>,
  sessionId: string | undefined,
  opts: { expectBody: boolean; timeoutMs: number },
): Promise<{ result: Record<string, unknown> | null; sessionId: string | undefined; url: string }> {
  const body = JSON.stringify(payload);
  let url: string;
  let resp: Response;
  try {
    const resolved = await resolveEndpoint(endpoint, body, sessionId, opts.timeoutMs);
    url = resolved.url;
    resp = resolved.resp;
  } catch (exc) {
    if (exc instanceof EndpointError) {
      raiseIfConsentRequired(exc);
      raiseIfApprovalRequired(exc);
    }
    throw exc;
  }

  const status = resp.status;
  if (status !== 200 && status !== 202) {
    const text = await resp.text().catch(() => "");
    const err = new EndpointError(
      `MCP endpoint returned ${status}: ${text.slice(0, 200)}`,
      { status, body: text },
    );
    raiseIfConsentRequired(err);
    raiseIfApprovalRequired(err);
    throw err;
  }

  const newSessionId = resp.headers.get("mcp-session-id") ?? undefined;

  if (!opts.expectBody) {
    return { result: null, sessionId: newSessionId, url };
  }

  const contentType = resp.headers.get("content-type") ?? "";
  let text: string;
  try {
    text = await resp.text();
  } catch (err) {
    throw new EndpointError(`MCP response read failed: ${(err as Error).message}`);
  }

  const message = parseJsonRpc(text, contentType, url);
  if ("error" in message) {
    const err = message["error"] as Record<string, unknown>;
    throw new EndpointError(
      `MCP error from ${url}: ${String(err["message"] ?? JSON.stringify(err))}`,
      { status },
    );
  }

  const result = message["result"];
  return {
    result: typeof result === "object" && result !== null ? (result as Record<string, unknown>) : null,
    sessionId: newSessionId,
    url,
  };
}

function parseJsonRpc(
  text: string,
  contentType: string,
  endpoint: string,
): Record<string, unknown> {
  let jsonText = text;
  if (contentType.includes("text/event-stream")) {
    // SSE frames: lines beginning "data:"; take the last data payload.
    let dataLine: string | null = null;
    for (const line of text.split("\n")) {
      if (line.startsWith("data:")) {
        dataLine = line.slice("data:".length).trim();
      }
    }
    if (dataLine === null) {
      throw new EndpointError(`MCP SSE response from ${endpoint} contained no data frame`);
    }
    jsonText = dataLine;
  }
  try {
    const parsed: unknown = JSON.parse(jsonText);
    if (typeof parsed === "object" && parsed !== null) {
      return parsed as Record<string, unknown>;
    }
    throw new EndpointError(`MCP response from ${endpoint} was not a JSON object`);
  } catch (err) {
    if (err instanceof EndpointError) throw err;
    throw new EndpointError(
      `MCP response from ${endpoint} was not valid JSON: ${(err as Error).message}`,
    );
  }
}

/**
 * Full MCP session: initialize → notifications/initialized → tools/list →
 * resolve name → tools/call.
 *
 * `catalogName` is the discovery-manifest key; the actual MCP tool name is
 * discovered from the server and may differ. Returns the first text content
 * item of the tool result.
 *
 * `timeout` is the per-round-trip socket timeout in ms; `undefined` keeps the
 * historical TOOL_CALL_TIMEOUT_MS so an un-plumbed caller is unchanged.
 */
async function mcpCallTool(
  endpoint: string,
  catalogName: string,
  arguments_: Record<string, unknown>,
  timeout?: number,
): Promise<string> {
  const callTimeout =
    typeof timeout === "number" && isFinite(timeout) && timeout > 0
      ? timeout
      : TOOL_CALL_TIMEOUT_MS;

  // 1. initialize.
  const initRes = await mcpPost(
    endpoint,
    {
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: MCP_PROTOCOL_VERSION,
        capabilities: {},
        clientInfo: { name: "ctxmesh", version: "0.1.0" },
      },
    },
    undefined,
    { expectBody: true, timeoutMs: callTimeout },
  );
  const sessionId = initRes.sessionId;
  // The resolved endpoint URL (after any 307 redirect) — use for all subsequent calls.
  const resolvedEndpoint = initRes.url;

  // 2. notifications/initialized (a notification: no id, no response expected).
  await mcpPost(
    resolvedEndpoint,
    { jsonrpc: "2.0", method: "notifications/initialized" },
    sessionId,
    { expectBody: false, timeoutMs: callTimeout },
  );

  // 3. tools/list -> discover the server's real tool names.
  const listRes = await mcpPost(
    resolvedEndpoint,
    { jsonrpc: "2.0", id: 2, method: "tools/list", params: {} },
    sessionId,
    { expectBody: true, timeoutMs: callTimeout },
  );
  const serverNames = serverToolNames(listRes.result, resolvedEndpoint);
  const mcpName = resolveToolName(catalogName, serverNames, resolvedEndpoint);

  // 4. tools/call with the RESOLVED MCP name.
  const callRes = await mcpPost(
    resolvedEndpoint,
    {
      jsonrpc: "2.0",
      id: 3,
      method: "tools/call",
      params: { name: mcpName, arguments: arguments_ },
    },
    sessionId,
    { expectBody: true, timeoutMs: callTimeout },
  );

  return firstTextContent(callRes.result, resolvedEndpoint);
}

function serverToolNames(
  listResult: Record<string, unknown> | null,
  endpoint: string,
): string[] {
  if (!listResult || typeof listResult !== "object") {
    throw new EndpointError(`MCP tools/list at ${endpoint} returned no result object`);
  }
  const tools = listResult["tools"];
  if (!Array.isArray(tools)) {
    throw new EndpointError(`MCP tools/list at ${endpoint} returned no tools array`);
  }
  const names: string[] = [];
  for (const t of tools) {
    if (typeof t === "object" && t !== null) {
      const n = (t as Record<string, unknown>)["name"];
      if (typeof n === "string" && n) names.push(n);
    }
  }
  if (names.length === 0) {
    throw new EndpointError(`MCP server at ${endpoint} advertises no tools`);
  }
  return names;
}

/** Fold hyphen/underscore so catalog `word-count` matches MCP `word_count`. */
function normalizeName(name: string): string {
  return name.replace(/-/g, "_");
}

/**
 * Map a catalog name to a server MCP tool name.
 *
 * Precedence: exact match → hyphen/underscore-normalized match → if the server
 * advertises exactly one tool, use it → otherwise raise a clear error listing what
 * the server actually exposes.
 */
function resolveToolName(
  catalogName: string,
  serverNames: string[],
  endpoint: string,
): string {
  // 1. Exact.
  if (serverNames.includes(catalogName)) return catalogName;
  // 2. Normalized (hyphen<->underscore). Only accept an unambiguous match.
  const target = normalizeName(catalogName);
  const normalizedMatches = serverNames.filter((n) => normalizeName(n) === target);
  if (normalizedMatches.length === 1) return normalizedMatches[0]!;
  // 3. Sole-tool fallback.
  if (serverNames.length === 1) return serverNames[0]!;
  // 4. Give up with an actionable error.
  throw new ConfigError(
    `tool ${JSON.stringify(catalogName)} could not be resolved to an MCP tool at ` +
      `${endpoint}: the server exposes ${JSON.stringify(serverNames)}. ` +
      `(The discovery-catalog name may differ from the MCP tool name; a ` +
      `normalized match was ambiguous or absent.)`,
  );
}

function firstTextContent(
  result: Record<string, unknown> | null,
  endpoint: string,
): string {
  if (!result || typeof result !== "object") {
    throw new EndpointError(`MCP tools/call at ${endpoint} returned no result object`);
  }
  const content = result["content"];
  if (!Array.isArray(content) || content.length === 0) {
    throw new EndpointError(`MCP tools/call at ${endpoint} returned empty content`);
  }
  const first = content[0];
  if (typeof first === "object" && first !== null && "text" in first) {
    return String((first as Record<string, unknown>)["text"]);
  }
  throw new EndpointError(`MCP tools/call at ${endpoint} returned non-text content`);
}
