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
from typing import FrozenSet, Iterable, Iterator, Optional

from ctxmesh.errors import ApprovalRequiredError

# The approvals granted for the CURRENT run (a set of step keys). Empty ⇒ nothing approved yet, so
# every pause_for_approval raises.
_granted_approvals: "ContextVar[FrozenSet[str]]" = ContextVar(
    "ctxmesh_granted_approvals", default=frozenset()
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
