# ctxmesh — agent-engine Python SDK

Optional, typed sugar over the launcher's language-agnostic localhost platform
plane (ADR 0002). Bundled into `base-python`, importable as `ctxmesh`. Never a
hard dependency: every capability it exposes is *also* a raw launcher endpoint.

- **Distribution name:** `ctxmesh` · **import name:** `ctxmesh`
- **Python:** 3.9+ · **runtime deps:** none (the m10.2 clients are pure stdlib)

## Surface (m10.2)

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
```

`agent.from_env()` **fails fast** (`NotInPodError`) when no launcher env is
present — it never silently no-ops. For tests / offline use, build a
`PlaneConfig` explicitly (`PlaneConfig.for_test(...)`) and call
`agent.from_config(config)` against a fake localhost plane.

`model` and the `trace.*` step-tracing helpers are **m10.3** and are not in this
package yet.

## Dev

The toolchain (ruff + pytest, pinned) is wired into the engine `Makefile`:

```
make py-venv     # create .venv-sdk with pinned ruff+pytest (from host python3)
make lint        # go lint + ruff (sdk/python)
make test        # go unit tests + pytest (sdk/python)
```

Pins live in `requirements-dev.txt` (mirrored in the `dev` extra of
`pyproject.toml`).
