/**
 * ctxmesh — the agent-engine TypeScript SDK (parity with the Python `ctxmesh`).
 *
 * Foundation surface (M77.1): the launcher-plane configuration and the typed error
 * hierarchy. The data-plane clients (memory/knowledge/tools+MCP/feedback/model),
 * tracing, `serve()` + the managed loop land in M77.2–M77.6.
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
