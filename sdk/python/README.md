# ctxmesh — agentry Python SDK

Optional, typed sugar over the launcher's language-agnostic localhost platform
plane (ADR 0002). Bundled into `base-python`, importable as `ctxmesh`. Never a
hard dependency: every capability it exposes is *also* a raw launcher endpoint.

- **Distribution name:** `ctxmesh` · **import name:** `ctxmesh`
- **Python:** 3.9+ · **runtime deps:** the plane clients are pure stdlib; the
  model + step-tracing helpers add a minimal OTLP/gRPC exporter +
  OpenInference semantic-convention constants, **pinned to the exact versions
  `images/base-python` already bundles** (opentelemetry `1.27.0`,
  openinference-semantic-conventions `0.1.30`) — so `import ctxmesh` adds **zero
  net footprint** to the base image.

## Surface

```python
from ctxmesh import agent

client = agent.from_env()          # in-pod: reads the launcher-injected env

# memory (:2998, M5)
client.memory.get()                       # full context (list)
client.memory.put([{"role": "user", "content": "hi"}])
client.memory.append({"role": "assistant", "content": "hey"})
client.memory.search("hi")
# bind a conversationId for the turn (the agent reads it from the request):
turn = client.with_conversation("conv-42")
turn.memory.get()

# tools / discovery (:2999, M4)
client.tools.list()                       # live manifest as Tool objects
client.tools.call("word-count", text="a b c")   # MCP tools/call

# feedback (:2995, M9)
client.feedback.score("trace-abc", "thumbs-up", 1, comment="great")

# model gateway ($MODEL_GATEWAY_URL, M2/M8) — emits an OpenInference LLM span
resp = client.model.chat("gpt-4o-mini", [{"role": "user", "content": "q"}])
resp.text        # the completion; resp.usage → token counts; resp.raw → full body
```

`agent.from_env()` **fails fast** (`NotInPodError`) when no launcher env is
present — it never silently no-ops. For tests / offline use, build a
`PlaneConfig` explicitly (`PlaneConfig.for_test(...)`) and call
`agent.from_config(config)` against a fake localhost plane.

## Step-tracing helpers (custom loops)

A framework agent (LangChain/OpenAI/Anthropic) gets its `step → tool → model`
trace tree SDK-free via base-image OpenInference auto-instrumentation. A **custom,
no-framework loop** has no inferable step boundaries, so it emits the tree
explicitly with `client.trace.*` — producing an OpenInference tree
**structurally identical** to a framework one (same `CHAIN`/`TOOL`/`LLM` span
kinds + attribute keys), exported over the same OTLP/gRPC path to the collector
(`:4317`) → Langfuse.

```python
# Bind the inbound /invoke request so the WHOLE tree roots under the launcher's
# `agent.invoke` span (the launcher injected a W3C `traceparent`). Without this
# bind the SDK spans would start a detached trace.
with client.trace.request_context(request.headers):
    with client.trace.step("plan") as step:            # CHAIN span
        step.set_input(user_prompt)
        plan = client.model.chat(model, messages)       # nested LLM span (auto)
        with client.trace.tool("web_search", args) as t:  # TOOL span (child of step)
            t.set_output(client.tools.call("web_search", **args))
        step.set_output(plan.text)
```

`client.trace.loop(name, headers=request.headers)` is a convenience that binds
the request context **and** opens the `AGENT` loop-root span in one `with`.

**Rooting under `agent.invoke` is the invariant** — the SDK extracts the W3C
`traceparent` the launcher proxy injects on every `/invoke` and makes the step
spans children of the launcher's `agent.invoke` root (same trace id, correct
parent span id), not a detached trace.

**Offline / telemetry resilience:** when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset
(offline/tests) the trace client runs in **no-op export** mode — spans are still
created (so nesting/propagation still works and can be asserted) but exported
nowhere. Export/setup failures degrade to no-op and **never crash the loop** (a
telemetry blip is not an error); this is deliberately distinct from the plane
clients, which *surface* endpoint errors (a rejected memory write is a real
error).

## Serving an agent (`ctxmesh.serve`)

You don't hand-roll the HTTP server, the `/invoke` body/​envelope, the
`/healthz`/`/readyz` probes, the `$AGENT_PORT` the launcher proxies to, the
`traceparent` capture, or the SSE token stream — `ctxmesh.serve` encodes the whole
runtime contract, and it binds `request_scope` for you (so a custom loop keeps the
invoking user's run capability instead of silently downgrading to org/public creds):

```python
import ctxmesh

def handle(req: ctxmesh.InvokeRequest) -> str:
    # req.client is scoped to the caller (capability + granted approvals bound) and
    # conversation-aware (req.conversation_id); req.headers roots the trace.
    with req.client.trace.loop("my-agent", headers=req.headers):
        answer = req.client.model.chat(
            "gpt-4o-mini", [{"role": "user", "content": req.input}]
        )
    return answer.text          # or return a ctxmesh.ManagedResult for steps/tools/approval

ctxmesh.serve(handle)           # blocks; serves /invoke + health on $AGENT_PORT
```

- **Streaming** is transparent: call `req.emit_token(delta)` as your loop produces
  content — it emits an SSE `token` frame when the caller sent
  `Accept: text/event-stream`, and is a no-op otherwise (same handler, both modes).
- **The stock managed agent** is just `ctxmesh.serve()` with **no** handler — it runs
  the config-driven tool-calling loop (`run_managed_loop`) with `ManagedConfig.from_env()`.
  That is exactly the managed-agent image's entrypoint.
- `serve(handler, *, client=…, agent_name=…, port=…)` overrides the env defaults
  (`agent.from_env()` / `$AGENT_NAME` / `$AGENT_PORT`) — handy for local runs and tests.

`examples/sdk-custom-agent` shows the same loop with the HTTP handler written out by
hand — the "under the hood" reference for what `serve` collapses into one call.

## Dev

The toolchain (ruff + pytest, pinned) is wired into the engine `Makefile`:

```
make py-venv     # create .venv-sdk with pinned ruff+pytest (from host python3)
make lint        # go lint + ruff (sdk/python)
make test        # go unit tests + pytest (sdk/python)
```

Pins live in `requirements-dev.txt` (mirrored in the `dev` extra of
`pyproject.toml`).
