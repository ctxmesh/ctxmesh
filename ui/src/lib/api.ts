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

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
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
};
