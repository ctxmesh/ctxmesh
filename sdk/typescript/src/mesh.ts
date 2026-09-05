/**
 * `client.mesh` — synchronous agent-to-agent calls through the launcher (M6, M156).
 *
 * WHY THIS IS CALLED `mesh`
 * --------------------------
 * The call surface is AMP (ADR 0138) and the boundary it runs inside is the MESH — the closed
 * communication scope of an AgentRegistry. `client.mesh` is operations over that boundary,
 * which is why the API keeps this name while the protocol got its own.
 *
 * AMP is not Google's Agent2Agent, which the platform's old name (A2A, since M6) collided
 * with. Theirs is an INTEROP protocol for agents from different organisations — Agent Cards,
 * JSON-RPC, a task lifecycle, declared security schemes. Ours is MEDIATION for agents the
 * platform already owns: the launcher stamps an envelope carrying hop depth, the traversal
 * path and a spend budget, so it can enforce registry isolation, cycle detection and fan-out
 * limits. Neither is a worse version of the other; they solve different problems.
 *
 * WHAT THE LAUNCHER DOES FOR YOU
 * ------------------------------
 * Your payload is opaque. The launcher stamps the envelope, resolves the target over DNS,
 * injects W3C `traceparent` so the callee's spans join YOUR trace, and enforces registry
 * isolation plus the callee's `allowedCallers` before the request reaches it. A blocked,
 * unknown or cross-registry target fails fast and typed — it never hangs.
 *
 * Calling a peer's URL directly instead of using this loses all of it: the envelope, the
 * access control, and the joined trace. It is not an error, which is what makes it
 * dangerous.
 */

import { PlaneConfig } from "./config.js";
import { ConfigError, EndpointError } from "./errors.js";

/** Bound on a target agent name, matching the launcher's own validation (a DNS label). */
const MAX_TARGET = 253;

/** Agent-to-agent calls, mediated by this agent's own launcher. */
export class MeshClient {
  private readonly config: PlaneConfig;

  constructor(config: PlaneConfig) {
    this.config = config;
  }

  private requireWired(): void {
    // The launcher starts the :2997 listener ONLY for a resolved AgentRegistry member. An
    // agent outside a registry has no peers by construction, so this is a configuration
    // answer rather than a runtime failure — say so, instead of letting the caller meet a
    // connection-refused on a port they never heard of.
    if (!this.config.meshWired) {
      throw new ConfigError(
        "the mesh is not wired for this agent: the launcher starts its AMP listener only " +
          "for a resolved AgentRegistry member (AGENT_REGISTRY_ID). Add this agent to an " +
          "AgentRegistry to call peers.",
      );
    }
  }

  /**
   * Call `target` in this agent's registry and return its response.
   *
   * `payload` is yours and opaque to the platform — the launcher wraps it in the envelope
   * rather than inspecting it.
   *
   * Throws ConfigError when the mesh is unwired or `target` is malformed, and EndpointError
   * (carrying `.status`) when the launcher refuses the hop: 403 for a disallowed caller or a
   * cross-registry target, 404 for an unknown one, 502 for a blocked or failed peer. A
   * refusal is an OUTCOME delivered typed — the launcher fails fast precisely so a governed
   * mesh never presents as a hang.
   */
  async call(
    target: string,
    payload: Record<string, unknown> = {},
    opts: { timeoutMs?: number } = {},
  ): Promise<Record<string, unknown>> {
    this.requireWired();
    const name = (target ?? "").trim();
    if (!name) {
      throw new ConfigError("a target agent name is required");
    }
    if (name.length > MAX_TARGET) {
      throw new ConfigError(`target agent name too long (max ${MAX_TARGET})`);
    }
    if (name.includes("/") || name.includes("..")) {
      // Defence in depth on top of the launcher's validation: a name is a path segment, and
      // a segment that can escape its position is never legitimate.
      throw new ConfigError(`invalid target agent name: ${JSON.stringify(name)}`);
    }

    // Deliberately the legacy path. The SDK ships in the customer's image and can be
    // upgraded BEFORE the platform, so posting /amp/ would 404 against a launcher that
    // predates ADR 0138. Launchers serve both; this moves to /amp/ once the both-paths
    // release is everywhere. Mirror of the header ordering.
    const url = `${this.config.meshBaseUrl}/a2a/${encodeURIComponent(name)}`;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? 30_000);
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
        `mesh call to ${name} failed: ${(err as Error).message}`,
      );
    } finally {
      clearTimeout(timer);
    }
    if (resp.status !== 200) {
      const body = await resp.text().catch(() => "");
      throw new EndpointError(
        `mesh call to ${name} returned ${resp.status}: ${body.slice(0, 200)}`,
        { status: resp.status, body },
      );
    }
    const data = (await resp.json().catch(() => null)) as unknown;
    return data && typeof data === "object" && !Array.isArray(data)
      ? (data as Record<string, unknown>)
      : { response: data };
  }
}
