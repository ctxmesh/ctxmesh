"""Request-scoped propagation of the record-mode capture toggle (M78, ADR 0071 §1).

When a run opts into RECORD mode, the BFF stamps ``X-Ctxmesh-Record: <runId>`` on the agent's
inbound ``/invoke`` (only for a recorded run against a record-capable agent). The SDK must relay it
on every outbound MODEL call so the launcher gateway captures that call's request+response into the
run's portable replay fixture (``internal/replay`` on the Go side). It is the exact sibling of the
run-capability relay (:mod:`ctxmesh._capability`): a launcher-internal, request-scoped header the
SDK forwards but never originates.

Held in a :class:`contextvars.ContextVar` (PEP 567), **not** a module global — each inbound request
runs in its own execution context, so concurrent runs on a warm pod never observe each other's
record toggle. There is no process-wide setter: the only way to bind it is :func:`record_scope`,
which is request-scoped and RESETS the ContextVar on exit. A non-recorded run binds ``None`` ⇒ the
model client relays no header ⇒ the gateway captures nothing for that run.
"""

from contextlib import contextmanager
from contextvars import ContextVar
from typing import Iterator, Mapping, Optional

# The header the BFF stamps and the launcher gateway reads — MUST match hdrRecord on the Go side
# (internal/bff/invoke.go) and recordHeaderName in the launcher gateway (cmd/launcher). Its value is
# the run id the fixture is keyed on. Case-insensitive on the wire.
RECORD_HEADER = "X-Ctxmesh-Record"

# The request-scoped record toggle. default=None ⇒ a non-recorded run: the model client relays no
# X-Ctxmesh-Record header and the gateway captures nothing.
_record_run_id: "ContextVar[Optional[str]]" = ContextVar("ctxmesh_record_run_id", default=None)


def current_record_run_id() -> Optional[str]:
    """Return the record-mode run id bound to the CURRENT request context, or ``None`` when the run
    is not being recorded."""
    return _record_run_id.get()


def _extract(headers: Optional[Mapping[str, str]]) -> Optional[str]:
    """Pull the record run id out of inbound *headers* case-insensitively (HTTP header case is not
    guaranteed), returning ``None`` when absent or blank."""
    if not headers:
        return None
    target = RECORD_HEADER.lower()
    for key, value in headers.items():
        if key.lower() == target:
            stripped = (value or "").strip()
            return stripped or None
    return None


@contextmanager
def record_scope(headers: Optional[Mapping[str, str]]) -> Iterator[None]:
    """Bind the record-mode run id extracted from inbound *headers* for the duration of the block,
    then reset it.

    Request-scoped (set on entry, RESET on exit) so a reused worker thread can never leak a prior
    request's record toggle. A missing/blank header binds ``None`` (a non-recorded run)."""
    token = _record_run_id.set(_extract(headers))
    try:
        yield
    finally:
        _record_run_id.reset(token)
