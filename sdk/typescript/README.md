# ctxmesh — TypeScript SDK

Typed clients for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
control plane for AI agents.

Your agent runs in a pod beside the platform's sidecars. This SDK is the typed way to reach
them — conversation memory, tools and MCP servers, knowledge bases, model calls, and feedback —
plus OpenTelemetry tracing that produces the same trace tree a framework agent gets.

**Your code never holds credentials.** Endpoints and identity arrive in the environment the
platform injects, so there is no API key to manage and no base URL to configure.

```bash
npm install ctxmesh@beta
```

> Use the `beta` tag. Plain `npm install ctxmesh` currently resolves to the older
> `0.1.0-beta.1`; that pin lifts with the first stable release.

## Use it

```ts
import { agent } from "ctxmesh";

const client = agent.fromEnv();   // reads MODEL_GATEWAY_URL, MEMORY_PORT, AGENT_NAME, …

// Conversation memory, keyed by conversation id
await client.memory.append({ role: "user", content: "What changed in the deploy?" }, cid);
const history = await client.memory.get(cid);

// Long-term memory for this agent
await client.memory.remember("The customer prefers email.", { topic: "prefs" });
const facts = await client.memory.searchAgent("contact preference", 5, 0.0);

// Tools — everything the platform granted, including MCP servers
const tools = await client.tools.list();
const result = await client.tools.call("search_web", { query: "ctxmesh" });

// Feedback — the signal that drives evals and canary promotion
await client.feedback.score(traceId, "helpfulness", 1.0, "clear");
```

`agent.fromEnv()` throws `NotInPodError` outside the platform. For tests and offline work,
`agent.fromConfig(config)` builds a client from an explicit `PlaneConfig`.

## Testing without a cluster

`ctxmesh/testing` ships offline stubs for every plane, so unit tests need no cluster and no
network.

## Is the SDK required?

No, and that is deliberate. Every capability here is also a plain HTTP endpoint the platform
serves, so an agent in any language can call the contract directly. This SDK is the typed,
ergonomic path over it — never a dependency the platform imposes.

## Documentation

- [TypeScript SDK guide](https://ctxmesh.github.io/sdk/typescript/) — every client, with examples
- [Compatibility](https://ctxmesh.github.io/reference/compatibility/) — which SDK works with which ctxmesh
- [Source and issues](https://github.com/ctxmesh/ctxmesh)

Apache-2.0. Contributor and toolchain notes live in
[the repository](https://github.com/ctxmesh/ctxmesh/tree/main/sdk/typescript).
