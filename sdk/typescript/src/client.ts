/**
 * The Client facade: one object holding the localhost-plane data-plane clients.
 * Parity with `sdk/python/src/ctxmesh/client.py`.
 *
 * Constructed by `agent.fromEnv()` (in-pod) or `agent.fromConfig()` (tests/offline).
 * Each attribute wraps one raw launcher endpoint:
 *
 *     client.memory     -> :2998               (M5)
 *     client.knowledge  -> :2998               (M68) — knowledge-base retrieval
 *     client.feedback   -> :2995               (M9)
 *     client.model      -> $MODEL_GATEWAY_URL   (M2/M8)
 *
 * Tools (M4/MCP) land in M77.3; trace in M77.4; serve/managed-loop in M77.5.
 *
 * `withConversation(conversationId)` returns a new Client whose memory ops default
 * to that conversationId — bind it once per request so `client.memory.get()` needs
 * no repeated id argument through the turn.
 */

import { PlaneConfig } from "./config.js";
import { FeedbackClient } from "./feedback.js";
import { KnowledgeClient } from "./knowledge.js";
import { MemoryClient } from "./memory.js";
import { ModelClient } from "./model.js";

export class Client {
  readonly memory: MemoryClient;
  readonly knowledge: KnowledgeClient;
  readonly feedback: FeedbackClient;
  readonly model: ModelClient;

  private readonly _config: PlaneConfig;

  constructor(config: PlaneConfig) {
    this._config = config;
    this.memory = new MemoryClient(config);
    this.knowledge = new KnowledgeClient(config);
    this.feedback = new FeedbackClient(config);
    this.model = new ModelClient(config);
  }

  get config(): PlaneConfig {
    return this._config;
  }

  get run() {
    return this._config.run;
  }

  /**
   * Return a client whose memory ops default to `conversationId`.
   *
   * conversationId is per-request (the agent reads it from its inbound payload).
   * Bind it once here so `client.memory.get()` needs no repeated id argument through
   * the turn. The knowledge/feedback/model clients are shared (they are not
   * conversation-scoped).
   */
  withConversation(conversationId: string): Client {
    const bound = new Client(this._config);
    // Replace only the memory client with a conversation-scoped one; share the rest.
    (bound as { memory: MemoryClient }).memory = new MemoryClient(
      this._config,
      conversationId,
    );
    return bound;
  }
}
