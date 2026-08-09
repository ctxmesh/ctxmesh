"""Knowledge client + synthetic tool + multimodal helpers (M68, ADR 0061).

Tests mirror the memory/tools test style: a lightweight HTTP stub stands in for the
launcher's :2998 /knowledge/search endpoint; env manipulation with monkeypatch gates
the KNOWLEDGE_BASE_ENABLED + KNOWLEDGE_BASES signals.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, Iterator, List, Optional
from urllib.parse import urlparse

import pytest

import ctxmesh
from ctxmesh import agent, content, image_url, text_part
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError
from ctxmesh.knowledge import KnowledgeClient

# ── minimal stub for :2998/knowledge/search ───────────────────────────────────


class KnowledgeStub:
    """Tiny in-process HTTP stub for POST /knowledge/search (the M68 wire contract)."""

    def __init__(self, results: Optional[List[Dict[str, Any]]] = None) -> None:
        self.results = results if results is not None else [
            {
                "content": "The capital of France is Paris.",
                "documentRef": "doc-42",
                "chunkIndex": 0,
                "startOffset": 0,
                "endOffset": 40,
                "mimeType": "text/plain",
                "score": 0.97,
            }
        ]
        self.requests: List[Dict[str, Any]] = []
        self._force_status: Optional[int] = None

        handler_cls = self._make_handler()
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    def __enter__(self) -> "KnowledgeStub":
        self._thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=2)

    def _make_handler(self):
        stub = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args: Any) -> None:
                pass

            def do_POST(self) -> None:
                parsed = urlparse(self.path)
                length = int(self.headers.get("Content-Length", 0) or 0)
                body = self.rfile.read(length) if length else b""
                stub.requests.append(
                    {
                        "path": parsed.path,
                        "body": json.loads(body) if body else {},
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                    }
                )
                if stub._force_status is not None:
                    self.send_response(stub._force_status)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                resp_body = json.dumps({"results": stub.results}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(resp_body)))
                self.end_headers()
                self.wfile.write(resp_body)

        return Handler


@pytest.fixture
def kb_stub() -> Iterator[KnowledgeStub]:
    with KnowledgeStub() as stub:
        yield stub


def _kb_config(stub: KnowledgeStub) -> PlaneConfig:
    """A PlaneConfig pointing at the knowledge stub's base URL."""
    # The KnowledgeClient uses memory_base_url (same :2998 listener).
    return PlaneConfig.for_test(memory_base_url=stub.base_url)


# ── KnowledgeClient.search ────────────────────────────────────────────────────


def test_search_posts_correct_body(kb_stub: KnowledgeStub, monkeypatch):
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "docs", "namespace": "default"}]))

    cfg = _kb_config(kb_stub)
    kc = KnowledgeClient(cfg)
    results = kc.search("capital of France", knowledge_base="docs", top_k=5, threshold=0.5)

    assert results == kb_stub.results
    assert len(kb_stub.requests) == 1
    req = kb_stub.requests[0]
    assert req["path"] == "/knowledge/search"
    body = req["body"]
    assert body["knowledgeBase"] == "docs"
    assert body["query"] == "capital of France"
    assert body["topK"] == 5
    assert body["threshold"] == 0.5
    # SDK must NOT send embeddingModel (the launcher fills it from the KB spec).
    assert "embeddingModel" not in body


def test_search_returns_results_list(kb_stub: KnowledgeStub, monkeypatch):
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "kb1", "namespace": "default"}]))

    cfg = _kb_config(kb_stub)
    kc = KnowledgeClient(cfg)
    results = kc.search("Paris", knowledge_base="kb1")
    assert isinstance(results, list)
    assert results[0]["documentRef"] == "doc-42"
    assert results[0]["score"] == 0.97


def test_search_defaults_to_single_granted_kb(kb_stub: KnowledgeStub, monkeypatch):
    """When knowledge_base is None and exactly one KB is granted, default to it."""
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "solo-kb", "namespace": "ns"}]))

    cfg = _kb_config(kb_stub)
    kc = KnowledgeClient(cfg)
    # No knowledge_base argument — should default to "solo-kb"
    results = kc.search("something")
    assert results == kb_stub.results
    req = kb_stub.requests[0]
    assert req["body"]["knowledgeBase"] == "solo-kb"


def test_search_raises_on_multiple_kbs_without_choice(monkeypatch):
    """When knowledge_base is None and multiple KBs are granted, raise ConfigError naming them."""
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv(
        "KNOWLEDGE_BASES",
        json.dumps([
            {"name": "kb-alpha", "namespace": "ns"},
            {"name": "kb-beta", "namespace": "ns"},
        ]),
    )
    cfg = PlaneConfig.for_test()
    kc = KnowledgeClient(cfg)
    with pytest.raises(ConfigError) as exc:
        kc.search("query")
    err = str(exc.value)
    assert "kb-alpha" in err
    assert "kb-beta" in err


