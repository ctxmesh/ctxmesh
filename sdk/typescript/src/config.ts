/**
 * Launcher-plane configuration: the env the SDK reads to find the localhost plane.
 * Parity with `sdk/python/src/ctxmesh/config.py` (the ground-truth env/port contract).
 *
 * The launcher (PID 1) injects a fixed set of env vars into every agent container
 * describing the localhost platform plane and the run context (ADR 0002). `PlaneConfig.fromEnv`
 * reads them; the per-capability clients consult the resulting config for their base URLs.
 *
 * Ports (spec table):
 *
 *     memory     localhost:$MEMORY_PORT      (default 2998)   gate: MEMORY_PORT set
 *     tools      localhost:2999              (fixed)          always addressable
 *     delegate   127.0.0.1:2994              (fixed)          gate: DELEGATE_ENABLED
 *     feedback   localhost:$FEEDBACK_PORT    (default 2995)   gate: FEEDBACK_PORT set
 *     otlp       $OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317)
 *
 * Run context: AGENT_NAME (+ version/role/registryId/promptVersion) and the
 * conversationId. conversationId is NOT a launcher env — the agent extracts it from
 * its inbound request payload and stamps it on outbound calls via `X-Conversation-Id`.
 */

import { NotInPodError } from "./errors.js";

/** The discovery sidecar always listens on this fixed localhost port (no env). */
export const DISCOVERY_PORT = 2999;
/** The synthetic delegate/handoff endpoint (:2994) the launcher proxies (127.0.0.1). */
export const DELEGATE_PORT = 2994;
export const DEFAULT_TOOLS_JSON_PATH = "/etc/agent/tools.json";

export const DEFAULT_MEMORY_PORT = 2998;
export const DEFAULT_FEEDBACK_PORT = 2995;
export const DEFAULT_OTLP_ENDPOINT = "localhost:4317";

/**
 * Any one of these env vars being present is proof we are running under the
 * launcher (some plane, some run context). Their combined absence means we are not
 * in a launcher pod and `fromEnv()` must fail fast.
 */
export const LAUNCHER_MARKERS = [
  "AGENT_NAME",
  "MEMORY_PORT",
  "MEMORY_BACKEND_ADDR",
  "FEEDBACK_PORT",
  "LANGFUSE_HOST",
] as const;

/** An env source: `process.env` (default) or an explicit map (tests). */
export type Env = Record<string, string | undefined>;

/** Parse a port env var, mirroring the launcher's own 1..65535 rule. */
function parsePort(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined || raw === "") {
    return fallback;
  }
  if (!/^\d+$/.test(raw)) {
    throw new NotInPodError(`${name}=${JSON.stringify(raw)} is not a valid port`);
  }
  const port = Number(raw);
  if (port < 1 || port > 65535) {
    throw new NotInPodError(`${name}=${JSON.stringify(raw)} out of range (1..65535)`);
  }
  return port;
}

function isTrue(raw: string | undefined): boolean {
  return (raw ?? "").trim().toLowerCase() === "true";
}

/** Identity + correlation the SDK stamps on plane calls (spec "Run context"). */
export interface RunContext {
  agentName: string;
  agentVersion: string;
  agentRole: string;
  agentRegistryId: string;
  promptVersion: string;
  /**
   * Default conversationId, if the agent chose to inject one via env. Usually
   * empty — conversationId is normally supplied per-call from the request.
   */
  conversationId: string;
}

/** Build a RunContext with empty defaults, overriding the given fields. */
export function makeRunContext(overrides: Partial<RunContext> = {}): RunContext {
  return {
    agentName: "",
    agentVersion: "",
    agentRole: "",
    agentRegistryId: "",
    promptVersion: "",
    conversationId: "",
    ...overrides,
  };
}

/** Options for `PlaneConfig.fromEnv`. */
export interface FromEnvOptions {
  /** When true (default), throw `NotInPodError` off-plane instead of silently no-oping. */
  requireLauncher?: boolean;
}

/** Explicit-URL overrides for `PlaneConfig.forTest` (the mock-plane ports). */
export interface ForTestOptions {
  memoryBaseUrl?: string;
  discoveryBaseUrl?: string;
  feedbackBaseUrl?: string;
  delegateBaseUrl?: string;
  modelGatewayUrl?: string;
  modelGatewayKey?: string;
  otlpEndpoint?: string;
  toolsJsonPath?: string;
  run?: RunContext;
  memoryWired?: boolean;
  longtermWired?: boolean;
  feedbackWired?: boolean;
  knowledgeEnabled?: boolean;
  delegateEnabled?: boolean;
}

