"""The consent-required contract (ADR 0029 §2 / m25.9).

The egress sidecar answers a tool call with a structured consent_required when the invoking
user has not connected an account to the MCP server. The SDK turns that into a distinct,
catchable ConsentRequiredError (naming the server) so the managed loop can surface a
"Connect your account" run outcome instead of a generic tool failure.
"""

import json

import pytest

from ctxmesh.errors import ConsentRequiredError, EndpointError
from ctxmesh.tools import _raise_if_consent_required


def test_detects_structured_consent_required():
    body = json.dumps(
        {"error": "consent_required", "server": "scalekit-mcp-server", "message": "connect"}
    )
    with pytest.raises(ConsentRequiredError) as excinfo:
        _raise_if_consent_required(EndpointError("forbidden", status=403, body=body))
    assert excinfo.value.server == "scalekit-mcp-server"
    assert excinfo.value.status == 403


def test_ignores_non_consent_errors():
    # None of these should raise — the caller re-raises the original EndpointError.
    for exc in (
        EndpointError("x", status=500, body='{"error":"consent_required"}'),  # wrong status
        EndpointError("x", status=403, body='{"error":"forbidden"}'),  # different code
        EndpointError("x", status=403, body=None),  # no body
        EndpointError("x", status=403, body="not json at all"),  # malformed
        EndpointError("x", status=None, body=None),  # transport failure
    ):
        _raise_if_consent_required(exc)  # returns without raising


def test_consent_required_is_an_endpoint_error():
    # It subclasses EndpointError, so existing `except EndpointError` sites still catch it.
    err = ConsentRequiredError("x", server="s", status=403)
    assert isinstance(err, EndpointError)
    assert err.server == "s"