def test_search_raises_when_not_enabled(monkeypatch):
    """When KNOWLEDGE_BASE_ENABLED != true, every call raises ConfigError immediately."""
    monkeypatch.delenv("KNOWLEDGE_BASE_ENABLED", raising=False)
    cfg = PlaneConfig.for_test()
    kc = KnowledgeClient(cfg)
    with pytest.raises(ConfigError) as exc:
        kc.search("anything")
    assert "not enabled" in str(exc.value).lower()


def test_search_raises_on_empty_query(monkeypatch):
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "kb", "namespace": "ns"}]))
    cfg = PlaneConfig.for_test()
    kc = KnowledgeClient(cfg)
    with pytest.raises(ConfigError):
        kc.search("   ")


def test_available_returns_roster_names(monkeypatch):
    monkeypatch.setenv(
        "KNOWLEDGE_BASES",
        json.dumps([
            {"name": "docs", "namespace": "default"},
            {"name": "wiki", "namespace": "public"},
        ]),
    )
    cfg = PlaneConfig.for_test()
    kc = KnowledgeClient(cfg)
    assert kc.available() == ["docs", "wiki"]


def test_available_returns_empty_when_no_roster(monkeypatch):
    monkeypatch.delenv("KNOWLEDGE_BASES", raising=False)
    cfg = PlaneConfig.for_test()
    kc = KnowledgeClient(cfg)
    assert kc.available() == []


def test_client_exposes_knowledge_attr(kb_stub: KnowledgeStub, monkeypatch):
    """client.knowledge is a KnowledgeClient, exposed at the SDK Client level."""
    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "kb", "namespace": "ns"}]))
    cfg = _kb_config(kb_stub)
    c = agent.from_config(cfg)
    assert isinstance(c.knowledge, KnowledgeClient)
    results = c.knowledge.search("Paris", knowledge_base="kb")
    assert results == kb_stub.results


# ── synthetic knowledge_search tool ──────────────────────────────────────────


def test_knowledge_search_tool_injected_when_enabled(monkeypatch):
    """knowledge_search tool appears in tools.list() when KNOWLEDGE_BASE_ENABLED + roster set."""
    from ctxmesh.testing import DiscoveryStub

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "docs", "namespace": "default"}]))

    with DiscoveryStub() as disc:
        cfg = PlaneConfig.for_test(discovery_base_url=disc.base_url)
        c = agent.from_config(cfg)
        tools = c.tools.list()
        names = [t.name for t in tools]
        assert "knowledge_search" in names
        # MCP tools still present
        assert "word-count" in names

        ks_tool = next(t for t in tools if t.name == "knowledge_search")
        assert ks_tool.mode == "knowledge"
        # Schema must have query as required
        assert "query" in ks_tool.input_schema["required"]
        # Description mentions the granted corpus
        assert "docs" in ks_tool.description
        # The schema must have knowledge_base and top_k as optional properties
        props = ks_tool.input_schema["properties"]
        assert "knowledge_base" in props
        assert "top_k" in props


def test_knowledge_search_tool_absent_when_disabled(monkeypatch):
    """A plain agent (no KNOWLEDGE_BASE_ENABLED) never sees the knowledge_search tool."""
    from ctxmesh.testing import DiscoveryStub

    monkeypatch.delenv("KNOWLEDGE_BASE_ENABLED", raising=False)

    with DiscoveryStub() as disc:
        cfg = PlaneConfig.for_test(discovery_base_url=disc.base_url)
        c = agent.from_config(cfg)
        tools = c.tools.list()
        assert "knowledge_search" not in [t.name for t in tools]


def test_knowledge_search_tool_absent_when_roster_empty(monkeypatch):
    """Even with KNOWLEDGE_BASE_ENABLED=true, no roster → no knowledge_search tool."""
    from ctxmesh.testing import DiscoveryStub

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([]))

    with DiscoveryStub() as disc:
        cfg = PlaneConfig.for_test(discovery_base_url=disc.base_url)
        c = agent.from_config(cfg)
        tools = c.tools.list()
        assert "knowledge_search" not in [t.name for t in tools]


def test_knowledge_search_tool_dispatches_to_client(
    kb_stub: KnowledgeStub, monkeypatch
):
    """A managed loop knowledge_search tool call dispatches to client.knowledge.search and
    returns results including provenance (documentRef/chunkIndex) so the model can cite."""
    from ctxmesh.managed import _dispatch_knowledge_search

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "docs", "namespace": "default"}]))

    cfg = _kb_config(kb_stub)
    c = agent.from_config(cfg)

    result_str = _dispatch_knowledge_search(c, {"query": "Paris", "knowledge_base": "docs"})
    # Result is JSON-encoded; parse and verify provenance fields are present
    result = json.loads(result_str)
    assert isinstance(result, list)
    assert len(result) == 1
    assert result[0]["documentRef"] == "doc-42"
    assert result[0]["chunkIndex"] == 0
    assert result[0]["content"] == "The capital of France is Paris."

    # The stub was actually called
    assert len(kb_stub.requests) == 1
    assert kb_stub.requests[0]["body"]["knowledgeBase"] == "docs"
    assert kb_stub.requests[0]["body"]["query"] == "Paris"


