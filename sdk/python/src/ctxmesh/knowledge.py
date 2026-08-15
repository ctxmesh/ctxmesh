"""Knowledge-retrieval client — typed sugar over the launcher's :2998 knowledge endpoint (M68).

Wire contract (ADR 0061, Fork 3):

    POST /knowledge/search  <- {knowledgeBase, query, topK, threshold}
                            -> {results: [{content, documentRef, chunkIndex,
                                           startOffset, endOffset, mimeType, score}]}

The launcher verifies the requested KB is in its injected KNOWLEDGE_BASES roster
(un-forgeable, exactly like the DELEGATE_ROSTER gate) and fills in the embeddingModel
from the KB spec — the SDK does NOT send embeddingModel.

Gating:
  KNOWLEDGE_BASE_ENABLED=true   the launcher wired the /knowledge/search endpoint
  KNOWLEDGE_BASES               JSON list of {name, namespace, embeddingRoute}
                                the roster of granted corpora for this agent

When KNOWLEDGE_BASE_ENABLED != "true" every call raises ConfigError immediately.
"""

from __future__ import annotations

import json
import os
from typing import Any, Dict, List, Optional

from ctxmesh import _http
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError

#: Launcher-local knowledge search endpoint (same :2998 listener as memory).
_KNOWLEDGE_SEARCH_PATH = "/knowledge/search"

#: Request timeout — a vector search round-trip; the token-service may need
#: a moment for the embedding call, so be generous without being unlimited.
_KNOWLEDGE_TIMEOUT = 30.0


def _knowledge_enabled() -> bool:
    """True when the launcher wired the /knowledge/search endpoint."""
    return os.environ.get("KNOWLEDGE_BASE_ENABLED", "").strip() == "true"


def _knowledge_roster() -> List[Dict[str, Any]]:
    """Parse KNOWLEDGE_BASES env (a JSON list of {name, namespace, embeddingRoute}).

    Returns the parsed list (may be empty). Malformed JSON → [] (a misconfig must
    never crash the pod — the feature is simply unavailable).
    """
    raw = os.environ.get("KNOWLEDGE_BASES", "").strip()
    if not raw:
        return []
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return []
    if not isinstance(data, list):
        return []
    out: List[Dict[str, Any]] = []
    for entry in data:
        if isinstance(entry, dict) and entry.get("name"):
            out.append(entry)
    return out


def _roster_names() -> List[str]:
    """Return the list of granted KB names from the KNOWLEDGE_BASES roster."""
    return [str(e["name"]) for e in _knowledge_roster()]


def _auto_inject_names() -> List[str]:
    """Return the KB names whose roster entry has ``autoInject: true`` (ADR 0061 #5, M10).

    These are the corpora the in-pod SDK auto-retrieves on the user input each turn (RAG-style,
    ephemeral ``<retrieved_context>``). A KB WITHOUT the flag stays tool-only. An empty/malformed
    roster → [] (no auto-inject — the tool-only default is byte-for-byte unchanged).
    """
    return [str(e["name"]) for e in _knowledge_roster() if e.get("autoInject") is True]


class KnowledgeClient:
    """Knowledge-base retrieval against the launcher's :2998 /knowledge/search endpoint (M68).

    Expose as ``client.knowledge`` on the SDK Client (mirror of ``client.memory``).
    """

    def __init__(self, config: PlaneConfig) -> None:
        self._config = config

    def _require_enabled(self) -> None:
        if not _knowledge_enabled():
            raise ConfigError(
                "knowledge base is not enabled for this agent: the launcher did not inject "
                "KNOWLEDGE_BASE_ENABLED=true. Add spec.knowledgeBases[] to the AgentDeployment "
                "to use client.knowledge.*"
            )

    def available(self) -> List[str]:
        """Return the KB names this agent is granted access to (from the KNOWLEDGE_BASES roster).

        Does NOT require KNOWLEDGE_BASE_ENABLED — readable at any time so tooling
        (the synthetic tool injector, user code) can discover what is available.
        """
        return _roster_names()

    def search(
        self,
        query: str,
        knowledge_base: Optional[str] = None,
        top_k: int = 10,
        threshold: float = 0.0,
    ) -> List[Dict[str, Any]]:
        """Semantically search a knowledge base, returning ranked result chunks.

        ``query`` is the free-text search query (required, non-empty).
        ``knowledge_base`` is the KB name to search:
          - When ``None`` and exactly ONE KB is granted, it defaults to that KB.
          - When ``None`` and multiple KBs are granted, raises ConfigError naming
            the choices — the caller must be explicit.
          - When the plane is unconfigured (``KNOWLEDGE_BASE_ENABLED`` != true),
            raises ConfigError with a clear "not enabled" message.
        ``top_k`` is the maximum results to return (the launcher/token-service
        may return fewer when fewer chunks clear ``threshold``).
        ``threshold`` is the minimum cosine similarity (0.0 = no floor).

        Returns a list of result dicts, each carrying:
          content     — the retrieved text chunk
          documentRef — source document identifier (for citation)
          chunkIndex  — zero-based position within the document
          startOffset — byte/char offset of the chunk start
          endOffset   — byte/char offset of the chunk end
          mimeType    — media type of the original document
          score       — cosine similarity in [0, 1]

        Note: retrieved-IMAGE-chunk plumbing is deferred (v1 ingestion is
        text-only). mimeType will be "text/plain" for all v1 results.
        """
        self._require_enabled()

        if not query or not query.strip():
            raise ConfigError("knowledge.search requires a non-empty query")

        # Resolve the knowledge_base name.
        kb_name = knowledge_base
        if kb_name is None:
            names = _roster_names()
            if len(names) == 1:
                kb_name = names[0]
            elif len(names) == 0:
                raise ConfigError(
                    "knowledge.search: no knowledge bases are granted to this agent "
                    "(KNOWLEDGE_BASES is empty). Add spec.knowledgeBases[] to the "
                    "AgentDeployment."
                )
            else:
                raise ConfigError(
                    "knowledge.search: multiple knowledge bases are available — specify one "
                    "via the knowledge_base argument. Available: "
                    + ", ".join(repr(n) for n in names)
                )

        body: Dict[str, Any] = {
            "knowledgeBase": kb_name,
            "query": query,
            "topK": top_k,
            "threshold": threshold,
        }
        resp = _http.request(
            "POST",
            f"{self._config.memory_base_url}{_KNOWLEDGE_SEARCH_PATH}",
            body=_http.json_body(body),
            headers={"Content-Type": "application/json"},
            timeout=_KNOWLEDGE_TIMEOUT,
            expect=(200,),
        )
        data = resp.json()
        results = data.get("results") if isinstance(data, dict) else None
        return results if isinstance(results, list) else []
