"""Unit tests for the m6.7 A2A delegation path in agent.py.

Tests the three required scenarios using a stubbed A2A endpoint (no real
launcher or network required — pattern mirrors the m5.6 memory test approach):

  A. Happy delegate: call_agent set, worker reachable → delegated_to + delegate_output.
  B. A2A unavailable: call_agent set, connection refused → a2a: "unavailable",
     agent still answers (output present).
  C. No call_agent: unchanged behavior — no a2a/delegated_to fields in output.

Additionally tests the typed HTTP error codes (403, 404, 502) and timeout.

Run with:
  python3 examples/langchain-agent/test_a2a.py
"""

from __future__ import annotations

import json
import os
import socket
import socketserver
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import MagicMock, patch

# ── Ensure agent.py is importable without installed LangChain deps ──────────
# We patch the heavy deps before importing so the module-level ChatOpenAI
# construction doesn't fail in a bare Python environment.  The A2A helpers
# we test are pure stdlib and don't touch the LLM at all.

_langchain_core_mock = MagicMock()
_langchain_openai_mock = MagicMock()
sys.modules.setdefault("langchain_core", _langchain_core_mock)
sys.modules.setdefault("langchain_core.prompts", _langchain_core_mock.prompts)
sys.modules.setdefault("langchain_core.tools", _langchain_core_mock.tools)
sys.modules.setdefault("langchain_openai", _langchain_openai_mock)

# tool decorator — return a MagicMock (not the raw function) so the result has
# an .invoke() method (LangChain @tool produces a BaseTool with .invoke).
def _tool_mock(f):
    m = MagicMock()
    m.__name__ = f.__name__
    # Make .invoke() call the underlying function like LangChain does.
    m.invoke = lambda args: f(**args) if isinstance(args, dict) else f(args)
    return m

_langchain_core_mock.tools.tool = _tool_mock

# Now import the module under test.
_here = os.path.dirname(__file__)
if _here not in sys.path:
    sys.path.insert(0, _here)

import agent  # noqa: E402 — must come after the sys.modules patches


# ── Stub A2A server ──────────────────────────────────────────────────────────

class _StubA2AHandler(BaseHTTPRequestHandler):
    """Minimal HTTP handler that simulates the launcher's /a2a endpoint.

    Class-level attribute ``responses`` maps path → (status, body dict).
    """
    responses: dict = {}
    captured_headers: list = []

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        _StubA2AHandler.captured_headers.append(dict(self.headers))
        status, body = _StubA2AHandler.responses.get(self.path, (404, {"error": "not found"}))
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args) -> None:
        pass


