/**
 * ts-custom-agent — no-framework loop using the ctxmesh TS SDK (M77 example).
 *
 * The TypeScript analogue of `examples/sdk-custom-agent/agent.py`: a hand-written loop with NO
 * LangChain / LlamaIndex / OpenAI SDK. The trace tree (AGENT → step/CHAIN → tool/TOOL → model/LLM)
 * is emitted *explicitly* via `client.trace.*` rather than by auto-instrumentation — that is the
 * entire point of the SDK's step-tracing helpers.
 *
 * Trace structure per /invoke:
 *
 *     agent.invoke                 (launcher — the boundary span)
 *     └─ ts-custom-agent (AGENT)     ← client.trace.loop — rooted under agent.invoke
 *        ├─ plan (CHAIN)             ← client.trace.step
 *        │  └─ chat gpt-4o-mini (LLM)   ← client.model.chat wrapped in trace.llm
 *        ├─ word-count (TOOL)        ← client.trace.tool
 *        │  └─ (MCP round-trip via client.tools.call)
 *        └─ answer (CHAIN)           ← client.trace.step
 *           └─ chat gpt-4o-mini (LLM)   ← client.model.chat wrapped in trace.llm
 *
 * `serve(handler)` binds `Client.requestScope` (capability + approvals — the DX-2 mandate) and
 * roots the trace under the launcher's `agent.invoke` span before calling `handle`, so the whole
 * step→tool→model tree nests correctly. The handler is pure business logic — no HTTP plumbing.
 *
 * Runtime contract (mirrors the Python sample, served by `ctxmesh.serve`):
 *   - POST /invoke   — body: {"input": "<prompt>"} → the serve envelope with `output`
 *   - GET  /healthz  — 200 ok
 *   - GET  /readyz   — 200 ok
 *
 * In the base-node image this file is bundled into a derived agent image whose
 * `AGENT_ENTRYPOINT` is `node /app/agent.js`; the launcher spawns it, owns `$AGENT_PORT`, and
 * reverse-proxies to the upstream port. Here it imports the vendored `ctxmesh` package; the test
 * (`test/ts-custom-agent.test.ts`) imports `handle` directly and drives it against the mock plane.
 */

import type { Client, InvokeRequest } from "../../src/index.js";
import { serve } from "../../src/index.js";

/** The catalog name of the word-count tool (mirrors the Python sample + the mock DiscoveryStub). */
const WORD_COUNT_TOOL = "word-count";

/** A concise, deterministic system prompt (keeps the mock gateway reproducible). */
const SYSTEM_PROMPT = "You are a concise assistant. Reply in one sentence.";

/** The gateway route alias — matches the ToolRegistry / LiteLLM route config. */
function modelRoute(): string {
  return process.env["MODEL_ROUTE"] ?? "gpt-4o-mini";
}

/** The result of one no-framework turn: the final answer + the word count the tool produced. */
export interface AgentResult {
  output: string;
  wordCount: number;
}

/**
 * The no-framework loop, factored out so the test can drive it directly.
 *
 * `serve` has already entered `requestScope` (capability + trace context) around this call, so the
 * `client.trace.*` spans opened here root under the launcher's `agent.invoke` span. The loop:
 *
 *   1. **plan step (CHAIN):** ask the model for a one-sentence summary; the model call is wrapped
 *      in `trace.llm` so it appears as an LLM child span.
 *   2. **word-count tool (TOOL):** invoke the word-count MCP tool via `client.tools.call`; the
 *      SDK emits the TOOL span, the MCP round-trip is handled by the ToolsClient.
 *   3. **answer step (CHAIN):** synthesise the final answer from the plan + the word count.
 */
export async function runLoop(client: Client, input: string, req: InvokeRequest): Promise<AgentResult> {
  return client.trace.loop("ts-custom-agent", req.headers, async (root) => {
    root.setInput(input);

    // ── step 1: plan ──────────────────────────────────────────────────────────
    const plan = await client.trace.step("plan", async (planStep) => {
      planStep.setInput(input);
      const messages = [
        { role: "system", content: SYSTEM_PROMPT },
        { role: "user", content: `Summarise in one sentence: ${input}` },
      ];
      const resp = await client.trace.llm("chat plan", modelRoute(), messages, () =>
        client.model.chat(modelRoute(), messages),
      );
      planStep.setOutput(resp.text);
      return resp.text;
    });

    // ── step 2: word-count tool call ──────────────────────────────────────────
    const wordCount = await client.trace.tool(
      WORD_COUNT_TOOL,
      { text: input },
      async (toolSpan) => {
        const result = await client.tools.call(WORD_COUNT_TOOL, { text: input });
        const count =
          typeof result === "object" && result !== null && typeof (result as Record<string, unknown>)["count"] === "number"
            ? ((result as Record<string, unknown>)["count"] as number)
            : input.split(/\s+/).filter(Boolean).length;
        toolSpan.setOutput({ wordCount: count });
        return count;
      },
    );

    // ── step 3: final answer ──────────────────────────────────────────────────
    const answer = await client.trace.step("answer", async (answerStep) => {
      const messages = [
        { role: "system", content: SYSTEM_PROMPT },
        {
          role: "user",
          content: `User said: ${JSON.stringify(input)} (${wordCount} words). Plan summary: ${JSON.stringify(plan)}. Reply in one sentence.`,
        },
      ];
      const resp = await client.trace.llm("chat answer", modelRoute(), messages, () =>
        client.model.chat(modelRoute(), messages),
      );
      answerStep.setInput(messages[messages.length - 1]!.content);
      answerStep.setOutput(resp.text);
      return resp.text;
    });

    root.setOutput(answer);
    return { output: answer, wordCount };
  });
}

/**
 * The `serve` handler: pure business logic over the parsed request.
 *
 * `serve` binds `req.client` (capability + approvals + trace context) before this runs. Returning
 * a bare string yields the `{agent, output, ...}` envelope. Exported so the test drives it directly.
 */
export async function handle(req: InvokeRequest): Promise<string> {
  const { output } = await runLoop(req.client!, req.input, req);
  return output;
}

/** Serve the agent over the launcher runtime contract (client + name + port from the env). */
export function main(): void {
  serve(handle);
}

// Run only when invoked as the entrypoint (not when imported by the test).
// import.meta.url vs process.argv[1] is the standard ESM "is-main" check.
if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
