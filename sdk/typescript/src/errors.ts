/**
 * Exception hierarchy for the ctxmesh SDK — parity with `sdk/python/src/ctxmesh/errors.py`.
 *
 * The SDK never swallows a launcher-plane error: a bad configuration, an absent
 * launcher env, or a non-2xx endpoint response all surface as a typed error the
 * caller can catch (spec "Edge cases": endpoint down -> surface, not silent).
 *
 * Each class extends `Error`, sets `name`, and restores the prototype chain via
 * `Object.setPrototypeOf` so `instanceof` works reliably when the code is emitted
 * to an ES target where `Error` subclassing otherwise breaks the prototype (a
 * well-known TS/`extends Error` pitfall).
 */

/** Base class for every error raised by the SDK. */
export class CtxmeshError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CtxmeshError";
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * The SDK was asked to do something its configuration cannot support.
 *
 * Raised, for example, when a client is used but its endpoint was not wired (the
 * launcher did not inject the corresponding env), so there is no address to talk to.
 */
export class ConfigError extends CtxmeshError {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * `agent.fromEnv()` was called outside a launcher pod.
 *
 * The launcher-injected env that identifies the localhost plane is absent, so the
 * SDK cannot know where memory / tools / feedback live. This fails fast with a
 * clear message rather than silently no-oping (spec: "never silently no-ops"). For
 * tests / offline use, build a `PlaneConfig` explicitly or use `agent.fromConfig(...)`.
 */
export class NotInPodError extends ConfigError {
  constructor(message: string) {
    super(message);
    this.name = "NotInPodError";
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * A launcher-plane endpoint returned an error or was unreachable.
 *
 * Carries the HTTP `status` (when there was a response) so the caller can react
 * to, e.g., a feedback 400 (bad request) vs a 502 (upstream/Langfuse down) without
 * string-matching. `status` is `undefined` for transport-level failures
 * (connection refused, timeout) where no HTTP response was received.
 */
export class EndpointError extends CtxmeshError {
  readonly status?: number;
  readonly body?: string;

  constructor(message: string, opts: { status?: number; body?: string } = {}) {
    super(message);
    this.name = "EndpointError";
    this.status = opts.status;
    this.body = opts.body;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * A tool call reached an MCP server the invoking user has not connected an account to.
 *
 * The injecting egress sidecar (ADR 0029 §2) returned a structured `consent_required`:
 * the user must connect their OWN account to `server` before the agent can call the
 * tool on their behalf. The managed loop turns this into a run OUTCOME (a "Connect
 * your account" signal the console renders as a CTA), not a crash.
 */
export class ConsentRequiredError extends EndpointError {
  readonly server: string;

  constructor(
    message: string,
    opts: { server: string; status?: number; body?: string },
  ) {
    super(message, { status: opts.status, body: opts.body });
    this.name = "ConsentRequiredError";
    this.server = opts.server;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * A model call was refused by the launcher's in-path guardrail engine (M66, ADR 0059 §8).
 *
 * The guardrail proxy returned HTTP 403 with `{"error":{"type":"guardrail_blocked",
 * "detector":"…","scan_point":"…"}}`. This is a terminal content-policy decision, not
 * a transient failure: the request was examined and refused on policy grounds, so it
 * MUST NOT be retried. The managed loop surfaces it as an honest run failure rather
 * than crashing or silently succeeding.
 */
export class GuardrailBlockedError extends EndpointError {
  /** The name of the guardrail rule (detector) that triggered the block. */
  readonly detector: string;
  /** Where the block originated — "input", "toolOutput", or "output". */
  readonly scanPoint: string;

  constructor(
    message: string,
    opts: { detector: string; scanPoint: string; status?: number; body?: string },
  ) {
    super(message, { status: opts.status ?? 403, body: opts.body });
    this.name = "GuardrailBlockedError";
    this.detector = opts.detector;
    this.scanPoint = opts.scanPoint;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * A run reached a step gated on human approval (human-in-the-loop, ADR 0034, m32.4).
 *
 * Raised by `pauseForApproval` when the step's `key` has not (yet) been approved.
 * The managed loop turns it into a run OUTCOME — a `requires_action` (approval) the
 * console renders as an approve/deny affordance — not a crash. When the approver
 * resolves it, the re-invoke carries the approved key, so `pauseForApproval` proceeds
 * instead of raising.
 */
export class ApprovalRequiredError extends CtxmeshError {
  readonly key: string;
  readonly summary: string;

  constructor(message: string, opts: { key: string; summary: string }) {
    super(message);
    this.name = "ApprovalRequiredError";
    this.key = opts.key;
    this.summary = opts.summary;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * A root supervisor delegated and chose to SUSPEND rather than block (L7, ADR 0091).
 *
 * Raised inside the managed loop when a depth-0 durable supervisor issues `delegate_to` calls:
 * instead of blocking for the sub-agents' lifetimes, the loop serializes its state (`checkpoint` —
 * an opaque loop-state payload) and records the delegations (`delegates`). `runManagedLoop` turns
 * it into a `delegate_waiting` OUTCOME the BFF worker enacts as one durable suspend transaction
 * (child-create + parent→waiting). On resume the children's results are re-dispatched through the
 * idempotent blocking delegate path and the loop continues from where it paused.
 */
export class DelegateWaitingError extends CtxmeshError {
  /** The opaque loop-state payload (a JSON string) the BFF wraps in a hashed envelope. */
  readonly checkpoint: string;
  /** The delegations to enact: `[{sub_agent, endpoint, input, step, call_id}]`. */
  readonly delegates: Array<Record<string, string>>;
  /** The loop step at which the suspend occurred. */
  readonly steps: number;

  constructor(
    message: string,
    opts: { checkpoint: string; delegates: Array<Record<string, string>>; steps: number },
  ) {
    super(message);
    this.name = "DelegateWaitingError";
    this.checkpoint = opts.checkpoint;
    this.delegates = opts.delegates;
    this.steps = opts.steps;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}
