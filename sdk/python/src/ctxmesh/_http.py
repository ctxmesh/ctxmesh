"""Minimal stdlib HTTP helper shared by the plane clients.

The m10.2 clients depend on nothing but the standard library (``urllib``) so the
SDK stays lean enough to bundle into base-python and runs on the 3.9 target.
This module centralises the request/response plumbing and — critically — the
error translation: a non-2xx response or a transport failure becomes an
:class:`~ctxmesh.errors.EndpointError` carrying the status, never a swallowed
error (spec: "surface the launcher endpoint's error rather than swallowing").
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from typing import Any, Dict, Optional, Tuple

from ctxmesh.errors import EndpointError

#: Default per-op timeout (seconds). Matches the launcher's own 2s Valkey bound
#: for the memory path; callers may override for slower endpoints.
DEFAULT_TIMEOUT = 5.0


class Response:
    """A thin view over an HTTP response the clients care about."""

    __slots__ = ("status", "headers", "body")

    def __init__(self, status: int, headers: Dict[str, str], body: bytes):
        self.status = status
        self.headers = headers
        self.body = body

    def json(self) -> Any:
        """Decode the body as JSON, or raise EndpointError on malformed JSON."""
        if not self.body:
            return None
        try:
            return json.loads(self.body)
        except json.JSONDecodeError as exc:
            raise EndpointError(
                f"endpoint returned malformed JSON: {exc}", status=self.status
            ) from exc

    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")


def request(
    method: str,
    url: str,
    *,
    body: Optional[bytes] = None,
    headers: Optional[Dict[str, str]] = None,
    timeout: float = DEFAULT_TIMEOUT,
    expect: Optional[Tuple[int, ...]] = None,
) -> Response:
    """Perform an HTTP request and return a :class:`Response`.

    Raises :class:`EndpointError` when:
      * the transport fails (connection refused / timeout) — ``status`` is None; or
      * ``expect`` is given and the response status is not in it — ``status`` is
        the actual code so the caller can distinguish 400 from 502.
    """
    req = urllib.request.Request(url, data=body, method=method)  # noqa: S310 (localhost plane)
    for key, value in (headers or {}).items():
        req.add_header(key, value)

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
            resp_headers = {k.lower(): v for k, v in resp.headers.items()}
            response = Response(resp.status, resp_headers, resp.read())
    except urllib.error.HTTPError as exc:
        # A non-2xx with a response body (e.g. feedback 400/502). Read the body
        # so the caller can see the endpoint's error message, and surface the
        # status — never swallow it.
        err_body = exc.read().decode("utf-8", errors="replace") if exc.fp else ""
        raise EndpointError(
            f"{method} {url} returned {exc.code}: {err_body.strip()}",
            status=exc.code,
            body=err_body,
        ) from exc
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        # Transport-level failure: no HTTP status. Includes connection refused
        # (endpoint not wired / down) and timeouts.
        raise EndpointError(f"{method} {url} failed: {exc}", status=None) from exc

    if expect is not None and response.status not in expect:
        raise EndpointError(
            f"{method} {url} returned {response.status} "
            f"(expected {', '.join(str(s) for s in expect)}): {response.text().strip()}",
            status=response.status,
            body=response.text(),
        )
    return response


def json_body(value: Any) -> bytes:
    """Serialise a value as compact JSON bytes (matches the launcher contract)."""
    return json.dumps(value, separators=(",", ":")).encode("utf-8")
