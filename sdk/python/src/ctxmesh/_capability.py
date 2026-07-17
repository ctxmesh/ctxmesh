"""Request-scoped propagation of the run capability (ADR 0030 §3).

The run capability is the unforgeable token the BFF mints at ``/invoke`` identifying the
INVOKING user (its ``sub`` is that user's hashed identity). It arrives on the inbound
request headers, and the SDK must relay it on every outbound MCP tool call so the egress
sidecar can resolve *that* user's credential — never another user's.

It is held in a :class:`contextvars.ContextVar` (PEP 567), **not** a module global. Each
inbound request runs in its own execution context — a fresh thread under the entrypoint's
``ThreadingHTTPServer``, or an ``asyncio`` task — so concurrent requests for different
users never observe each other's capability. There is no cross-user bleed by construction.

There is deliberately **no setter that mutates a process-wide value**: the only way to bind
a capability is :func:`capability_scope`, which is request-scoped and RESETS the ContextVar
on exit. :func:`current_capability` only ever reads the current context. This structural
absence of a global accessor is the no-bleed guarantee (a compromised/injected agent cannot
reach out and set another request's capability).
"""

from contextlib import contextmanager
from contextvars import ContextVar
from typing import Iterator, Mapping, Optional

# The header the BFF mints into and the launcher passes through — MUST match
# runcap.HeaderName on the Go side (internal/runcap). Case-insensitive on the wire.
CAPABILITY_HEADER = "X-Ctxmesh-Run-Capability"

# The request-scoped capability. default=None ⇒ a run with no capability (unattended /
# minting-disabled) resolves as org/public only, never another user's grant.
_run_capability: "ContextVar[Optional[str]]" = ContextVar("ctxmesh_run_capability", default=None)


def current_capability() -> Optional[str]:
    """Return the run capability bound to the CURRENT request context, or ``None``."""
    return _run_capability.get()


def _extract(headers: Optional[Mapping[str, str]]) -> Optional[str]:
    """Pull the capability out of inbound *headers* case-insensitively (HTTP header case
    is not guaranteed), returning ``None`` when absent or blank."""
    if not headers:
        return None
    target = CAPABILITY_HEADER.lower()
    for key, value in headers.items():
        if key.lower() == target:
            stripped = (value or "").strip()
            return stripped or None
    return None


@contextmanager
def capability_scope(headers: Optional[Mapping[str, str]]) -> Iterator[None]:
    """Bind the run capability extracted from inbound *headers* for the duration of the
    block, then reset it.

    Request-scoped: the ContextVar is set on entry and RESET on exit, so even if a worker
    thread is later reused for another request it can never leak a prior request's
    capability. A missing/blank header binds ``None`` (an unattended run).
    """
    token = _run_capability.set(_extract(headers))
    try:
        yield
    finally:
        _run_capability.reset(token)
