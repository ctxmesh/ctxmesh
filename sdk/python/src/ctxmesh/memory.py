"""Memory client — typed sugar over the launcher's :2998 memory endpoint (M5).

Wire contract (state-layer.md, "The :2998 launcher memory endpoint"):

    GET    /memory/{conversationId}          -> JSON array (empty [] if none)
    PUT    /memory/{conversationId}          <- JSON-array body (replace)      204
    POST   /memory/{conversationId}/append   <- one JSON value (append)        204
    GET    /memory/{conversationId}/search?q= -> JSON array (substring match)

The conversationId scopes the key ``mem:{namespace}/{agent}:{conversationId}``;
namespace + agent are the launcher's own env, so the SDK only supplies the
conversationId. It is validated client-side against the same rules the endpoint
enforces so a bad id surfaces as a clear ConfigError instead of a mangled URL.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional
from urllib.parse import quote

from ctxmesh import _http
from ctxmesh._capability import CAPABILITY_HEADER, current_capability
from ctxmesh._tracing import current_traceparent
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError

#: Mirrors the launcher's maxConversationID (state-layer.md).
_MAX_CONVERSATION_ID = 128


def _validate_conversation_id(conv_id: str) -> None:
    """Client-side mirror of the :2998 contract's conversationId rules.

    Raises ConfigError on an id the endpoint would reject anyway (empty, too
    long, or containing a path/key separator or whitespace).
    """
    if not conv_id:
        raise ConfigError(
            "no conversationId: pass one to the memory call, or set it on the "
            "client via client.with_conversation(id) / CONVERSATION_ID env"
        )
    if len(conv_id) > _MAX_CONVERSATION_ID:
        raise ConfigError(f"conversationId too long (max {_MAX_CONVERSATION_ID})")
    for ch in conv_id:
        if ch in "/:" or ch.isspace() or ord(ch) < 0x20 or ord(ch) == 0x7F:
            raise ConfigError(f"conversationId contains disallowed character {ch!r}")


class MemoryClient:
    """Session-memory operations against the launcher's :2998 endpoint."""

    def __init__(self, config: PlaneConfig, conversation_id: str = ""):
        self._config = config
        self._conversation_id = conversation_id or config.run.conversation_id

    def _require_wired(self) -> None:
        if not self._config.memory_wired:
            raise ConfigError(
                "memory is not wired for this agent: the launcher did not inject "
                "MEMORY_PORT/MEMORY_BACKEND_ADDR (no MemoryBinding). Bind memory "
                "to the agent to use client.memory.*"
            )

    def _url(self, conversation_id: Optional[str], suffix: str = "") -> str:
        conv_id = conversation_id or self._conversation_id
        _validate_conversation_id(conv_id)
        # quote(safe="") is defence-in-depth on top of validation: a validated
        # id needs no escaping, but the URL must never be corruptible.
        return f"{self._config.memory_base_url}/memory/{quote(conv_id, safe='')}{suffix}"

    def _session_headers(self, extra: Optional[Dict[str, str]] = None) -> Dict[str, str]:
        """Headers for a SESSION-memory call. Attaches the run capability (like long-term memory)
        so the launcher can user-scope per-user session memory (M98, EU1a, ADR 0080). Harmless when
        the agent is not perUser: the launcher strips the capability before forwarding to the
        state-layer proxy either way, and only derives X-Memory-User from it when perUser is on."""
        headers = dict(extra or {})
        cap = current_capability()
        if cap:
            headers[CAPABILITY_HEADER] = cap
        return headers

    def get(self, conversation_id: Optional[str] = None) -> List[Any]:
        """GET the full conversation context as a list of entries (empty if none)."""
        self._require_wired()
        resp = _http.request(
            "GET", self._url(conversation_id), headers=self._session_headers(), expect=(200,)
        )
        data = resp.json()
        return data if isinstance(data, list) else []

    def put(self, entries: List[Any], conversation_id: Optional[str] = None) -> None:
        """PUT (replace) the whole conversation context with a JSON array."""
        self._require_wired()
        if not isinstance(entries, list):
            raise ConfigError("memory.put expects a list of entries (the JSON-array body)")
        _http.request(
            "PUT",
            self._url(conversation_id),
            body=_http.json_body(entries),
            headers=self._session_headers({"Content-Type": "application/json"}),
            expect=(204,),
        )

    def append(
        self, entry: Any, conversation_id: Optional[str] = None, message_id: Optional[str] = None
    ) -> None:
        """POST-append one JSON value to the conversation context.

        message_id (m33.4): the per-hop id to attribute a message entry to. When set it rides
        X-Message-Id, which the launcher's :2998 endpoint stamps onto the entry (ADR 0035); absent,
        the endpoint mints one. Relayed by the managed loop from the inbound AMP hop's messageId.
        """
        self._require_wired()
        extra = {"Content-Type": "application/json"}
        if message_id:
            extra["X-Message-Id"] = message_id
        _http.request(
            "POST",
            self._url(conversation_id, "/append"),
            body=_http.json_body(entry),
            headers=self._session_headers(extra),
            expect=(204,),
        )

    def search(self, query: str = "", conversation_id: Optional[str] = None) -> List[Any]:
        """GET entries matching *query* (v1 = naive substring; empty q = all)."""
        self._require_wired()
        url = self._url(conversation_id, "/search") + f"?q={quote(query, safe='')}"
        resp = _http.request("GET", url, headers=self._session_headers(), expect=(200,))
        data = resp.json()
        return data if isinstance(data, list) else []

    # ── Long-term (agent-scope) memory (ADR 0045) ──────────────────────────────────
    # Persists ACROSS conversations and is retrieved by MEANING (embeddings), unlike
    # the conversation memory above. The launcher proxies these to the token-service.
    # For per-user memory the run capability is forwarded automatically (request-scoped)
    # so a user's memories are isolated from other users of the same agent.

    def _require_longterm(self) -> None:
        if not self._config.longterm_wired:
            raise ConfigError(
                "long-term memory is not enabled for this agent: set "
                "spec.longTermMemory.enabled (the launcher injects "
                "MEMORY_LONGTERM_ENABLED). Use remember / search_agent only when bound."
            )

    def _longterm_headers(self) -> Dict[str, str]:
        headers = {"Content-Type": "application/json"}
        cap = current_capability()
        if cap:
            headers[CAPABILITY_HEADER] = cap  # per-user scoping (launcher verifies+hashes)
        # Propagate the active run's W3C traceparent (m54.3) so the launcher can tag
        # the stored memory with its originating run's trace id — the console then
        # back-links each remembered fact to the trace that produced it. Best-effort:
        # absent when remember is called outside a traced run.
        tp = current_traceparent()
        if tp:
            headers["traceparent"] = tp
        return headers

    def remember(self, content: str, tags: Optional[Dict[str, str]] = None) -> None:
        """Store a long-term memory (ADR 0045). Returns immediately; the launcher embeds
        + persists it in the background (best-effort, like ``append`` — never blocks)."""
        self._require_longterm()
        if not content or not content.strip():
            raise ConfigError("memory.remember requires non-empty content")
        body: Dict[str, Any] = {"content": content}
        if tags:
            body["tags"] = tags
        _http.request(
            "POST",
            f"{self._config.memory_base_url}/memory/agent/remember",
            body=_http.json_body(body),
            headers=self._longterm_headers(),
            expect=(202,),
        )

    def search_agent(
        self, query: str, top_k: int = 5, threshold: float = 0.0
    ) -> List[Dict[str, Any]]:
        """Semantically retrieve the agent's most relevant long-term memories (ADR 0045).

        Returns a ranked list of ``{"content": str, "score": float}`` (score = cosine similarity in
        [0,1]); empty when nothing clears *threshold*. Traced as a ``memory.agent_recall`` span."""
        self._require_longterm()
        body = {"query": query, "topK": top_k, "threshold": threshold}
        resp = _http.request(
            "POST",
            f"{self._config.memory_base_url}/memory/agent/search",
            body=_http.json_body(body),
            headers=self._longterm_headers(),
            expect=(200,),
        )
        data = resp.json()
        results = data.get("results") if isinstance(data, dict) else None
        return results if isinstance(results, list) else []
