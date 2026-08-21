/**
 * L7 supervisor-loop checkpoint (ADR 0091) — the TS SDK side of durable delegate suspend/resume.
 *
 * A depth-0 durable supervisor that delegates and suspends serializes its managed-loop state into an
 * opaque **payload** string (built here) and returns it in the `delegate_waiting` marker. The BFF
 * wraps that payload in a hashed **envelope** (`internal/run/checkpoint.go`, NewSupervisorCheckpoint)
 * and stores it in the run's cursor. On resume the worker injects the ENVELOPE back into the invoke
 * body as `body["checkpoint"]`; `verifyAndExtract` re-verifies it (kind / version / hash — defense in
 * depth; the worker already verified before injecting) and returns the payload's fields.
 *
 * The payload carries ONLY loop state — never authorization (consent/approval/OBO), which is
 * re-derived server-side on resume (ADR 0091 fork 3). Verification is fail-safe: a corrupt /
 * version-skewed envelope or payload yields `null` and the caller runs fresh from the request input.
 *
 * Kept in lockstep with the Python `_checkpoint.py` (same payload shape) and the Go envelope contract.
 */

import { createHash } from "node:crypto";

const ENVELOPE_KIND = "supervisor-loop";
const ENVELOPE_VERSION = 1;

/** The payload's OWN schema version (independent of the envelope). A resume rejects an unknown one. */
export const PAYLOAD_VERSION = 1;

/**
 * Cap on the serialized payload size. The `delegate_waiting` marker rides the /invoke response, which
 * the BFF reads through a 4 MiB LimitReader (internal/bff/invoke.go) — an oversized marker is silently
 * TRUNCATED, so the loop measures the payload and falls back to blocking dispatch above this threshold
 * (a loud, graceful M64 degradation) rather than emitting a truncated, unparseable marker.
 */
export const CHECKPOINT_MAX_BYTES = 1_500_000;

/** A pending delegation recorded in the checkpoint for re-dispatch on resume. */
export interface PendingDelegate {
  call_id: string;
  step: string;
  sub_agent: string;
  task: string;
}

/** The restorable managed-loop state (the opaque payload's fields). */
export interface CheckpointFields {
  v: number;
  messages: Array<Record<string, unknown>>;
  step: number;
  pending: PendingDelegate[];
  tools_called: string[];
  consent_required: string[];
  spotlight_token: string;
  model_index: number;
  tool_index: number;
}

/** Serialize the resumable managed-loop state into the opaque payload string. */
export function buildPayload(fields: Omit<CheckpointFields, "v">): string {
  const payload: CheckpointFields = { v: PAYLOAD_VERSION, ...fields };
  return JSON.stringify(payload);
}

/**
 * Verify a checkpoint ENVELOPE and return its payload fields, or `null` (fail-safe).
 *
 * *envelope* is `body["checkpoint"]` as parsed from the invoke body — the worker injects the Go
 * envelope as a raw JSON object, so it normally arrives as an object (a JSON string is tolerated too).
 * `null` is returned — never thrown — for any envelope that is absent, malformed, of an unknown
 * kind/version, whose payload hash does not match, or whose payload is not a known version.
 */
export function verifyAndExtract(envelope: unknown): CheckpointFields | null {
  if (envelope === null || envelope === undefined) return null;
  let env: unknown = envelope;
  if (typeof env === "string") {
    try {
      env = JSON.parse(env);
    } catch {
      return null;
    }
  }
  if (typeof env !== "object" || env === null || Array.isArray(env)) return null;
  const e = env as Record<string, unknown>;
  if (e["kind"] !== ENVELOPE_KIND || e["version"] !== ENVELOPE_VERSION) return null;
  const payloadStr = e["payload"];
  if (typeof payloadStr !== "string") return null;
  const digest = createHash("sha256").update(payloadStr, "utf8").digest("hex");
  if (digest !== e["sha256"]) return null;
  let fields: unknown;
  try {
    fields = JSON.parse(payloadStr);
  } catch {
    return null;
  }
  if (typeof fields !== "object" || fields === null || Array.isArray(fields)) return null;
  const f = fields as Record<string, unknown>;
  if (f["v"] !== PAYLOAD_VERSION) return null;
  return f as unknown as CheckpointFields;
}
