"""Run-capability propagation + no-bleed (ADR 0030 §3).

Proves the invoking user's run capability is bound per-request in a ContextVar and relayed
on MCP tool-call egress, with ZERO cross-user bleed under concurrency — the core isolation
property of the OBO design.
"""

import threading

from ctxmesh._capability import (
    CAPABILITY_HEADER,
    capability_scope,
    current_capability,
)
from ctxmesh.tools import _mcp_headers


def test_current_capability_is_none_outside_any_scope():
    assert current_capability() is None


def test_scope_binds_and_resets():
    assert current_capability() is None
    with capability_scope({CAPABILITY_HEADER: "cap-abc"}):
        assert current_capability() == "cap-abc"
    # Reset on exit — a reused worker never leaks a prior request's capability.
    assert current_capability() is None


def test_missing_or_blank_header_binds_none():
    with capability_scope({}):
        assert current_capability() is None
    with capability_scope({CAPABILITY_HEADER: "   "}):
        assert current_capability() is None
    with capability_scope(None):
        assert current_capability() is None


def test_header_extraction_is_case_insensitive():
    # HTTP header case is not guaranteed on the wire.
    with capability_scope({"x-ctxmesh-run-capability": "cap-lower"}):
        assert current_capability() == "cap-lower"
    with capability_scope({"X-CTXMESH-RUN-CAPABILITY": "cap-upper"}):
        assert current_capability() == "cap-upper"


def test_mcp_headers_carry_capability_only_in_scope():
    # Outside a scope: no capability header on the egress request.
    assert CAPABILITY_HEADER not in _mcp_headers(None)
    # Inside a scope: the capability is attached to every tool-call egress.
    with capability_scope({CAPABILITY_HEADER: "cap-xyz"}):
        headers = _mcp_headers("session-1")
        assert headers[CAPABILITY_HEADER] == "cap-xyz"
        assert headers["Mcp-Session-Id"] == "session-1"


def test_no_cross_user_bleed_under_concurrency():
    """N threads each bind a DISTINCT capability and, held simultaneously inside their
    scopes by a barrier, each must observe ONLY its own — never another's. This is the
    thread-per-request isolation the entrypoint's ThreadingHTTPServer relies on."""
    n = 16
    barrier = threading.Barrier(n)
    seen = [None] * n
    egress = [None] * n
    errors = []

    def worker(i):
        try:
            with capability_scope({CAPABILITY_HEADER: f"cap-{i}"}):
                # Force every thread to be inside its own scope at the same instant, so any
                # shared mutable state would manifest as a wrong read here.
                barrier.wait(timeout=5)
                seen[i] = current_capability()
                egress[i] = _mcp_headers(None).get(CAPABILITY_HEADER)
        except Exception as exc:  # pragma: no cover - surfaced via errors list
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert not errors, f"worker errors: {errors}"
    for i in range(n):
        assert seen[i] == f"cap-{i}", f"thread {i} saw {seen[i]!r} — cross-user bleed"
        assert egress[i] == f"cap-{i}", f"thread {i} egress carried {egress[i]!r}"
    # Back on the main thread, no capability leaked out of any worker's scope.
    assert current_capability() is None


def test_no_global_setter_exists():
    """Structural no-bleed guarantee: the module exposes NO way to set a process-wide
    capability — only the request-scoped context manager. A bare setter would let injected
    agent code hijack another request's identity."""
    import ctxmesh._capability as cap

    public_setters = [
        name
        for name in dir(cap)
        if not name.startswith("_")
        and callable(getattr(cap, name))
        and ("set" in name.lower() or "put" in name.lower())
    ]
    assert public_setters == [], f"unexpected capability setters: {public_setters}"


def test_client_request_scope_binds_capability(client):
    """DX-2: client.request_scope binds the run capability from the inbound /invoke headers,
    so a CUSTOM agent loop's tool egress relays the user's OBO capability instead of silently
    resolving org/public creds. Resets on exit (no cross-request bleed)."""
    assert current_capability() is None
    with client.request_scope({CAPABILITY_HEADER: "cap-dx2"}):
        assert current_capability() == "cap-dx2"
    assert current_capability() is None