function stripTrailingSlash(url: string): string {
  return url.replace(/\/+$/, "");
}

/** Resolved addresses + run context for the launcher localhost plane. */
export class PlaneConfig {
  /** memory (:2998) base URL. */
  readonly memoryBaseUrl: string;
  /** discovery/tools (:2999) base URL. */
  readonly discoveryBaseUrl: string;
  /** feedback (:2995) base URL. */
  readonly feedbackBaseUrl: string;
  /** synthetic delegate/handoff (:2994) base URL (127.0.0.1). */
  readonly delegateBaseUrl: string;
  /** the discovery sidecar's durable cold-start backing file. */
  readonly toolsJsonPath: string;
  /** identity + correlation stamped on plane calls. */
  readonly run: RunContext;

  /**
   * Whether each capability was actually wired by the launcher. A client for an
   * unwired capability raises ConfigError rather than hitting a dead port.
   */
  readonly memoryWired: boolean;
  /**
   * Whether long-term (agent-scope, ADR 0045) memory is enabled — the launcher
   * exposes /memory/agent/{remember,search}. Gate: MEMORY_LONGTERM_ENABLED=true.
   */
  readonly longtermWired: boolean;
  readonly feedbackWired: boolean;
  /** Whether the knowledge-base data plane (M68) is enabled. Gate: KNOWLEDGE_BASE_ENABLED=true. */
  readonly knowledgeEnabled: boolean;
  /** Whether synthetic delegate/handoff (:2994) is enabled. Gate: DELEGATE_ENABLED=true. */
  readonly delegateEnabled: boolean;

  /**
   * Model gateway base URL ($MODEL_GATEWAY_URL) — LiteLLM directly, or the
   * launcher's in-pod budget proxy when the agent is budgeted (transparent).
   * Empty when unwired (not in a pod); model.chat then raises ConfigError.
   */
  readonly modelGatewayUrl: string;
  /**
   * Bearer token the gateway expects; the launcher injects the master key in-pod.
   * Empty offline (the mock gateway ignores auth).
   */
  readonly modelGatewayKey: string;
  /**
   * OTLP/gRPC collector endpoint ($OTEL_EXPORTER_OTLP_ENDPOINT, default :4317).
   */
  readonly otlpEndpoint: string;

  constructor(init: {
    memoryBaseUrl: string;
    discoveryBaseUrl: string;
    feedbackBaseUrl: string;
    delegateBaseUrl: string;
    toolsJsonPath: string;
    run: RunContext;
    memoryWired: boolean;
    longtermWired: boolean;
    feedbackWired: boolean;
    knowledgeEnabled: boolean;
    delegateEnabled: boolean;
    modelGatewayUrl: string;
    modelGatewayKey: string;
    otlpEndpoint: string;
  }) {
    this.memoryBaseUrl = init.memoryBaseUrl;
    this.discoveryBaseUrl = init.discoveryBaseUrl;
    this.feedbackBaseUrl = init.feedbackBaseUrl;
    this.delegateBaseUrl = init.delegateBaseUrl;
    this.toolsJsonPath = init.toolsJsonPath;
    this.run = init.run;
    this.memoryWired = init.memoryWired;
    this.longtermWired = init.longtermWired;
    this.feedbackWired = init.feedbackWired;
    this.knowledgeEnabled = init.knowledgeEnabled;
    this.delegateEnabled = init.delegateEnabled;
    this.modelGatewayUrl = init.modelGatewayUrl;
    this.modelGatewayKey = init.modelGatewayKey;
    this.otlpEndpoint = init.otlpEndpoint;
  }