def _find_free_port() -> int:
    """Bind to port 0 and return the OS-assigned ephemeral port number."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class _StubServer:
    """Reusable stub A2A server that allocates its own free port."""

    def __init__(self, responses: dict):
        _StubA2AHandler.responses = responses
        _StubA2AHandler.captured_headers = []
        self._server = HTTPServer(("127.0.0.1", 0), _StubA2AHandler)
        self._server.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.port = self._server.server_address[1]
        self.base_url = f"http://127.0.0.1:{self.port}"
        t = threading.Thread(target=self._server.serve_forever, daemon=True)
        t.start()

    def shutdown(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    @property
    def captured_headers(self):
        return _StubA2AHandler.captured_headers


# ── Mock helpers ─────────────────────────────────────────────────────────────

def _make_mock_chain(content: str = "MOCK_OK") -> MagicMock:
    """Return a MagicMock that acts like a composed LangChain chain."""
    chain = MagicMock()
    result = MagicMock()
    result.content = content
    chain.invoke.return_value = result
    return chain


def _or_chain(chain: MagicMock):
    """Return a __or__ lambda that always returns *chain*, for mocking ``prompt | llm``."""
    return lambda s, o: chain


# ── Test cases ───────────────────────────────────────────────────────────────

class TestA2AHappyDelegate(unittest.TestCase):
    """Scenario A: call_agent set, worker reachable → delegated_to + delegate_output."""

    def setUp(self):
        self.stub = _StubServer({
            # path keyed on target agent name
        })
        # Will be updated per-test.
        self._orig_base = agent.A2A_BASE_URL

    def tearDown(self):
        self.stub.shutdown()
        agent.A2A_BASE_URL = self._orig_base

    def _set_stub_response(self, path: str, status: int, body: dict) -> None:
        _StubA2AHandler.responses = {path: (status, body)}
        _StubA2AHandler.captured_headers = []
        agent.A2A_BASE_URL = self.stub.base_url

    def test_happy_delegate(self):
        """Scenario A: delegated_to + delegate_output present, no a2a error."""
        self._set_stub_response("/a2a/worker-agent", 200, {
            "agent": "langchain-agent",
            "output": "MOCK_OK",
        })
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None):
            result = agent.run_agent(
                user_input="hello",
                call_agent="worker-agent",
                traceparent="00-abc123-def456-01",
            )

        # The orchestrator produces its own output.
        self.assertIn("output", result)
        self.assertEqual(result["output"], "MOCK_OK")
        # And incorporates the delegated response.
        self.assertEqual(result["delegated_to"], "worker-agent")
        self.assertIn("delegate_output", result)
        self.assertEqual(result["delegate_output"]["output"], "MOCK_OK")
        # No a2a error markers.
        self.assertNotIn("a2a", result)
        self.assertNotIn("a2a_error", result)

    def test_traceparent_forwarded(self):
        """traceparent from inbound request is forwarded to the stub A2A server."""
        self._set_stub_response("/a2a/worker-agent", 200, {"output": "MOCK_OK"})
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None):
            agent.run_agent(
                user_input="trace test",
                call_agent="worker-agent",
                traceparent="00-traceid-spanid-01",
            )

        headers = self.stub.captured_headers
        self.assertTrue(len(headers) > 0, "No request reached the stub")
        hdr_map = {k.lower(): v for k, v in headers[-1].items()}
        self.assertEqual(hdr_map.get("traceparent"), "00-traceid-spanid-01")

    def test_conv_id_forwarded_as_x_conversation_id(self):
        """X-Conversation-Id is sent on the A2A call when conv_id is valid."""
        self._set_stub_response("/a2a/worker-agent", 200, {"output": "MOCK_OK"})
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None), \
             patch("agent._memory_get", return_value=None), \
             patch("agent._memory_append", return_value=False):
            agent.run_agent(
                user_input="conv test",
                call_agent="worker-agent",
                conv_id="test-conv-1",
                traceparent=None,
            )

        headers = self.stub.captured_headers
        self.assertTrue(len(headers) > 0, "No request reached the stub")
        hdr_map = {k.lower(): v for k, v in headers[-1].items()}
        self.assertEqual(hdr_map.get("x-conversation-id"), "test-conv-1")


class TestA2AUnavailable(unittest.TestCase):
    """Scenario B: call_agent set, no listener → a2a: unavailable, agent answers."""

    def setUp(self):
        self._orig_base = agent.A2A_BASE_URL
        # Port with nothing listening — use a free port then immediately close it.
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        agent.A2A_BASE_URL = f"http://127.0.0.1:{port}"

    def tearDown(self):
        agent.A2A_BASE_URL = self._orig_base

    def test_a2a_unavailable_degrades(self):
        """Scenario B: agent answers normally, a2a: unavailable added."""
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None):
            result = agent.run_agent(
                user_input="hello",
                call_agent="worker-agent",
            )

        # Agent still produces its own answer.
        self.assertIn("output", result)
        self.assertEqual(result["output"], "MOCK_OK")
        # A2A marked as unavailable.
        self.assertEqual(result.get("a2a"), "unavailable")
        # No delegated output.
        self.assertNotIn("delegated_to", result)
        self.assertNotIn("delegate_output", result)


class TestA2ATypedErrors(unittest.TestCase):
    """Typed HTTP failures from the launcher (403, 404, 502) → a2a_error."""

    def setUp(self):
        self.stub = _StubServer({})
        self._orig_base = agent.A2A_BASE_URL
        agent.A2A_BASE_URL = self.stub.base_url

    def tearDown(self):
        self.stub.shutdown()
        agent.A2A_BASE_URL = self._orig_base

    def _run_with_stub_status(self, status: int, expected_code: str) -> None:
        _StubA2AHandler.responses = {
            "/a2a/worker-agent": (status, {"error": "typed"}),
        }
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None):
            result = agent.run_agent(user_input="hello", call_agent="worker-agent")
        # Agent still answers.
        self.assertIn("output", result)
        self.assertEqual(result["output"], "MOCK_OK")
        # Typed error marker in a2a_error.
        self.assertEqual(result.get("a2a_error"), expected_code, f"Expected a2a_error={expected_code!r} for HTTP {status}, got {result}")
        self.assertNotIn("delegated_to", result)

    def test_403_caller_not_allowed(self):
        self._run_with_stub_status(403, "caller_not_allowed")

    def test_404_unknown_target(self):
        self._run_with_stub_status(404, "unknown_target")

    def test_502_upstream_failure(self):
        self._run_with_stub_status(502, "upstream_failure")


class TestNoCallAgent(unittest.TestCase):
    """Scenario C: no call_agent → identical output to M5 (no a2a fields)."""

    def test_no_call_agent_unchanged_behavior(self):
        """Scenario C: byte-identical to pre-M6 behavior when call_agent absent."""
        chain = _make_mock_chain("MOCK_OK")
        with patch.object(agent, "_prompt", MagicMock(__or__=_or_chain(chain))), \
             patch.object(agent, "_llm", MagicMock()), \
             patch("agent._fetch_manifest", return_value=None):
            result = agent.run_agent(user_input="hello")

        # Standard output present.
        self.assertIn("output", result)
        self.assertEqual(result["output"], "MOCK_OK")
        # Absolutely no A2A-specific fields.
        self.assertNotIn("delegated_to", result)
        self.assertNotIn("delegate_output", result)
        self.assertNotIn("a2a", result)
        self.assertNotIn("a2a_error", result)


class TestA2ACallHelperDirect(unittest.TestCase):
    """Unit tests for _a2a_call directly, without going through run_agent."""

    def setUp(self):
        self._orig_base = agent.A2A_BASE_URL

    def tearDown(self):
        if hasattr(self, "stub"):
            self.stub.shutdown()
            del self.stub
        agent.A2A_BASE_URL = self._orig_base

    def test_success_returns_ok_body(self):
        self.stub = _StubServer({"/a2a/peer": (200, {"output": "MOCK_OK", "agent": "peer"})})
        agent.A2A_BASE_URL = self.stub.base_url
        result = agent._a2a_call("peer", {"input": "hi"}, None, None)
        self.assertTrue(result["ok"])
        self.assertEqual(result["body"]["output"], "MOCK_OK")

    def test_connection_refused_returns_unavailable(self):
        # Allocate and immediately close a port so nothing is listening.
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        agent.A2A_BASE_URL = f"http://127.0.0.1:{port}"
        result = agent._a2a_call("peer", {"input": "hi"}, None, None)
        self.assertFalse(result["ok"])
        self.assertEqual(result["code"], "unavailable")


if __name__ == "__main__":
    unittest.main(verbosity=2)
