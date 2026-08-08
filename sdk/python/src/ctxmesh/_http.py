"""Minimal stdlib HTTP helper shared by the plane clients.

The m10.2 clients depend on nothing but the standard library (``urllib``) so the
SDK stays lean enough to bundle into base-python and runs on the 3.9 target.
This module centralises the request/response plumbing and — critically — the
error translation: a non-2xx response or a transport failure becomes an
:class:`~ctxmesh.errors.EndpointError` carrying the status, never a swallowed
error (spec: "surface the launcher endpoint's error rather than swallowing").

307/308 redirect handling
-------------------------
``urllib`` does not re-POST a body on a 307 response — it raises
``HTTPError 307``. Our MCP endpoint is registered at ``/mcp/`` (trailing slash)
and FastMCP/Starlette issues a 307 for ``POST /mcp`` (without the slash).  The
:func:`request` helper therefore catches 307/308 explicitly and re-issues the
**same method, body, and headers** to the ``Location`` URL.  Two guards apply:

* **Same-origin only** — the redirect target must share scheme, host, and port
  with the original URL.  Re-POSTing a body to a different host is a security
  boundary violation; we raise :class:`~ctxmesh.errors.EndpointError` instead.
* **Bounded** — at most ``_MAX_REDIRECTS`` consecutive 307/308 hops before we
  raise, preventing infinite redirect loops.

301/302/303 are left to urllib's default handling (it converts the method to GET,
which is correct for those status codes and not expected on the localhost plane).
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, Iterator, Optional, Tuple

from ctxmesh.errors import EndpointError

#: Default per-op timeout (seconds). Matches the launcher's own 2s Valkey bound
#: for the memory path; callers may override for slower endpoints.
DEFAULT_TIMEOUT = 5.0

#: SSE read timeout (seconds, FUNC-7). A long-lived event stream (runs.stream) may idle
#: between events for a slow model turn or a run parked at requires_action — far longer
#: than DEFAULT_TIMEOUT. The server heartbeats well within this window, so a live-but-idle
#: stream never trips it; a genuinely dead connection still errors after it (not never).
STREAM_READ_TIMEOUT = 120.0

#: Maximum number of 307/308 hops before we give up to prevent infinite loops.
_MAX_REDIRECTS = 3


def _origin(url: str) -> Tuple[str, str, int]:
    """Return (scheme, host, port) for *url*, normalising the default port."""
    parsed = urllib.parse.urlparse(url)
    scheme = parsed.scheme.lower()
    host = parsed.hostname or ""
    # urllib.parse returns None for the default port; normalise to the scheme's.
    if parsed.port is not None:
        port = parsed.port
    elif scheme == "https":
        port = 443
    else:
        port = 80
    return scheme, host, port


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

    307/308 redirects are followed by re-issuing the **same method and body**
    to the ``Location`` URL.  Cross-origin redirects and redirect loops (more
    than ``_MAX_REDIRECTS`` hops) are refused with an :class:`EndpointError`.

    Raises :class:`EndpointError` when:
      * the transport fails (connection refused / timeout) — ``status`` is None; or
      * a 307/308 redirect is cross-origin or exceeds the hop limit; or
      * ``expect`` is given and the response status is not in it — ``status`` is
        the actual code so the caller can distinguish 400 from 502.
    """
    original_origin = _origin(url)
    current_url = url
    hops = 0

    while True:
        req = urllib.request.Request(current_url, data=body, method=method)  # noqa: S310
        for key, value in (headers or {}).items():
            req.add_header(key, value)

        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
                resp_headers = {k.lower(): v for k, v in resp.headers.items()}
                response = Response(resp.status, resp_headers, resp.read())
        except urllib.error.HTTPError as exc:
            if exc.code in (307, 308):
                # urllib raises HTTPError for 307/308 instead of following the
                # redirect.  Re-issue the same method and body to the Location.
                location = exc.headers.get("Location") or exc.headers.get("location")
                if not location:
                    raise EndpointError(
                        f"{method} {current_url} returned {exc.code} with no Location header",
                        status=exc.code,
                    ) from exc
                # Resolve relative Location against the current URL.
                redirect_url = urllib.parse.urljoin(current_url, location)
                if _origin(redirect_url) != original_origin:
                    raise EndpointError(
                        f"{method} {current_url} returned {exc.code} to a "
                        f"cross-origin location {redirect_url!r}; refusing to "
                        f"re-POST body to a different host (security boundary)",
                        status=exc.code,
                    ) from exc
                hops += 1
                if hops > _MAX_REDIRECTS:
                    raise EndpointError(
                        f"{method} {url} exceeded {_MAX_REDIRECTS} redirects "
                        f"(last location: {redirect_url!r})",
                        status=exc.code,
                    ) from exc
                current_url = redirect_url
                continue

            # A non-2xx with a response body (e.g. feedback 400/502). Read the
            # body so the caller can see the endpoint's error message, and
            # surface the status — never swallow it.
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

        break  # 2xx (or unexpected status) — exit the redirect loop

    if expect is not None and response.status not in expect:
        raise EndpointError(
            f"{method} {url} returned {response.status} "
            f"(expected {', '.join(str(s) for s in expect)}): {response.text().strip()}",
            status=response.status,
            body=response.text(),
        )
    return response


def stream(
    method: str,
    url: str,
    *,
    body: Optional[bytes] = None,
    headers: Optional[Dict[str, str]] = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> Iterator[str]:
    """POST and yield the response body LINE BY LINE (for Server-Sent Events).

    Unlike :func:`request` this does NOT buffer the whole body — it streams, so a token
    stream is delivered as it arrives. No redirect following (streaming endpoints are
    same-origin: the model gateway, the agent's own /invoke). Raises :class:`EndpointError`
    on a non-2xx (with ``status`` + the error body) or a transport failure (``status`` None) —
    never swallows the failure.
    """
    req = urllib.request.Request(url, data=body, method=method)  # noqa: S310
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)  # noqa: S310
    except urllib.error.HTTPError as exc:
        err_body = exc.read().decode("utf-8", errors="replace") if exc.fp else ""
        raise EndpointError(
            f"{method} {url} returned {exc.code}: {err_body.strip()}",
            status=exc.code,
            body=err_body,
        ) from exc
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        raise EndpointError(f"{method} {url} failed: {exc}", status=None) from exc

    # The read loop is INSIDE its own try/except (FUNC-7): urllib's `timeout` is a
    # per-socket-op timeout, so a mid-stream gap between SSE events raised a RAW
    # socket TimeoutError here — outside the connect try/except above — killing any
    # stream with a >timeout idle gap (a slow model turn, or a run parked at
    # requires_action). Translate it (and any transport error) to EndpointError so the
    # caller sees the SDK's typed error, never a bare TimeoutError. SSE callers pass a
    # long read timeout (STREAM_READ_TIMEOUT) and the server heartbeats within it.
    try:
        with resp:
            for raw in resp:
                yield raw.decode("utf-8", errors="replace").rstrip("\r\n")
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        raise EndpointError(f"{method} {url} stream interrupted: {exc}", status=None) from exc


def json_body(value: Any) -> bytes:
    """Serialise a value as compact JSON bytes (matches the launcher contract)."""
    return json.dumps(value, separators=(",", ":")).encode("utf-8")
