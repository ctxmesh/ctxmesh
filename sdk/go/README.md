# ctxmesh — Go SDK

Typed clients for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
control plane for AI agents.

Your agent runs in a pod beside the platform's sidecars. This package is the typed way to reach
them — conversation memory, long-term memory, knowledge bases, skills, feedback, agent-to-agent
calls, delegation and handoff.

**Your code never holds credentials.** Endpoints and identity arrive in the environment the
platform injects, so there is no API key to manage and no base URL to configure.

```sh
go get github.com/ctxmesh/ctxmesh/sdk/go
```

## Use it

```go
c, err := ctxmesh.New()          // reads MEMORY_PORT, CONVERSATION_ID, DELEGATE_PORT, …
if err != nil {
    log.Fatal(err)               // ErrNotInPod outside the platform
}

_ = c.Memory.Append(ctx, ctxmesh.Entry{Role: "user", Content: "what changed?"}, "")
history, _ := c.Memory.Get(ctx, "")

facts, _ := c.Memory.SearchAgent(ctx, "contact preference", 5, 0)
hits, _ := c.Knowledge.Search(ctx, "rollback procedure", "runbooks", 5)
_ = c.Feedback.Score(ctx, traceID, "helpfulness", 1.0, "clear")
```

`ctxmesh.New()` returns `ErrNotInPod` outside the platform. `NewWithConfig` builds one explicitly
for tests.

## Errors say which kind of "no" it was

- **`ErrNotWired`** — the platform did not grant this capability. The port is absent because
  nothing is listening; a configuration answer, not a failure.
- **`ErrDenied`** — a 403. The plane understood the call and refused it (a guardrail, a budget),
  and the reason survives.
- **`APIError`** — any other non-2xx, with the body.

## Delegation needs a run capability

`Delegate` and `Handoff` take the run capability the platform issues, plus `step` and `callId` —
the idempotency key, so a reclaimed supervisor resolves to the same sub-run rather than spawning a
second one. Both return the platform's own answer: it replies `200` for every outcome and signals
success in `OK`, so check that rather than the error.

## Its own module

`sdk/go` is a separate Go module with **no dependencies** — inside the main module, `go get` would
pull the operator's entire Kubernetes tree into an agent that wants an HTTP client.

## Conformance tier

**plane-client** — every launcher route is reachable here. The managed agent loop and model client
live in the Python and TypeScript SDKs. Every capability is also a plain HTTP endpoint, so this
package is convenience, never a requirement.

## Documentation

- [Go SDK guide](https://ctxmesh.github.io/sdk/go/) — the full surface, with examples
- [SDK overview](https://ctxmesh.github.io/sdk/) — every language, and what each tier gives you
- [Compatibility](https://ctxmesh.github.io/reference/compatibility/) — which SDK works with which ctxmesh
- [Source and issues](https://github.com/ctxmesh/ctxmesh)

Apache-2.0. Contributor and toolchain notes live in
[the repository](https://github.com/ctxmesh/ctxmesh/tree/main/sdk/go).
