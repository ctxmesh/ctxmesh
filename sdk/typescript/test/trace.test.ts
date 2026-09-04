/**
 * THE FEASIBILITY-GATE test (M77.4, ADR 0070 §2) — the whole point of the task.
 *
 * Proves the JS OpenTelemetry + OpenInference stack emits a span tree whose KINDS +
 * ATTRIBUTES MATCH the Python SDK's output (the M10 invariant), and that the request-scope
 * capability relay + pause-for-approval behave at parity with `_capability.py`/`_approval.py`.
 *
 * The parity reference is `sdk/python/src/ctxmesh/{trace,_semconv,_capability,_approval}.py`
 * + its tests. The attribute keys/values asserted below are the exact strings those modules
 * emit:
 *
 *     openinference.span.kind  = AGENT | CHAIN | TOOL | LLM
 *     input.value / output.value / tool.name / llm.model_name
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  OpenInferenceSpanKind,
  SemanticConventions,
} from "@arizeai/openinference-semantic-conventions";

import { currentCapability } from "../src/_capability.js";
import { Client } from "../src/client.js";
import { ApprovalRequiredError } from "../src/errors.js";
import { pauseForApproval } from "../src/_approval.js";
import { startPlane, type MockPlane } from "../src/testing.js";

// The OpenInference keys/values — read straight from the JS package so the test asserts the
// SAME source of truth the SDK emits (byte-for-byte the Python `_semconv.py` constants).
const SPAN_KIND = SemanticConventions.OPENINFERENCE_SPAN_KIND;
const INPUT_VALUE = SemanticConventions.INPUT_VALUE;
const OUTPUT_VALUE = SemanticConventions.OUTPUT_VALUE;
const TOOL_NAME = SemanticConventions.TOOL_NAME;
const LLM_MODEL_NAME = SemanticConventions.LLM_MODEL_NAME;

// A W3C traceparent standing in for the launcher's injected `agent.invoke` context.
const LAUNCHER_TRACE_ID = "0af7651916cd43dd8448eb211c80319c";
const LAUNCHER_SPAN_ID = "b7ad6b7169203331";
const TRACEPARENT = `00-${LAUNCHER_TRACE_ID}-${LAUNCHER_SPAN_ID}-01`;

/** OTel-JS 2.x: the parent span id lives on `parentSpanContext.spanId` (not `parentSpanId`). */
function parentSpanId(span: { parentSpanContext?: { spanId: string } }): string | undefined {
  return span.parentSpanContext?.spanId;
}

let plane: MockPlane;
let client: Client;

beforeEach(async () => {
  plane = await startPlane();
  // Wire the TraceClient to the in-memory span exporter (the Python InMemorySpanExporter fixture).
  client = new Client(plane.config, { spanProcessor: plane.spans.processor });
});

afterEach(async () => {
  plane.spans.reset();
  await plane.stop();
});

