# ctxmesh — Ruby SDK

Typed clients for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
control plane for AI agents.

Your agent runs in a pod beside the platform's sidecars. This gem is the typed way to reach
them — conversation memory, long-term memory, knowledge bases, skills, feedback, agent-to-agent
calls, delegation and handoff.

**Your code never holds credentials.** Endpoints and identity arrive in the environment the
platform injects, so there is no API key to manage and no base URL to configure.

```ruby
gem "ctxmesh"
```

## Use it

```ruby
require "ctxmesh"

cx = Ctxmesh::Client.from_env      # reads MEMORY_PORT, CONVERSATION_ID, …

cx.memory_append(role: "user", content: "What changed in the deploy?")
history = cx.memory_get

cx.remember("The customer prefers email.", { "topic" => "prefs" })
facts = cx.search_agent("contact preference", top_k: 5)

hits = cx.knowledge_search("rollback procedure", top_k: 5)
cx.feedback(trace_id, "helpfulness", 1.0, "clear")
```

`Ctxmesh::Client.from_env` raises `NotInPodError` outside the platform.

## Errors say which kind of "no" it was

- **`NotWiredError`** — the platform did not grant this capability. The port is absent because
  nothing is listening; a configuration answer, not a failure.
- **`DeniedError`** — a 403. The plane understood the call and refused it (a guardrail, a
  budget, the delegate fence), and it carries the reason.
- **`ApiError`** — any other non-2xx, with the body.

## No runtime dependencies

`net/http` and the stdlib `json`. A gem dependency in an SDK becomes a version conflict in every
application that already has it.

## Conformance tier

**plane-client** (ADR 0139) — every launcher route is reachable here. The managed agent loop and
model client are authoring-tier and live in the Python and TypeScript SDKs. Every capability is
also a plain HTTP endpoint, so this gem is convenience, never a requirement.

Apache-2.0.