  /**
   * Build a PlaneConfig from the launcher-injected env.
   *
   * Fails fast with `NotInPodError` when no launcher marker env is present and
   * `requireLauncher` is true — never silently no-ops. Tests set
   * `requireLauncher: false` (or pass an `env` that includes a marker) to build an
   * offline config against a stub.
   */
  static fromEnv(
    env: Env = process.env,
    { requireLauncher = true }: FromEnvOptions = {},
  ): PlaneConfig {
    if (requireLauncher && !LAUNCHER_MARKERS.some((marker) => env[marker])) {
      throw new NotInPodError(
        "ctxmesh.agent.fromEnv(): no launcher environment detected (none of " +
          LAUNCHER_MARKERS.join(", ") +
          " is set). The SDK reads the launcher-injected localhost plane and only " +
          "works inside an agentry pod. For tests or offline use, build a " +
          "PlaneConfig explicitly and call agent.fromConfig(config).",
      );
    }

    const memoryPort = parsePort(env.MEMORY_PORT, DEFAULT_MEMORY_PORT, "MEMORY_PORT");
    const feedbackPort = parsePort(env.FEEDBACK_PORT, DEFAULT_FEEDBACK_PORT, "FEEDBACK_PORT");

    // Memory is wired iff the launcher started the :2998 listener (spec.sessionMemory
    // injected MEMORY_PORT / MEMORY_BACKEND_ADDR). Feedback is wired iff
    // LANGFUSE_HOST/FEEDBACK_PORT were injected (M9). Tools/:2999 is always
    // addressable when a discovery sidecar is present.
    const memoryWired = Boolean(env.MEMORY_PORT || env.MEMORY_BACKEND_ADDR);
    const feedbackWired = Boolean(env.FEEDBACK_PORT || env.LANGFUSE_HOST);

    const run = makeRunContext({
      agentName: env.AGENT_NAME ?? "",
      agentVersion: env.AGENT_VERSION ?? "",
      agentRole: env.AGENT_ROLE ?? "",
      agentRegistryId: env.AGENT_REGISTRY_ID ?? "",
      promptVersion: env.PROMPT_VERSION ?? "",
      conversationId: env.CONVERSATION_ID ?? "",
    });

    // Model gateway ($MODEL_GATEWAY_URL): LiteLLM directly, or the launcher's
    // budget proxy when budgeted — same wire either way. The bearer token is a
    // dummy in dev mode; honour an explicit MODEL_GATEWAY_KEY/OPENAI_API_KEY.
    const modelGatewayUrl = stripTrailingSlash(env.MODEL_GATEWAY_URL ?? "");
    const modelGatewayKey = env.MODEL_GATEWAY_KEY || env.OPENAI_API_KEY || "";

    return new PlaneConfig({
      memoryBaseUrl: `http://localhost:${memoryPort}`,
      discoveryBaseUrl: `http://localhost:${DISCOVERY_PORT}`,
      feedbackBaseUrl: `http://localhost:${feedbackPort}`,
      delegateBaseUrl: `http://127.0.0.1:${DELEGATE_PORT}`,
      toolsJsonPath: env.TOOLS_JSON_PATH || DEFAULT_TOOLS_JSON_PATH,
      run,
      memoryWired,
      longtermWired: isTrue(env.MEMORY_LONGTERM_ENABLED),
      feedbackWired,
      knowledgeEnabled: isTrue(env.KNOWLEDGE_BASE_ENABLED),
      delegateEnabled: isTrue(env.DELEGATE_ENABLED),
      modelGatewayUrl,
      modelGatewayKey,
      otlpEndpoint: env.OTEL_EXPORTER_OTLP_ENDPOINT ?? "",
    });
  }

  /**
   * Build a fully-wired config pointing at explicit URLs (the mock/test plane).
   *
   * The documented offline/test mode: point the base URLs at a fake localhost plane
   * so the clients can be exercised without a live launcher. `otlpEndpoint` defaults
   * empty so the trace client runs in offline/no-op export mode unless a test opts in.
   */
  static forTest(overrides: ForTestOptions = {}): PlaneConfig {
    return new PlaneConfig({
      memoryBaseUrl: stripTrailingSlash(overrides.memoryBaseUrl ?? "http://localhost:2998"),
      discoveryBaseUrl: stripTrailingSlash(overrides.discoveryBaseUrl ?? "http://localhost:2999"),
      feedbackBaseUrl: stripTrailingSlash(overrides.feedbackBaseUrl ?? "http://localhost:2995"),
      delegateBaseUrl: stripTrailingSlash(overrides.delegateBaseUrl ?? "http://127.0.0.1:2994"),
      toolsJsonPath: overrides.toolsJsonPath ?? DEFAULT_TOOLS_JSON_PATH,
      run: overrides.run ?? makeRunContext({ agentName: "test-agent" }),
      memoryWired: overrides.memoryWired ?? true,
      longtermWired: overrides.longtermWired ?? false,
      feedbackWired: overrides.feedbackWired ?? true,
      knowledgeEnabled: overrides.knowledgeEnabled ?? false,
      delegateEnabled: overrides.delegateEnabled ?? false,
      modelGatewayUrl: stripTrailingSlash(overrides.modelGatewayUrl ?? "http://localhost:2996"),
      modelGatewayKey: overrides.modelGatewayKey ?? "",
      otlpEndpoint: overrides.otlpEndpoint ?? "",
    });
  }
}