describe("TraceClient — the M10 span tree (kinds + attributes match Python)", () => {
  it("emits AGENT -> step(CHAIN) -> tool(TOOL) + llm(LLM) with the correct kinds", () => {
    client.trace.loop("research", undefined, () => {
      client.trace.step("plan", (step) => {
        step.setInput("user prompt");
        client.trace.tool("web_search", { q: "otel" }, (t) => {
          t.setOutput({ results: [] });
        });
        client.trace.llm("gen", "gpt-4o-mini", "prompt", (llm) => {
          llm.setOutput("answer");
        });
        step.setOutput("planned");
      });
    });

    const spans = plane.spans.finishedSpans();
    expect(spans.length).toBe(4);

    const agent = plane.spans.byName("research")!;
    const step = plane.spans.byName("plan")!;
    const tool = plane.spans.byName("web_search")!;
    const llm = plane.spans.byName("gen")!;

    // (a) each span's OpenInference kind attribute is exactly AGENT/CHAIN/TOOL/LLM
    expect(agent.attributes[SPAN_KIND]).toBe(OpenInferenceSpanKind.AGENT);
    expect(step.attributes[SPAN_KIND]).toBe(OpenInferenceSpanKind.CHAIN);
    expect(tool.attributes[SPAN_KIND]).toBe(OpenInferenceSpanKind.TOOL);
    expect(llm.attributes[SPAN_KIND]).toBe(OpenInferenceSpanKind.LLM);

    // The literal string values (match Python's OpenInferenceSpanKindValues.*.value).
    expect(agent.attributes[SPAN_KIND]).toBe("AGENT");
    expect(step.attributes[SPAN_KIND]).toBe("CHAIN");
    expect(tool.attributes[SPAN_KIND]).toBe("TOOL");
    expect(llm.attributes[SPAN_KIND]).toBe("LLM");
  });

  it("sets input.value / output.value / tool.name / llm.model_name at the Python keys", () => {
    client.trace.step("plan", (step) => {
      step.setInput("hello");
      client.trace.tool("web_search", { q: "x" }, (t) => {
        t.setOutput({ ok: true });
      });
      client.trace.llm("gen", "claude-sonnet", undefined, () => undefined);
      step.setOutput("done");
    });

    const step = plane.spans.byName("plan")!;
    const tool = plane.spans.byName("web_search")!;
    const llm = plane.spans.byName("gen")!;

    // (b) input.value/output.value/tool.name/llm.model_name match the Python keys + values.
    expect(step.attributes[INPUT_VALUE]).toBe("hello"); // a str passes through verbatim
    expect(step.attributes[OUTPUT_VALUE]).toBe("done");
    // tool input is compact-JSON-encoded (the Python `_to_value` contract).
    expect(tool.attributes[INPUT_VALUE]).toBe('{"q":"x"}');
    expect(tool.attributes[OUTPUT_VALUE]).toBe('{"ok":true}');
    expect(tool.attributes[TOOL_NAME]).toBe("web_search");
    expect(llm.attributes[LLM_MODEL_NAME]).toBe("claude-sonnet");

    // The keys themselves are the exact Python strings.
    expect(SPAN_KIND).toBe("openinference.span.kind");
    expect(INPUT_VALUE).toBe("input.value");
    expect(OUTPUT_VALUE).toBe("output.value");
    expect(TOOL_NAME).toBe("tool.name");
    expect(LLM_MODEL_NAME).toBe("llm.model_name");
  });

  it("(c) nests correctly: tool + llm parent to step, step to agent, all one trace", () => {
    client.trace.loop("agent", undefined, () => {
      client.trace.step("plan", () => {
        client.trace.tool("t", null, () => undefined);
        client.trace.llm("m", "gpt", undefined, () => undefined);
      });
    });

    const agent = plane.spans.byName("agent")!;
    const step = plane.spans.byName("plan")!;
    const tool = plane.spans.byName("t")!;
    const llm = plane.spans.byName("m")!;

    // step is a child of agent; tool + llm are children of step.
    expect(parentSpanId(step)).toBe(agent.spanContext().spanId);
    expect(parentSpanId(tool)).toBe(step.spanContext().spanId);
    expect(parentSpanId(llm)).toBe(step.spanContext().spanId);

    // The whole tree shares ONE trace id.
    const traceId = agent.spanContext().traceId;
    for (const s of plane.spans.finishedSpans()) {
      expect(s.spanContext().traceId).toBe(traceId);
    }
  });

  it("(c) roots the tree under the launcher's inbound traceparent (the M10 invariant)", () => {
    const headers = { traceparent: TRACEPARENT };

    client.trace.loop("agent", headers, () => {
      client.trace.step("plan", () => {
        client.trace.tool("t", null, () => undefined);
      });
    });

    const agent = plane.spans.byName("agent")!;
    const step = plane.spans.byName("plan")!;
    const tool = plane.spans.byName("t")!;

    // The AGENT root's parent is the launcher's agent.invoke span (not a detached root).
    expect(parentSpanId(agent)).toBe(LAUNCHER_SPAN_ID);
    // The whole tree shares the launcher's trace id.
    expect(agent.spanContext().traceId).toBe(LAUNCHER_TRACE_ID);
    expect(step.spanContext().traceId).toBe(LAUNCHER_TRACE_ID);
    expect(tool.spanContext().traceId).toBe(LAUNCHER_TRACE_ID);
  });

  it("requestContext roots a step tree under the inbound traceparent", () => {
    const headers = { traceparent: TRACEPARENT };
    client.trace.requestContext(headers, () => {
      client.trace.step("plan", () => undefined);
    });
    const step = plane.spans.byName("plan")!;
    expect(step.spanContext().traceId).toBe(LAUNCHER_TRACE_ID);
    expect(parentSpanId(step)).toBe(LAUNCHER_SPAN_ID);
  });

  it("an absent traceparent starts a fresh root trace (no error)", () => {
    client.trace.loop("agent", {}, () => undefined);
    const agent = plane.spans.byName("agent")!;
    expect(agent.spanContext().traceId).not.toBe(LAUNCHER_TRACE_ID);
    expect(parentSpanId(agent)).toBeUndefined();
  });

  it("marks a span ERROR + records the exception, then re-raises (never swallowed)", () => {
    expect(() =>
      client.trace.step("boom", () => {
        throw new Error("kaboom");
      }),
    ).toThrow("kaboom");
    const step = plane.spans.byName("boom")!;
    // OTel StatusCode.ERROR === 2
    expect(step.status.code).toBe(2);
    expect(step.events.some((e) => e.name === "exception")).toBe(true);
  });

  it("setAttribute on an ended span is a no-op (never throws)", () => {
    let captured: import("../src/trace.js").SpanHandle | undefined;
    client.trace.step("s", (h) => {
      captured = h;
    });
    // The span has ended; mutating the handle must be a safe no-op.
    expect(() => captured!.setOutput("late")).not.toThrow();
    const s = plane.spans.byName("s")!;
    expect(s.attributes[OUTPUT_VALUE]).toBeUndefined();
  });

  it("works across await — an async tool nests under an async step (real loop shape)", async () => {
    await client.trace.loop("agent", { traceparent: TRACEPARENT }, async () => {
      await client.trace.step("plan", async (step) => {
        step.setInput("q");
        await client.trace.tool("t", { a: 1 }, async () => {
          await Promise.resolve();
        });
      });
    });
    const agent = plane.spans.byName("agent")!;
    const step = plane.spans.byName("plan")!;
    const tool = plane.spans.byName("t")!;
    expect(parentSpanId(agent)).toBe(LAUNCHER_SPAN_ID);
    expect(parentSpanId(step)).toBe(agent.spanContext().spanId);
    expect(parentSpanId(tool)).toBe(step.spanContext().spanId);
  });
});

