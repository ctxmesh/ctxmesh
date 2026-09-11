# ctxmesh — Java SDK

Typed clients for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
control plane for AI agents.

Your agent runs in a pod beside the platform's sidecars. This SDK is the typed way to reach
them — conversation memory, long-term memory, tools, knowledge bases, feedback, agent-to-agent
calls, delegation and handoff.

**Your code never holds credentials.** Endpoints and identity arrive in the environment the
platform injects, so there is no API key to manage and no base URL to configure.

```xml
<dependency>
  <groupId>ai.ctxmesh</groupId>
  <artifactId>ctxmesh</artifactId>
  <version>0.1.0-beta.1</version>
</dependency>
```

## Use it

```java
import ai.ctxmesh.Client;

Client cx = Client.fromEnv();   // reads MEMORY_PORT, AGENT_NAME, CONVERSATION_ID, …

cx.memory.append(new Client.Entry("user", "What changed in the deploy?"), null);
var history = cx.memory.get(null);

cx.memory.remember("The customer prefers email.", Map.of("topic", "prefs"));
var facts = cx.memory.searchAgent("contact preference", 5, 0.0);

var hits = cx.knowledge.search("rollback procedure", null, 5);
var skills = cx.skills.list();
cx.feedback.score(traceId, "helpfulness", 1.0, "clear");
```

`Client.fromEnv()` throws `NotInPodException` outside the platform. `Client.fromConfig(...)`
builds one explicitly for tests.

## Errors say which kind of "no" it was

- **`NotWiredException`** — the platform did not grant this capability. The port is absent
  because nothing is listening; this is a configuration answer, not a failure.
- **`DeniedException`** — a 403. The plane understood the call and refused it (a guardrail, a
  budget, the delegate fence), and it carries the reason.
- **`ApiException`** — any other non-2xx, with the body.

## No runtime dependencies

`java.net.http` plus a small internal JSON reader. An SDK that depends on Jackson becomes a
version conflict in every application that already has one.

## Conformance tier

**plane-client** — every launcher route is
reachable here. The managed agent loop, tool dispatch and model client are authoring-tier and
live in the Python and TypeScript SDKs. Every capability is also a plain HTTP endpoint, so this
SDK is convenience, never a requirement.

Apache-2.0.
