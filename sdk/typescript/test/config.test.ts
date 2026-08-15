/**
 * PlaneConfig.fromEnv() plane resolution + fail-fast-outside-a-pod contract.
 * Parity with `sdk/python/tests/test_config.py`.
 */

import { describe, it, expect } from "vitest";

import {
  PlaneConfig,
  DEFAULT_FEEDBACK_PORT,
  DEFAULT_MEMORY_PORT,
  DELEGATE_PORT,
} from "../src/config.js";
import { NotInPodError } from "../src/errors.js";
import { startPlane } from "./plane.js";

describe("PlaneConfig.fromEnv", () => {
  it("fails fast without a launcher marker (NotInPodError, no silent no-op)", () => {
    let err: unknown;
    try {
      PlaneConfig.fromEnv({});
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(NotInPodError);
    // The message must be actionable: mention it needs a launcher pod.
    expect(String((err as Error).message).toLowerCase()).toContain("launcher");
  });

  it("does not fail off-plane when requireLauncher is false", () => {
    const cfg = PlaneConfig.fromEnv({}, { requireLauncher: false });
    expect(cfg.memoryBaseUrl).toBe(`http://localhost:${DEFAULT_MEMORY_PORT}`);
    expect(cfg.memoryWired).toBe(false);
    expect(cfg.feedbackWired).toBe(false);
  });

  it("reads injected ports and run context", () => {
    const cfg = PlaneConfig.fromEnv({
      AGENT_NAME: "billing-agent",
      AGENT_VERSION: "1.2.3",
      AGENT_ROLE: "assistant",
      AGENT_REGISTRY_ID: "reg-1",
      PROMPT_VERSION: "pv-7",
      MEMORY_PORT: "2998",
      MEMORY_BACKEND_ADDR: "valkey:6379",
      FEEDBACK_PORT: "2995",
      LANGFUSE_HOST: "http://langfuse",
    });
    expect(cfg.memoryBaseUrl).toBe("http://localhost:2998");
    expect(cfg.discoveryBaseUrl).toBe("http://localhost:2999");
    expect(cfg.feedbackBaseUrl).toBe("http://localhost:2995");
    expect(cfg.delegateBaseUrl).toBe(`http://127.0.0.1:${DELEGATE_PORT}`);
    expect(cfg.memoryWired).toBe(true);
    expect(cfg.feedbackWired).toBe(true);
    expect(cfg.run.agentName).toBe("billing-agent");
    expect(cfg.run.agentVersion).toBe("1.2.3");
    expect(cfg.run.agentRole).toBe("assistant");
    expect(cfg.run.agentRegistryId).toBe("reg-1");
    expect(cfg.run.promptVersion).toBe("pv-7");
  });

  it("defaults ports when unset but a marker is present, and leaves capabilities unwired", () => {
    const cfg = PlaneConfig.fromEnv({ AGENT_NAME: "a" });
    expect(cfg.memoryBaseUrl).toBe(`http://localhost:${DEFAULT_MEMORY_PORT}`);
    expect(cfg.feedbackBaseUrl).toBe(`http://localhost:${DEFAULT_FEEDBACK_PORT}`);
    expect(cfg.memoryWired).toBe(false);
    expect(cfg.feedbackWired).toBe(false);
  });

  it("rejects a non-numeric or out-of-range port", () => {
    expect(() => PlaneConfig.fromEnv({ AGENT_NAME: "a", MEMORY_PORT: "not-a-port" })).toThrow(
      NotInPodError,
    );
    expect(() => PlaneConfig.fromEnv({ AGENT_NAME: "a", FEEDBACK_PORT: "70000" })).toThrow(
      NotInPodError,
    );
  });

  it("resolves the model gateway url/key and the OTLP endpoint from env", () => {
    const cfg = PlaneConfig.fromEnv({
      AGENT_NAME: "a",
      MODEL_GATEWAY_URL: "http://gw:4000/",
      OPENAI_API_KEY: "sk-fallback",
      OTEL_EXPORTER_OTLP_ENDPOINT: "collector:4317",
    });
    // Trailing slash is stripped; OPENAI_API_KEY is the fallback bearer.
    expect(cfg.modelGatewayUrl).toBe("http://gw:4000");
    expect(cfg.modelGatewayKey).toBe("sk-fallback");
    expect(cfg.otlpEndpoint).toBe("collector:4317");

    // MODEL_GATEWAY_KEY takes precedence over OPENAI_API_KEY.
    const cfg2 = PlaneConfig.fromEnv({
      AGENT_NAME: "a",
      MODEL_GATEWAY_KEY: "sk-primary",
      OPENAI_API_KEY: "sk-fallback",
    });
    expect(cfg2.modelGatewayKey).toBe("sk-primary");
  });

  it("reads the boolean capability gates", () => {
    const off = PlaneConfig.fromEnv({ AGENT_NAME: "a" });
    expect(off.longtermWired).toBe(false);
    expect(off.knowledgeEnabled).toBe(false);
    expect(off.delegateEnabled).toBe(false);

    const on = PlaneConfig.fromEnv({
      AGENT_NAME: "a",
      MEMORY_LONGTERM_ENABLED: "true",
      KNOWLEDGE_BASE_ENABLED: "TRUE",
      DELEGATE_ENABLED: "true",
    });
    expect(on.longtermWired).toBe(true);
    expect(on.knowledgeEnabled).toBe(true);
    expect(on.delegateEnabled).toBe(true);
  });

  it("honours an explicit TOOLS_JSON_PATH, else the default", () => {
    expect(PlaneConfig.fromEnv({ AGENT_NAME: "a" }).toolsJsonPath).toBe("/etc/agent/tools.json");
    expect(
      PlaneConfig.fromEnv({ AGENT_NAME: "a", TOOLS_JSON_PATH: "/custom/tools.json" }).toolsJsonPath,
    ).toBe("/custom/tools.json");
  });
});

describe("PlaneConfig.forTest", () => {
  it("builds a fully-wired config pointing at explicit URLs", () => {
    const cfg = PlaneConfig.forTest();
    expect(cfg.memoryWired).toBe(true);
    expect(cfg.feedbackWired).toBe(true);
    expect(cfg.discoveryBaseUrl.startsWith("http://localhost:2999")).toBe(true);
    expect(cfg.run.agentName).toBe("test-agent");
  });

  it("applies overrides and strips trailing slashes", () => {
    const cfg = PlaneConfig.forTest({
      memoryBaseUrl: "http://127.0.0.1:9998/",
      modelGatewayUrl: "http://127.0.0.1:9996/",
    });
    expect(cfg.memoryBaseUrl).toBe("http://127.0.0.1:9998");
    expect(cfg.modelGatewayUrl).toBe("http://127.0.0.1:9996");
  });
});

describe("the mock launcher plane", () => {
  it("starts the stubs on ephemeral ports and wires a PlaneConfig at them", async () => {
    const plane = await startPlane();
    try {
      // Each stub bound a real localhost port and the config points at it.
      expect(plane.config.memoryBaseUrl).toBe(plane.memory.baseUrl);
      expect(plane.config.discoveryBaseUrl).toBe(plane.discovery.baseUrl);
      expect(plane.config.feedbackBaseUrl).toBe(plane.feedback.baseUrl);
      expect(plane.config.modelGatewayUrl).toBe(plane.gateway.baseUrl);
      expect(plane.config.memoryWired).toBe(true);
    } finally {
      await plane.stop();
    }
  });

  it("the memory stub answers put/get/append round-trips over http", async () => {
    const plane = await startPlane();
    try {
      const base = plane.memory.baseUrl;
      // PUT then GET.
      let res = await fetch(`${base}/memory/conv-1`, {
        method: "PUT",
        body: JSON.stringify([{ role: "user", content: "hi" }]),
      });
      expect(res.status).toBe(204);
      res = await fetch(`${base}/memory/conv-1`);
      expect(res.status).toBe(200);
      expect(await res.json()).toEqual([{ role: "user", content: "hi" }]);

      // APPEND then search.
      res = await fetch(`${base}/memory/conv-1/append`, {
        method: "POST",
        body: JSON.stringify({ role: "assistant", content: "needle" }),
      });
      expect(res.status).toBe(204);
      res = await fetch(`${base}/memory/conv-1/search?q=needle`);
      expect(await res.json()).toEqual([{ role: "assistant", content: "needle" }]);

      // Every request was recorded for assertions.
      expect(plane.memory.requests.length).toBeGreaterThanOrEqual(4);
    } finally {
      await plane.stop();
    }
  });

  it("the gateway stub returns an OpenAI-shaped completion", async () => {
    const plane = await startPlane({ gateway: { content: "hello" } });
    try {
      const res = await fetch(`${plane.gateway.baseUrl}/chat/completions`, {
        method: "POST",
        headers: { Authorization: "Bearer sk-test" },
        body: JSON.stringify({ model: "gpt-4o-mini", messages: [] }),
      });
      const body = (await res.json()) as { choices: Array<{ message: { content: string } }> };
      expect(body.choices[0]?.message.content).toBe("hello");
      expect(plane.gateway.requests[0]?.headers.authorization).toBe("Bearer sk-test");
    } finally {
      await plane.stop();
    }
  });

  it("the feedback stub 400s a payload with no traceId and 202s a valid one", async () => {
    const plane = await startPlane();
    try {
      const base = plane.feedback.baseUrl;
      let res = await fetch(`${base}/feedback`, { method: "POST", body: JSON.stringify({}) });
      expect(res.status).toBe(400);
      res = await fetch(`${base}/feedback`, {
        method: "POST",
        body: JSON.stringify({ traceId: "t-1", name: "quality", value: 1 }),
      });
      expect(res.status).toBe(202);
    } finally {
      await plane.stop();
    }
  });
});
