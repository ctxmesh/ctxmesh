# ctxmesh — Rust SDK

Typed clients for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
control plane for AI agents.

Your agent runs in a pod beside the platform's sidecars. This crate is the typed way to reach
them — conversation memory, long-term memory, knowledge bases, skills, feedback, agent-to-agent
calls, delegation and handoff.

**Your code never holds credentials.** Endpoints and identity arrive in the environment the
platform injects, so there is no API key to manage and no base URL to configure.

```toml
[dependencies]
ctxmesh = "0.1.0-beta.1"
```

## Use it

```rust
use ctxmesh::{Client, Entry};

let cx = Client::from_env()?;               // reads MEMORY_PORT, CONVERSATION_ID, …

cx.memory_append(&Entry { role: "user".into(), content: "what changed?".into() }, None)?;
let history = cx.memory_get(None)?;

let facts = cx.search_agent("contact preference", 5, 0.0)?;
let hits = cx.knowledge_search("rollback procedure", None, 5)?;
cx.feedback(&trace_id, "helpfulness", 1.0, Some("clear"))?;
```

`Client::from_env()` returns `Error::NotInPod` outside the platform. `Config::for_test(..)`
builds one explicitly.

## Errors say which kind of "no" it was

- **`Error::NotWired`** — the platform did not grant this capability. The port is absent because
  nothing is listening; a configuration answer, not a failure.
- **`Error::Denied`** — a 403. The plane understood the call and refused it (a guardrail, a
  budget, the delegate fence), and it carries the reason.
- **`Error::Api` / `Error::Transport` / `Error::Decode`** — everything else, kept apart so a
  caller can tell a refusal from an outage.

## Blocking, and no async runtime

Built on `ureq`, not `reqwest`. This is an SDK: it is installed beside an application that has
its own runtime and its own opinion about which one. A client that drags in tokio makes that
choice for them.

## Conformance tier

**plane-client** — every launcher route is reachable here. The managed agent loop and
model client are authoring-tier and live in the Python and TypeScript SDKs. Every capability is
also a plain HTTP endpoint, so this crate is convenience, never a requirement.

Apache-2.0.
