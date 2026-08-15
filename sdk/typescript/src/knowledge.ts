/**
 * Knowledge-retrieval client — typed sugar over the launcher's :2998 knowledge endpoint (M68).
 * Parity with `sdk/python/src/ctxmesh/knowledge.py`.
 *
 * Wire contract (ADR 0061, Fork 3):
 *
 *     POST /knowledge/search  <- {knowledgeBase, query, topK, threshold}
 *                             -> {results: [{content, documentRef, chunkIndex,
 *                                            startOffset, endOffset, mimeType, score}]}
 *
 * The launcher verifies the requested KB is in its injected KNOWLEDGE_BASES roster
 * (un-forgeable, exactly like the DELEGATE_ROSTER gate) and fills in the embeddingModel
 * from the KB spec — the SDK does NOT send embeddingModel.
 *
 * Gating:
 *   KNOWLEDGE_BASE_ENABLED=true   the launcher wired the /knowledge/search endpoint
 *   KNOWLEDGE_BASES               JSON list of {name, namespace, embeddingRoute}
 */

import { PlaneConfig } from "./config.js";
import { ConfigError, EndpointError } from "./errors.js";

/** Launcher-local knowledge search endpoint (same :2998 listener as memory). */
const KNOWLEDGE_SEARCH_PATH = "/knowledge/search";

/** Request timeout — generous for a vector-search round-trip (embedding call). */
const KNOWLEDGE_TIMEOUT_MS = 30_000;

interface KnowledgeEntry {
  name: string;
  [key: string]: unknown;
}

function parseKnowledgeRoster(): KnowledgeEntry[] {
  const raw = (process.env.KNOWLEDGE_BASES ?? "").trim();
  if (!raw) return [];
  let data: unknown;
  try {
    data = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(data)) return [];
  const out: KnowledgeEntry[] = [];
  for (const entry of data) {
    if (typeof entry === "object" && entry !== null && "name" in entry && (entry as KnowledgeEntry).name) {
      out.push(entry as KnowledgeEntry);
    }
  }
  return out;
}

function rosterNames(): string[] {
  return parseKnowledgeRoster().map((e) => String(e.name));
}

/**
 * Return the KB names whose roster entry has `autoInject: true` (ADR 0061 governance #5, M10).
 *
 * These are the corpora the in-pod SDK auto-retrieves on the user input each turn (RAG-style,
 * ephemeral `<retrieved_context>`). A KB WITHOUT the flag stays tool-only. An empty/malformed
 * roster → [] (no auto-inject — the tool-only default is byte-for-byte unchanged).
 * Parity with `_auto_inject_names` in the Python SDK.
 */
export function autoInjectNames(): string[] {
  return parseKnowledgeRoster()
    .filter((e) => e.autoInject === true)
    .map((e) => String(e.name));
}

/** A single knowledge search result chunk. */
export interface KnowledgeResult {
  content: string;
  documentRef: string;
  chunkIndex: number;
  startOffset: number;
  endOffset: number;
  mimeType: string;
  score: number;
}

/**
 * Knowledge-base retrieval against the launcher's :2998 /knowledge/search endpoint (M68).
 *
 * Exposed as `client.knowledge` on the SDK Client.
 */
export class KnowledgeClient {
  private readonly config: PlaneConfig;

  constructor(config: PlaneConfig) {
    this.config = config;
  }

  private requireEnabled(): void {
    if (!this.config.knowledgeEnabled) {
      throw new ConfigError(
        "knowledge base is not enabled for this agent: the launcher did not inject " +
          "KNOWLEDGE_BASE_ENABLED=true. Add spec.knowledgeBases[] to the AgentDeployment " +
          "to use client.knowledge.*",
      );
    }
  }

  /**
   * Return the KB names this agent is granted access to (from the KNOWLEDGE_BASES roster).
   *
   * Does NOT require knowledgeEnabled — readable at any time so tooling can discover
   * what is available.
   */
  available(): string[] {
    return rosterNames();
  }

  /**
   * Semantically search a knowledge base, returning ranked result chunks.
   *
   * - `query` is the free-text search query (required, non-empty).
   * - `knowledgeBase` is the KB name to search. When `undefined` and exactly ONE KB is
   *   granted, it defaults to that KB. When `undefined` and multiple KBs are granted,
   *   raises ConfigError naming the choices.
   * - `topK` is the maximum results to return.
   * - `threshold` is the minimum cosine similarity (0.0 = no floor).
   */
  async search(
    query: string,
    knowledgeBase?: string,
    topK = 10,
    threshold = 0.0,
  ): Promise<KnowledgeResult[]> {
    this.requireEnabled();

    if (!query || !query.trim()) {
      throw new ConfigError("knowledge.search requires a non-empty query");
    }

    let kbName = knowledgeBase;
    if (kbName === undefined) {
      const names = rosterNames();
      if (names.length === 1) {
        kbName = names[0];
      } else if (names.length === 0) {
        throw new ConfigError(
          "knowledge.search: no knowledge bases are granted to this agent " +
            "(KNOWLEDGE_BASES is empty). Add spec.knowledgeBases[] to the AgentDeployment.",
        );
      } else {
        throw new ConfigError(
          "knowledge.search: multiple knowledge bases are available — specify one " +
            "via the knowledgeBase argument. Available: " +
            names.map((n) => JSON.stringify(n)).join(", "),
        );
      }
    }

    const body = { knowledgeBase: kbName, query, topK, threshold };
    const url = `${this.config.memoryBaseUrl}${KNOWLEDGE_SEARCH_PATH}`;

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), KNOWLEDGE_TIMEOUT_MS);
    let resp: Response;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal: controller.signal,
      });
    } catch (err) {
      throw new EndpointError(
        `knowledge.search request failed: ${(err as Error).message}`,
      );
    } finally {
      clearTimeout(timer);
    }

    if (resp.status !== 200) {
      const text = await resp.text().catch(() => "");
      throw new EndpointError(
        `knowledge endpoint returned ${resp.status}: ${text.slice(0, 200)}`,
        { status: resp.status, body: text },
      );
    }

    const data: unknown = await resp.json();
    if (typeof data === "object" && data !== null && "results" in data) {
      const results = (data as Record<string, unknown>).results;
      return Array.isArray(results) ? (results as KnowledgeResult[]) : [];
    }
    return [];
  }
}
