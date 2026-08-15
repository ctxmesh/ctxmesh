/**
 * Request-scoped human-in-the-loop approvals (ADR 0034 §HITL, m32.4).
 * Parity with `sdk/python/src/ctxmesh/_approval.py`.
 *
 * A run can gate a sensitive step on human approval. The agent calls
 * `pauseForApproval(key, summary)`; if `key` is in the set of approvals GRANTED for this
 * run, the call returns and the step proceeds — otherwise it throws
 * `ApprovalRequiredError`, which the managed loop turns into a `requires_action`
 * (approval) outcome. When the approver resolves it, the run is re-invoked with the
 * approved key in the granted set, so the same `pauseForApproval` call now proceeds.
 *
 * The granted set is held in an `AsyncLocalStorage` (node:async_hooks), the Node analogue
 * of the Python `contextvars.ContextVar` — the same no-cross-bleed guarantee as the run
 * capability (`_capability.ts`): each inbound request runs in its own async context, so
 * concurrent runs never observe each other's approvals. The only way to bind approvals is
 * `approvalScope`, which unbinds on exit; there is no process-wide setter.
 */

import { AsyncLocalStorage } from "node:async_hooks";

import { ApprovalRequiredError } from "./errors.js";

/**
 * The approvals granted for the CURRENT run (a set of step keys). No store bound ⇒ nothing
 * approved yet, so every `pauseForApproval` throws.
 */
const store = new AsyncLocalStorage<ReadonlySet<string>>();

/**
 * The granted approval keys for the current async context (an empty set when unbound).
 * Internal — `pauseForApproval` consults it; callers bind via `approvalScope`.
 */
function granted(): ReadonlySet<string> {
  return store.getStore() ?? EMPTY;
}

const EMPTY: ReadonlySet<string> = new Set<string>();

/**
 * Bind the set of GRANTED approval keys for the duration of `fn` (and everything it
 * awaits), then unbind it.
 *
 * Request-scoped: bound only inside the `fn` callback, so a later continuation of another
 * run can never observe this run's approvals. `undefined` binds the empty set (nothing
 * approved). Returns `fn`'s result (async-aware).
 */
export function approvalScope<T>(
  approvals: Iterable<string> | undefined,
  fn: () => T,
): T {
  return store.run(new Set(approvals ?? []), fn);
}

/**
 * Gate the current step on human approval (human-in-the-loop, m32.4).
 *
 * If `key` has been approved for this run (the approver resolved a prior pause), this
 * returns and the step proceeds. Otherwise it throws `ApprovalRequiredError`, which the
 * managed loop surfaces as a `requires_action` (approval) outcome carrying `key` +
 * `summary` (a human-readable description of what needs approving). The run resumes on
 * approval.
 *
 * `key` is a STABLE identifier for this decision point (e.g. `"send-email"`) — the same
 * key must be used across the initial call and the resumed re-invoke so the approval
 * matches.
 */
export function pauseForApproval(key: string, summary: string): void {
  if (granted().has(key)) return;
  throw new ApprovalRequiredError(
    `approval required for ${JSON.stringify(key)}: ${summary}`,
    { key, summary },
  );
}

// ── The stateless approval VOUCHER (ADR 0074 §3, m82.4) ──────────────────────────────────────────
//
// require-approval is enforced at the egress WIRE, not just here in the loop: a tool call for a
// require-approval tool is FORWARDED by the sidecar only when the request carries a valid, signed
// `X-Ctxmesh-Approval` voucher (a short-lived token bound to {runId, toolName} the BFF minted on a
// human's approval). `pauseForApproval` is now the PRESENTATION UX — necessary but no longer
// sufficient — and the SDK must RELAY the voucher on the tool-call retry so the sidecar forwards it.
//
// The voucher arrives on the RESUMED run's inbound `/invoke` headers (the BFF stamped
// `X-Ctxmesh-Approval` after approval-grant). We hold it request-scoped in `AsyncLocalStorage` and
// relay it on every outbound tool call — the exact sibling of the run-capability relay
// (`_capability.ts`) and the record toggle (`_record.ts`). The sidecar's {tool, run} binding means a
// voucher only unlocks the ONE approved tool; relaying it on every call is safe.

/**
 * The header the BFF stamps on a resumed run and the SDK relays on each tool-call egress — MUST match
 * `runcap.ApprovalHeaderName` on the Go side (internal/runcap) + `hdrApproval` (internal/bff).
 * Case-insensitive on the wire.
 */
export const APPROVAL_HEADER = "X-Ctxmesh-Approval";

/**
 * The request-scoped approval-voucher store. No value bound ⇒ a run with no granted require-approval
 * tool: the tool client relays no voucher and a require-approval tool gets the sidecar's 403
 * `approval_required`.
 */
const voucherStore = new AsyncLocalStorage<string | undefined>();

/**
 * Return the approval voucher bound to the CURRENT request context, or `undefined`. The tool client
 * relays it on each outbound MCP tool call; `undefined` outside a resumed/approved run.
 */
export function currentApprovalVoucher(): string | undefined {
  return voucherStore.getStore();
}

/**
 * Pull the approval voucher out of inbound *headers* case-insensitively (HTTP header case is not
 * guaranteed), returning `undefined` when absent or blank.
 */
function extractVoucher(headers?: Record<string, string> | Headers): string | undefined {
  if (!headers) return undefined;
  const target = APPROVAL_HEADER.toLowerCase();
  if (typeof (headers as Headers).get === "function") {
    const value = (headers as Headers).get(APPROVAL_HEADER);
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
 * Bind the approval voucher extracted from inbound *headers* for the duration of `fn` (and everything
 * it awaits), then unbind it. Request-scoped over AsyncLocalStorage — a reused event-loop turn can
 * never observe a prior request's voucher. A missing/blank header binds `undefined` (no granted
 * require-approval tool). Returns `fn`'s result.
 */
export function voucherScope<T>(
  headers: Record<string, string> | Headers | undefined,
  fn: () => T,
): T {
  return voucherStore.run(extractVoucher(headers), fn);
}
