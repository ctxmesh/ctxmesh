/**
 * Request-scoped propagation of the record-mode capture toggle (M78, ADR 0071 §1).
 * Parity with `sdk/python/src/ctxmesh/_record.py`.
 *
 * When a run opts into RECORD mode, the BFF stamps `X-Ctxmesh-Record: <runId>` on the agent's
 * inbound `/invoke` (only for a recorded run against a record-capable agent). The SDK must relay it
 * on every outbound MODEL call so the launcher gateway captures that call's request+response into
 * the run's portable replay fixture (internal/replay on the Go side). It is the exact sibling of the
 * run-capability relay (`_capability.ts`): a launcher-internal, request-scoped header the SDK
 * forwards but never originates.
 *
 * Held in `AsyncLocalStorage` (the Node analogue of the Python ContextVar): `recordScope(headers,
 * fn)` binds it for the request's async life and `currentRecordRunId()` only READS the current
 * async context. No process-wide setter — a non-recorded run resolves `undefined` ⇒ the model
 * client relays no header ⇒ the gateway captures nothing for that run.
 */

import { AsyncLocalStorage } from "node:async_hooks";

/**
 * The header the BFF stamps and the launcher gateway reads — MUST match `hdrRecord` on the Go side
 * (internal/bff/invoke.go) and `recordHeaderName` in the launcher gateway (cmd/launcher). Its value
 * is the run id the fixture is keyed on. Case-insensitive on the wire.
 */
export const RECORD_HEADER = "X-Ctxmesh-Record";

/**
 * The request-scoped record-toggle store. No default value ⇒ a non-recorded run: the model client
 * relays no `X-Ctxmesh-Record` header and the gateway captures nothing.
 */
const store = new AsyncLocalStorage<string | undefined>();

/**
 * Return the record-mode run id bound to the CURRENT request context, or `undefined` when the run
 * is not being recorded. Outside any `recordScope` — or in a non-recorded run — this is `undefined`.
 */
export function currentRecordRunId(): string | undefined {
  return store.getStore();
}

/**
 * Pull the record run id out of inbound *headers* case-insensitively (HTTP header case is not
 * guaranteed), returning `undefined` when absent or blank.
 */
function extract(headers?: Record<string, string> | Headers): string | undefined {
  if (!headers) return undefined;
  const target = RECORD_HEADER.toLowerCase();
  if (typeof (headers as Headers).get === "function") {
    const value = (headers as Headers).get(RECORD_HEADER);
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
 * Bind the record-mode run id extracted from inbound *headers* for the duration of `fn` (and
 * everything it awaits), then unbind it. Request-scoped over AsyncLocalStorage — a reused event-loop
 * turn can never observe a prior request's record toggle. A missing/blank header binds `undefined`
 * (a non-recorded run). Returns `fn`'s result.
 */
export function recordScope<T>(
  headers: Record<string, string> | Headers | undefined,
  fn: () => T,
): T {
  return store.run(extract(headers), fn);
}
