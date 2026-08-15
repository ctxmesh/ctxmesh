/**
 * ctxmesh — the agent-engine TypeScript SDK (parity with the Python `ctxmesh`).
 *
 * Foundation surface (M77.1): the launcher-plane configuration and the typed error
 * hierarchy. M77.2 adds the data-plane clients (memory/knowledge/feedback/model) and
 * the Client facade. Tools+MCP (M77.3), tracing (M77.4), serve/managed-loop (M77.5)
 * land in subsequent tasks.
 */

export {
  PlaneConfig,
  makeRunContext,
  DISCOVERY_PORT,
  DELEGATE_PORT,
  DEFAULT_MEMORY_PORT,
  DEFAULT_FEEDBACK_PORT,
  DEFAULT_OTLP_ENDPOINT,
  DEFAULT_TOOLS_JSON_PATH,
  LAUNCHER_MARKERS,
} from "./config.js";
export type { RunContext, Env, FromEnvOptions, ForTestOptions } from "./config.js";

export {
  CtxmeshError,
  ConfigError,
  NotInPodError,
  EndpointError,
  ConsentRequiredError,
  GuardrailBlockedError,
  ApprovalRequiredError,
} from "./errors.js";

// ── M77.2: data-plane clients + facade ────────────────────────────────────────

export { CAPABILITY_HEADER, currentCapability } from "./_capability.js";

export { MemoryClient } from "./memory.js";
export { KnowledgeClient } from "./knowledge.js";
export type { KnowledgeResult } from "./knowledge.js";
export { FeedbackClient } from "./feedback.js";
export { ModelClient, ChatResponse } from "./model.js";
export type { ToolCall, ChatUsage } from "./model.js";

export { Client } from "./client.js";

// ── M77.3: tools client + MCP ─────────────────────────────────────────────────

export {
  ToolsClient,
  Tool,
  DELEGATE_TOOL_NAME,
  HANDOFF_TOOL_NAME,
  KNOWLEDGE_SEARCH_TOOL_NAME,
} from "./tools.js";

// The `agent` module: `agent.fromEnv()` / `agent.fromConfig()` — the primary entry points.
export * as agent from "./agent.js";
