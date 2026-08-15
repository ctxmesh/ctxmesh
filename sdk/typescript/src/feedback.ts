/**
 * Feedback client — the launcher's :2995 feedback-ingest hook (M9).
 * Parity with `sdk/python/src/ctxmesh/feedback.py`.
 *
 * Wire contract (eval-prompts-feedback.md §3, cmd/launcher/feedback.go):
 *
 *     POST /feedback  { "traceId": <id>, "name": <score-name>,
 *                       "value": <number>, "comment": <optional> }
 *                     -> 202 Accepted     (relayed to Langfuse)
 *                     -> 400              (missing traceId / malformed body)
 *                     -> 502              (Langfuse relay error)
 *
 * A 400 (bad request) and a 502 (upstream/Langfuse down) both surface as an
 * EndpointError carrying the status — the SDK never swallows a rejected or failed score.
 */

import { PlaneConfig } from "./config.js";
import { ConfigError, EndpointError } from "./errors.js";

/** Attach a numeric score to a trace via the launcher's :2995 hook. */
export class FeedbackClient {
  private readonly config: PlaneConfig;

  constructor(config: PlaneConfig) {
    this.config = config;
  }

  /**
   * POST a score for `traceId`; returns on 202, throws otherwise.
   *
   * Raises ConfigError when feedback is unwired or for arguments the endpoint would
   * 400 on (empty traceId, non-numeric value). Raises EndpointError (with `.status`)
   * for a server-side 400/502.
   */
  async score(
    traceId: string,
    name: string,
    value: number,
    comment?: string,
  ): Promise<void> {
    if (!this.config.feedbackWired) {
      throw new ConfigError(
        "feedback is not wired for this agent: the launcher did not " +
          "inject FEEDBACK_PORT/LANGFUSE_HOST. Wire feedback (LANGFUSE_HOST) " +
          "to use client.feedback.score(...)",
      );
    }
    if (!traceId) {
      throw new ConfigError("feedback.score requires a non-empty traceId");
    }
    if (typeof value !== "number" || !isFinite(value)) {
      throw new ConfigError(
        `feedback score value must be a finite number, got ${typeof value}`,
      );
    }

    const payload: Record<string, unknown> = { traceId, name, value };
    if (comment !== undefined) {
      payload.comment = comment;
    }

    const url = `${this.config.feedbackBaseUrl}/feedback`;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10_000);
    let resp: Response;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        signal: controller.signal,
      });
    } catch (err) {
      throw new EndpointError(
        `feedback request failed: ${(err as Error).message}`,
      );
    } finally {
      clearTimeout(timer);
    }

    if (resp.status !== 202) {
      const body = await resp.text().catch(() => "");
      throw new EndpointError(
        `feedback endpoint returned ${resp.status}: ${body.slice(0, 200)}`,
        { status: resp.status, body },
      );
    }
  }
}
