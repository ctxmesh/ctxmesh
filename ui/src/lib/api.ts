// Typed client for the Go BFF. The SPA is served same-origin by the BFF, so all
// requests are relative "/api/*" — the browser never holds K8s/Langfuse creds;
// the BFF injects them server-side (ADR 0010). Auth is carried by the browser's
// session (the M11 control-plane auth in front of the BFF); nothing is added
// here.

export interface HealthResponse {
  status: string;
  version: string;
}

// Mirrors the BFF's AgentSummary DTO (internal/bff): the SPA gets a flat,
// UI-shaped projection, NOT the raw CRD.
export interface AgentSummary {
  name: string;
  namespace: string;
  image: string;
  phase: string;
  ready: boolean;
}

export interface AgentListResponse {
  agents: AgentSummary[];
}

// --- Topology (GET /api/topology) -------------------------------------------
// Mirrors the BFF's flat graph DTO: a list of nodes + edges. The dashboard's
// React Flow view builds its graph from these; the SPA never sees raw CRDs.

export type TopologyNodeKind = "registry" | "agent" | "tool";
export type TopologyHealth =
  | "ready"
  | "notReady"
  | "pending"
  | "unknown";

export interface TopologyNode {
  id: string;
  kind: TopologyNodeKind;
  name: string;
  namespace: string;
  health: TopologyHealth;
  detail: string;
}

export interface TopologyEdge {
  id: string;
  source: string;
  target: string;
}

export interface TopologyResponse {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
}

// --- Cost / usage (GET /api/cost) -------------------------------------------

export interface MetricPoint {
  label: string;
  value: number;
}

export interface CostSummary {
  totalCostUSD: number;
  totalTokens: number;
  observations: number;
  byModel: MetricPoint[];
}

export interface CostResponse {
  summary: CostSummary;
  latency: MetricPoint[];
  scale: MetricPoint[];
}

// --- Recent runs (GET /api/runs) --------------------------------------------

export interface RunSummary {
  traceId: string;
  name: string;
  timestamp: string;
  costUSD: number;
  tokens: number;
  latencyMs: number;
}

export interface RunListResponse {
  runs: RunSummary[];
}

// --- Trace link (GET /api/traces/{id}) --------------------------------------
// The one Langfuse target for a traceId — the embedded iframe src AND the
// link-out href. Resolved server-side so the SPA never hardcodes a Langfuse URL.

export interface TraceLinkResponse {
  traceId: string;
  url: string;
}

// --- Config builder (POST /api/expand, POST /api/agents) --------------------
// The config-builder submits the SAME simplified agent.yaml the CLI consumes:
// the form builds the YAML, /api/expand previews the CRD (server-side, the CLI
// expand core), and /api/agents applies it (client-go create, RBAC-scoped). The
// browser never hand-edits raw CRDs.

// One created CRD object's flat identity (mirrors the BFF createdObject DTO).
export interface CreatedObject {
  kind: string;
  name: string;
  namespace: string;
}

export interface CreateAgentResponse {
  created: CreatedObject[];
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// errorMessage extracts the BFF's JSON {"error": "..."} body when present, so a
// validation 400 / RBAC 403 surfaces its real reason (not just a status code).
async function errorMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === "string" && body.error) return body.error;
  } catch {
    // Non-JSON body — fall through to the generic message.
  }
  return fallback;
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, {
    headers: { Accept: "application/json" },
    signal,
  });
  if (!res.ok) {
    throw new ApiError(`${path} failed (${res.status})`, res.status);
  }
  return (await res.json()) as T;
}

export const api = {
  health: (signal?: AbortSignal) =>
    getJSON<HealthResponse>("/api/health", signal),
  listAgents: (signal?: AbortSignal) =>
    getJSON<AgentListResponse>("/api/agents", signal),
  topology: (signal?: AbortSignal) =>
    getJSON<TopologyResponse>("/api/topology", signal),
  cost: (signal?: AbortSignal) => getJSON<CostResponse>("/api/cost", signal),
  runs: (signal?: AbortSignal) =>
    getJSON<RunListResponse>("/api/runs", signal),
  traceLink: (traceId: string, signal?: AbortSignal) =>
    getJSON<TraceLinkResponse>(
      `/api/traces/${encodeURIComponent(traceId)}`,
      signal,
    ),

  // expand posts the form's agent.yaml and returns the expanded CRD manifest(s)
  // as plain YAML text — the config-builder's read-only preview. A validation
  // failure (400) surfaces the BFF's message via ApiError.
  expand: async (agentYAML: string, signal?: AbortSignal): Promise<string> => {
    const res = await fetch("/api/expand", {
      method: "POST",
      headers: { "Content-Type": "application/yaml" },
      body: agentYAML,
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `expand failed (${res.status})`),
        res.status,
      );
    }
    return res.text();
  },

  // createAgent applies the previewed agent.yaml (the SAME body /api/expand
  // received) via the BFF's client-go create path. Returns the created objects'
  // identities. A 403 (RBAC viewer), 409 (already exists) or 400 (invalid)
  // surfaces the BFF message via ApiError.
  createAgent: async (
    agentYAML: string,
    namespace: string,
    signal?: AbortSignal,
  ): Promise<CreateAgentResponse> => {
    const res = await fetch("/api/agents", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agentYAML, namespace }),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CreateAgentResponse;
  },
};
