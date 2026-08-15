/**
 * Request-scoped run-capability stub (parity with `sdk/python/src/ctxmesh/_capability.py`).
 *
 * The BFF mints a run capability on every `/invoke` and the launcher passes it through on
 * inbound agent-request headers as `X-Ctxmesh-Run-Capability`. The SDK must relay it on
 * every outbound MCP tool call, model call, and long-term memory call so the sidecar can
 * resolve the invoking user's OBO credential (ADR 0030 §3).
 *
 * M77.2 stub: `currentCapability()` always returns `undefined` here. M77.4 wires the real
 * AsyncLocalStorage-based request-scoped binding (analogous to the Python ContextVar) —
 * until then, every client relays the header ONLY when this function returns a value.
 * The plumbing (capability relay in every client) is present and tested with an injected
 * value so M77.4 can plug in with zero changes to the data-plane clients.
 */

/** The header name the BFF mints and every outbound egress relays. */
export const CAPABILITY_HEADER = "X-Ctxmesh-Run-Capability";

/**
 * Return the run capability bound to the current request context, or `undefined`.
 *
 * M77.2 STUB — always returns `undefined`. M77.4 replaces this with an
 * AsyncLocalStorage read so in-flight requests are isolated from each other
 * (no cross-user bleed, the same guarantee the Python ContextVar gives).
 */
export function currentCapability(): string | undefined {
  return undefined;
}
