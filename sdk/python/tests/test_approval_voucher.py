"""The stateless approval-voucher relay (ADR 0074 §3, m82.4).

The egress sidecar answers a require-approval tool call with a typed 403 approval_required unless
the request carries a valid X-Ctxmesh-Approval voucher. The SDK:
  * maps that 403 to the existing ApprovalRequiredError (so the managed loop pauses — the wire is
    the enforcement point even for the managed loop; a custom loop that ignores it is denied);
  * on a resumed run, relays the voucher (bound per-request from the inbound header) on every
    tool-call egress so the sidecar forwards the approved tool.
"""

import json

import pytest

from ctxmesh._approval import (
    APPROVAL_HEADER,
    current_approval_voucher,
    voucher_scope,
)
from ctxmesh.errors import ApprovalRequiredError, EndpointError
from ctxmesh.tools import _mcp_headers, _raise_if_approval_required

# ── The wire 403 → ApprovalRequiredError mapping ─────────────────────────────────────────────────


def test_detects_structured_approval_required():
    body = json.dumps(
        {"error": "approval_required", "tool": "send_email", "run": "run-1", "server": "mail"}
    )
    with pytest.raises(ApprovalRequiredError) as excinfo:
        _raise_if_approval_required(EndpointError("forbidden", status=403, body=body))
    # The key mirrors the managed loop's tool:<name> so an approval resolves the SAME point.
    assert excinfo.value.key == "tool:send_email"


def test_ignores_non_approval_errors():
    for exc in (
        EndpointError("x", status=500, body='{"error":"approval_required"}'),  # wrong status
        EndpointError("x", status=403, body='{"error":"consent_required"}'),  # a different 403
        EndpointError("x", status=403, body=None),  # no body
        EndpointError("x", status=403, body="not json"),  # malformed
        EndpointError("x", status=None, body=None),  # transport failure
    ):
        _raise_if_approval_required(exc)  # returns without raising


# ── The voucher relay scope ──────────────────────────────────────────────────────────────────────


def test_current_voucher_is_none_outside_any_scope():
    assert current_approval_voucher() is None


def test_scope_binds_and_resets():
    assert current_approval_voucher() is None
    with voucher_scope({APPROVAL_HEADER: "voucher-abc"}):
        assert current_approval_voucher() == "voucher-abc"
    assert current_approval_voucher() is None  # reset on exit — no cross-request bleed


def test_missing_or_blank_header_binds_none():
    with voucher_scope({}):
        assert current_approval_voucher() is None
    with voucher_scope({APPROVAL_HEADER: "  "}):
        assert current_approval_voucher() is None
    with voucher_scope(None):
        assert current_approval_voucher() is None


def test_header_extraction_is_case_insensitive():
    with voucher_scope({"x-ctxmesh-approval": "v-lower"}):
        assert current_approval_voucher() == "v-lower"
    with voucher_scope({"X-CTXMESH-APPROVAL": "v-upper"}):
        assert current_approval_voucher() == "v-upper"


def test_mcp_headers_relay_voucher_only_in_scope():
    # Outside a scope: no voucher header on the egress request.
    assert APPROVAL_HEADER not in _mcp_headers(None)
    # Inside a scope: the voucher is relayed on every tool-call egress.
    with voucher_scope({APPROVAL_HEADER: "voucher-xyz"}):
        headers = _mcp_headers("session-1")
        assert headers[APPROVAL_HEADER] == "voucher-xyz"


def test_client_request_scope_binds_voucher(client):
    """A resumed run's inbound X-Ctxmesh-Approval header is bound via request_scope, so the
    tool-call egress relays the voucher — exactly like the run capability. Resets on exit."""
    assert current_approval_voucher() is None
    with client.request_scope({APPROVAL_HEADER: "voucher-resume"}):
        assert current_approval_voucher() == "voucher-resume"
        assert _mcp_headers(None)[APPROVAL_HEADER] == "voucher-resume"
    assert current_approval_voucher() is None


def test_no_global_setter_exists():
    """Structural no-bleed: no way to set a process-wide voucher — only the request-scoped manager.

    Restrict the scan to names DEFINED in the module (its own functions), so an imported typing name
    like ``FrozenSet`` doesn't trip the 'set'-substring heuristic."""
    import ctxmesh._approval as appr

    public_setters = [
        name
        for name in dir(appr)
        if not name.startswith("_")
        and callable(getattr(appr, name))
        and getattr(getattr(appr, name), "__module__", None) == appr.__name__
        and ("set" in name.lower() or "put" in name.lower())
    ]
    assert public_setters == [], f"unexpected voucher setters: {public_setters}"
