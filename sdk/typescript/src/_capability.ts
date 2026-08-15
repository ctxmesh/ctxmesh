/**
 * Request-scoped propagation of the run capability (ADR 0030 §3).
 * Parity with `sdk/python/src/ctxmesh/_capability.py`.
 *
 * The run capability is the unforgeable token the BFF mints at `/invoke` identifying the
 * INVOKING user (its `sub` is that user's hashed identity). It arrives on the inbound
 * request headers, and the SDK must relay it on every outbound MCP tool call, model call,
 * and long-term memory call so the egress sidecar can resolve *that* user's credential —
 * never another user's.
 *
 * The Python SDK holds it in a `contextvars.ContextVar` (PEP 567): each inbound request
 * runs in its own execution context, so concurrent requests for different users never
 * observe each other's capability. The Node analogue is `AsyncLocalStorage` (node:async_hooks):
 * `capabilityScope(headers, fn)` runs `fn` (and everything it `await`s) inside a store bound
 * to that request's capability, and `currentCapability()` only ever READS the store bound to
 * the current async context. There is deliberately **no process-wide setter** — the only way
 * to bind a capability is `capabilityScope`, which is request-scoped and unbinds on exit.
 * This structural absence of a global accessor is the no-cross-bleed guarantee (a
 * compromised/injected agent cannot reach out and set another request's capability).
 */

import { AsyncLocalStorage } from "node:async_hooks";

/**
 * The header the BFF mints into and the launcher passes through — MUST match
 * `runcap.HeaderName` on the Go side (internal/runcap). Case-insensitive on the wire.
 */
export const CAPABILITY_HEADER = "X-Ctxmesh-Run-Capability";

/**
 * The request-scoped capability store. No default value ⇒ a run with no capability
 * (unattended / minting-disabled) resolves as org/public only, never another user's grant.
 */
const store = new AsyncLocalStorage<string | undefined>();

/**
 * Return the run capability bound to the CURRENT request context, or `undefined`.
 *
 * Reads the AsyncLocalStorage store for the current async context (the Node analogue of
 * the Python ContextVar read). Outside any `capabilityScope` — or in a run with no
 * capability header — this is `undefined`, so the relaying clients send no capability.
 */
export function currentCapability(): string | undefined {
  return store.getStore();
}

/**
 * Pull the capability out of inbound *headers* case-insensitively (HTTP header case is not
 * guaranteed), returning `undefined` when absent or blank.
 */
function extract(headers?: Record<string, string> | Headers): string | undefined {
  if (!headers) return undefined;
  const target = CAPABILITY_HEADER.toLowerCase();
  // A WHATWG `Headers` normalises case and exposes `.get`.
  if (typeof (headers as Headers).get === "function") {
    const value = (headers as Headers).get(CAPABILITY_HEADER);
    const stripped = (value ?? "").trim();
    return stripped || undefined;
  }
  for (const [key, value] of Object.entries(headers as Record<string, string>)) {
    if (key.toLowerCase() === target) {
      const stripped = (value ?? "").trim();
      return stripped || undefined;
    }
  }
  return undefined;
}

/**
 * Bind the run capability extracted from inbound *headers* for the duration of `fn`
 * (and everything it awaits), then unbind it.
 *
 * Request-scoped: the store is bound only inside the `fn` callback, so even when the event
 * loop later runs another request's continuation it can never observe this request's
 * capability. A missing/blank header binds `undefined` (an unattended run). Returns `fn`'s
 * result (async-aware — `AsyncLocalStorage.run` preserves the store across `await`).
 */
export function capabilityScope<T>(
  headers: Record<string, string> | Headers | undefined,
  fn: () => T,
): T {
  return store.run(extract(headers), fn);
}