describe("requestScope — capability relay (DX-2) + async isolation", () => {
  it("(d) binds X-Ctxmesh-Run-Capability so a tool call inside relays it", async () => {
    const headers = { "X-Ctxmesh-Run-Capability": "cap-user-123" };
    let seen: string | undefined;
    await client.requestScope(headers, undefined, async () => {
      seen = currentCapability();
      // The MCP tool call inside the scope carries the capability header.
      await client.tools.call("word-count", { text: "a b c" });
    });
    expect(seen).toBe("cap-user-123");

    // The mock discovery/MCP stub recorded the inbound requests — assert the header relayed.
    const relayed = plane.discovery.requests.some(
      (r) => r.headers["x-ctxmesh-run-capability"] === "cap-user-123",
    );
    expect(relayed).toBe(true);
  });

  it("outside any requestScope, currentCapability is undefined (no relay)", () => {
    expect(currentCapability()).toBeUndefined();
  });

  it("does not leak a capability across sibling scopes (no cross-user bleed)", async () => {
    const a = client.requestScope({ "X-Ctxmesh-Run-Capability": "cap-A" }, undefined, async () => {
      await Promise.resolve();
      return currentCapability();
    });
    const b = client.requestScope({ "X-Ctxmesh-Run-Capability": "cap-B" }, undefined, async () => {
      await Promise.resolve();
      return currentCapability();
    });
    expect(await a).toBe("cap-A");
    expect(await b).toBe("cap-B");
    expect(currentCapability()).toBeUndefined();
  });

  it("a blank capability header binds no capability (unattended run)", async () => {
    let seen: string | undefined = "sentinel";
    await client.requestScope({ "X-Ctxmesh-Run-Capability": "   " }, undefined, async () => {
      seen = currentCapability();
    });
    expect(seen).toBeUndefined();
  });

  it("requestScope also roots the trace under the inbound traceparent", () => {
    client.requestScope({ traceparent: TRACEPARENT }, undefined, () => {
      client.trace.step("plan", () => undefined);
    });
    const step = plane.spans.byName("plan")!;
    expect(step.spanContext().traceId).toBe(LAUNCHER_TRACE_ID);
    expect(parentSpanId(step)).toBe(LAUNCHER_SPAN_ID);
  });
});

describe("pauseForApproval — request-scoped HITL (parity with _approval.py)", () => {
  it("(e) throws ApprovalRequiredError when the key is NOT granted", () => {
    client.requestScope(undefined, [], () => {
      expect(() => pauseForApproval("send-email", "Send the drafted email")).toThrow(
        ApprovalRequiredError,
      );
    });
  });

  it("(e) returns (no throw) when the key IS granted", () => {
    client.requestScope(undefined, ["send-email"], () => {
      expect(() => pauseForApproval("send-email", "Send the drafted email")).not.toThrow();
    });
  });

  it("carries key + summary on the error for the managed loop to surface", () => {
    let err: ApprovalRequiredError | undefined;
    client.requestScope(undefined, undefined, () => {
      try {
        pauseForApproval("wire-transfer", "Transfer $500");
      } catch (e) {
        err = e as ApprovalRequiredError;
      }
    });
    expect(err).toBeInstanceOf(ApprovalRequiredError);
    expect(err!.key).toBe("wire-transfer");
    expect(err!.summary).toBe("Transfer $500");
  });

  it("outside any scope, every pause throws (nothing approved by default)", () => {
    expect(() => pauseForApproval("x", "y")).toThrow(ApprovalRequiredError);
  });

  it("approvals are isolated per scope (no cross-run bleed)", () => {
    client.requestScope(undefined, ["a"], () => {
      expect(() => pauseForApproval("a", "s")).not.toThrow();
    });
    // A sibling scope with a DIFFERENT grant set must not see "a".
    client.requestScope(undefined, ["b"], () => {
      expect(() => pauseForApproval("a", "s")).toThrow(ApprovalRequiredError);
    });
  });
});
