/**
 * ctxmesh — the ctxmesh TypeScript SDK (parity with the Python `ctxmesh`).
 *
 * Foundation surface (M77.1): the launcher-plane configuration and the typed error
 * hierarchy. M77.2 adds the data-plane clients (memory/knowledge/feedback/model) and
 * the Client facade. Tools+MCP (M77.3), tracing + request-scope/capability + approvals
 * (M77.4), serve() + the managed tool-calling loop (M77.5), and the BFF
 * RunsClient (M77.6).
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

export { CAPABILITY_HEADER, currentCapability, capabilityScope } from "./_capability.js";

export { MemoryClient } from "./memory.js";
export { KnowledgeClient } from "./knowledge.js";
export type { KnowledgeResult } from "./knowledge.js";
export { FeedbackClient } from "./feedback.js";
export { ModelClient, ChatResponse } from "./model.js";
export type { ToolCall, ChatUsage } from "./model.js";

// Multimodal message-content helpers (O6) — surface parity with the Python SDK.
export { textPart, imageUrl, content } from "./_multimodal.js";
export type { TextPart, ImageUrlPart, ContentPart } from "./_multimodal.js";

export { Client } from "./client.js";

// ── M77.3: tools client + MCP ─────────────────────────────────────────────────

export {
  ToolsClient,
  Tool,
  DELEGATE_TOOL_NAME,
  HANDOFF_TOOL_NAME,
  KNOWLEDGE_SEARCH_TOOL_NAME,
} from "./tools.js";

// ── M77.4: tracing (the M10 span tree) + request-scope/capability + approvals ──

export { TraceClient, SpanHandle } from "./trace.js";
export type { SpanScope } from "./trace.js";
export { approvalScope, pauseForApproval } from "./_approval.js";

// The `agent` module: `agent.fromEnv()` / `agent.fromConfig()` — the primary entry points.
export * as agent from "./agent.js";

// ── M77.5: serve() + the managed loop ─────────────────────────────────────────

export {
  ManagedConfig,
  runManagedLoop,
  mintConversationId,
  DEFAULT_MAX_STEPS,
} from "./managed.js";
export type { ManagedResult, RunManagedLoopOptions, StepFrame } from "./managed.js";

export {
  serve,
  processInvoke,
  makeRequestHandler,
  parseBody,
  envelope,
} from "./serve.js";
export type { InvokeRequest, Handler, HandlerResult, ServeOptions } from "./serve.js";

// ── M77.6: the BFF RunsClient (caller-authenticated /api/runs — NOT the in-pod plane) ──

export { RunsClient, Run } from "./runs.js";
export type {
  RunEvent,
  RunsClientOptions,
  CreateRunOptions,
  RunOptions,
} from "./runs.js";
