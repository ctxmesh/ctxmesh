"""Request-scoped human-in-the-loop approvals (ADR 0034 §HITL, m32.4).

A run can gate a sensitive step on human approval. The agent calls
:func:`pause_for_approval(key, summary)`; if ``key`` is in the set of approvals GRANTED for this
run, the call returns and the step proceeds — otherwise it raises
:class:`ctxmesh.errors.ApprovalRequiredError`, which the managed loop turns into a
``requires_action`` (approval) outcome. When the approver resolves it, the run is re-invoked with
the approved key in the granted set, so the same ``pause_for_approval`` call now proceeds.

The granted set is held in a :class:`contextvars.ContextVar` (PEP 567), **not** a module global —
the same no-cross-bleed guarantee as the run capability (:mod:`ctxmesh._capability`): each inbound
request runs in its own execution context, so concurrent runs never observe each other's approvals.
The only way to bind approvals is :func:`approval_scope`, which RESETS the ContextVar on exit; there
is no process-wide setter.
"""

from contextlib import contextmanager
from contextvars import ContextVar
from typing import FrozenSet, Iterable, Iterator, Mapping, Optional

from ctxmesh.errors import ApprovalRequiredError

# The approvals granted for the CURRENT run (a set of step keys). Empty ⇒ nothing approved yet, so
# every pause_for_approval raises.
_granted_approvals: "ContextVar[FrozenSet[str]]" = ContextVar(
    "ctxmesh_granted_approvals", default=frozenset()
)

# ── The stateless approval VOUCHER (ADR 0074 §3, m82.4) ──────────────────────────────────────────
#
# require-approval is enforced at the egress WIRE, not just here in the loop: a tool call for a
# require-approval tool is FORWARDED by the sidecar only when the request carries a valid, signed
# X-Ctxmesh-Approval voucher (a short-lived token bound to {runId, toolName} the BFF minted on a
# human's approval). The managed loop's pause_for_approval is now the PRESENTATION UX — necessary
# but no longer sufficient — and the SDK must RELAY the voucher on the tool-call retry so the
# sidecar forwards it.
#
# The voucher arrives on the RESUMED run's inbound /invoke headers (the BFF stamped
# X-Ctxmesh-Approval after approval-grant). We hold it request-scoped in a ContextVar and relay it
# on every outbound tool call — the EXACT sibling of the run-capability relay
# (:mod:`ctxmesh._capability`) and the record toggle (:mod:`ctxmesh._record`): a launcher-internal
# header the SDK forwards but never originates. The sidecar's {tool, run} binding means a voucher
# only unlocks the ONE approved tool; relaying it on every call is safe (a mismatched tool 403s).

# The header the BFF stamps on a resumed run and the SDK relays on each tool-call egress — must
# match runcap.ApprovalHeaderName on the Go side (internal/runcap) + hdrApproval (internal/bff).
# Case-insensitive on the wire.
APPROVAL_HEADER = "X-Ctxmesh-Approval"

# The request-scoped approval voucher. default=None ⇒ a run with no granted require-approval tool:
# the tool client relays no voucher and a require-approval tool gets the sidecar's 403.
_approval_voucher: "ContextVar[Optional[str]]" = ContextVar(
    "ctxmesh_approval_voucher", default=None
)


@contextmanager
def approval_scope(approvals: Optional[Iterable[str]]) -> Iterator[None]:
    """Bind the set of GRANTED approval keys for the duration of the block, then reset it.

    Request-scoped: set on entry, RESET on exit, so a reused worker thread can never leak a prior
    run's approvals. ``None`` binds the empty set (nothing approved).
    """
    token = _granted_approvals.set(frozenset(approvals or ()))
    try:
        yield
    finally:
        _granted_approvals.reset(token)


def pause_for_approval(key: str, summary: str) -> None:
    """Gate the current step on human approval (human-in-the-loop, m32.4).

    If ``key`` has been approved for this run (the approver resolved a prior pause), this returns
    and the step proceeds. Otherwise it raises :class:`~ctxmesh.errors.ApprovalRequiredError`, which
    the managed loop surfaces as a ``requires_action`` (approval) outcome carrying ``key`` +
    ``summary`` (a human-readable description of what needs approving). The run resumes on approval.

    ``key`` is a STABLE identifier for this decision point (e.g. ``"send-email"``) — the same key
    must be used across the initial call and the resumed re-invoke so the approval matches.
    """
    if key in _granted_approvals.get():
        return
    raise ApprovalRequiredError(
        f"approval required for {key!r}: {summary}", key=key, summary=summary
    )


def current_approval_voucher() -> Optional[str]:
    """Return the approval voucher bound to the CURRENT request context, or ``None``.

    The tool client relays this on each outbound MCP tool call (the egress sidecar verifies it for a
    require-approval tool). ``None`` outside a resumed/approved run — the sidecar then returns its
    403 ``approval_required`` for a require-approval tool.
    """
    return _approval_voucher.get()


def _extract_voucher(headers: Optional[Mapping[str, str]]) -> Optional[str]:
    """Pull the approval voucher out of inbound *headers* case-insensitively (HTTP header case is
    not guaranteed), returning ``None`` when absent or blank."""
    if not headers:
        return None
    target = APPROVAL_HEADER.lower()
    for key, value in headers.items():
        if key.lower() == target:
            stripped = (value or "").strip()
            return stripped or None
    return None


@contextmanager
def voucher_scope(headers: Optional[Mapping[str, str]]) -> Iterator[None]:
    """Bind the approval voucher extracted from inbound *headers* for the duration of the block,
    then reset it.

    Request-scoped (set on entry, RESET on exit) so a reused worker thread can never leak a prior
    request's voucher. A missing/blank header binds ``None`` (no granted require-approval tool)."""
    token = _approval_voucher.set(_extract_voucher(headers))
    try:
        yield
    finally:
        _approval_voucher.reset(token)