def test_knowledge_search_dispatch_defaults_kb(kb_stub: KnowledgeStub, monkeypatch):
    """When knowledge_base is absent in args and one KB is granted, the dispatch defaults to it."""
    from ctxmesh.managed import _dispatch_knowledge_search

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "solo", "namespace": "ns"}]))

    cfg = _kb_config(kb_stub)
    c = agent.from_config(cfg)
    # No knowledge_base in args
    result_str = _dispatch_knowledge_search(c, {"query": "test query"})
    result = json.loads(result_str)
    assert isinstance(result, list)
    req = kb_stub.requests[0]
    assert req["body"]["knowledgeBase"] == "solo"


def test_knowledge_search_dispatch_error_is_honest_text(kb_stub: KnowledgeStub, monkeypatch):
    """When the retrieval fails, the dispatch returns honest error text (never raises)."""
    from ctxmesh.managed import _dispatch_knowledge_search

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "kb", "namespace": "ns"}]))

    # Make the stub return a 500
    kb_stub._force_status = 500
    cfg = _kb_config(kb_stub)
    c = agent.from_config(cfg)
    result = _dispatch_knowledge_search(c, {"query": "Paris", "knowledge_base": "kb"})
    # Should be a string error, not raise
    assert isinstance(result, str)
    assert "failed" in result.lower() or "error" in result.lower()


def test_knowledge_search_dispatch_empty_query_returns_error(monkeypatch):
    """An empty query returns an honest error string, not an exception."""
    from ctxmesh.managed import _dispatch_knowledge_search

    monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
    monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "kb", "namespace": "ns"}]))

    cfg = PlaneConfig.for_test()
    c = agent.from_config(cfg)
    result = _dispatch_knowledge_search(c, {"query": "", "knowledge_base": "kb"})
    assert "query" in result.lower()
    assert "error" in result.lower()


# ── multimodal content-parts helpers ─────────────────────────────────────────


def test_text_part_builds_correct_shape():
    part = text_part("Hello, world!")
    assert part == {"type": "text", "text": "Hello, world!"}


def test_text_part_empty_string():
    part = text_part("")
    assert part == {"type": "text", "text": ""}


def test_image_url_https_no_detail():
    part = image_url("https://example.com/photo.jpg")
    assert part == {"type": "image_url", "image_url": {"url": "https://example.com/photo.jpg"}}


def test_image_url_with_detail():
    part = image_url("https://example.com/photo.jpg", detail="high")
    assert part == {
        "type": "image_url",
        "image_url": {"url": "https://example.com/photo.jpg", "detail": "high"},
    }


def test_image_url_data_url():
    data = "data:image/png;base64,abc123"
    part = image_url(data)
    assert part["type"] == "image_url"
    assert part["image_url"]["url"] == data
    assert "detail" not in part["image_url"]


def test_content_assembles_from_positional_parts():
    parts = content(text_part("Describe this."), image_url("https://x.com/img.png"))
    assert parts == [
        {"type": "text", "text": "Describe this."},
        {"type": "image_url", "image_url": {"url": "https://x.com/img.png"}},
    ]


def test_content_accepts_list_argument():
    lst = [text_part("a"), text_part("b")]
    assert content(lst) == lst


def test_content_single_part():
    parts = content(text_part("only text"))
    assert parts == [{"type": "text", "text": "only text"}]


def test_content_empty():
    assert content() == []


def test_multimodal_helpers_exported_at_top_level():
    """text_part, image_url, content are importable directly from ctxmesh."""
    assert ctxmesh.text_part is text_part
    assert ctxmesh.image_url is image_url
    assert ctxmesh.content is content


def test_content_parts_pass_through_to_model_chat(kb_stub: KnowledgeStub, monkeypatch):
    """content() produces a list of dicts that can be used verbatim as the 'content' field
    of a message for model.chat. This is a shape/contract test, not a live gateway call."""
    msgs = [
        {
            "role": "user",
            "content": content(
                text_part("What is this?"),
                image_url("https://example.com/image.png"),
            ),
        }
    ]
    # The content field should be a list of two dicts with the right types.
    assert isinstance(msgs[0]["content"], list)
    assert msgs[0]["content"][0]["type"] == "text"
    assert msgs[0]["content"][1]["type"] == "image_url"
