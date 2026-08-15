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
