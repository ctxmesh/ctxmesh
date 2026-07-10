import { afterEach, describe, expect, it, vi } from "vitest";

import { api, ApiError } from "@/lib/api";

function mockFetch(body: unknown, ok = true, status = 200) {
  return vi.fn().mockResolvedValue({
    ok,
    status,
    json: async () => body,
  } as Response);
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("api client", () => {
  it("listAgents parses the BFF DTO", async () => {
    const payload = {
      agents: [
        {
          name: "echo",
          namespace: "default",
          image: "echo:latest",
          phase: "Ready",
          ready: true,
        },
      ],
    };
    vi.stubGlobal("fetch", mockFetch(payload));

    const res = await api.listAgents();
    expect(res.agents).toHaveLength(1);
    expect(res.agents[0].name).toBe("echo");
    expect(res.agents[0].ready).toBe(true);
  });

  it("health hits /api/health", async () => {
    const fetchMock = mockFetch({ status: "ok", version: "dev" });
    vi.stubGlobal("fetch", fetchMock);

    const res = await api.health();
    expect(res.status).toBe("ok");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/health",
      expect.objectContaining({ headers: { Accept: "application/json" } }),
    );
  });

  it("throws ApiError with the status on a non-2xx response", async () => {
    vi.stubGlobal("fetch", mockFetch({}, false, 403));

    await expect(api.listAgents()).rejects.toMatchObject({
      name: "ApiError",
      status: 403,
    });
    await expect(api.listAgents()).rejects.toBeInstanceOf(ApiError);
  });
});
