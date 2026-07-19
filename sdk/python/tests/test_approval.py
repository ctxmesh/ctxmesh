"""Human-in-the-loop approval primitive (ADR 0034 §HITL, m32.4).

pause_for_approval gates a step on a human decision: it raises ApprovalRequiredError when the step's
key is not in the run's GRANTED approvals (bound by approval_scope), and returns silently once it is
— which is exactly what happens on a resume, when the BFF re-invokes with the approved key. The
granted set is a request-scoped ContextVar, so concurrent runs never see each other's approvals.
"""

from __future__ import annotations

import pytest

from ctxmesh import pause_for_approval
from ctxmesh._approval import approval_scope
from ctxmesh.errors import ApprovalRequiredError


def test_pauses_when_not_approved():
    with pytest.raises(ApprovalRequiredError) as excinfo:
        pause_for_approval("send-email", "Send the email to the customer?")
    assert excinfo.value.key == "send-email"
    assert excinfo.value.summary == "Send the email to the customer?"


def test_proceeds_when_key_is_granted():
    # Inside a scope that granted this key (a resume), the same call returns silently.
    with approval_scope(["send-email"]):
        pause_for_approval("send-email", "Send the email to the customer?")  # no raise


def test_a_different_granted_key_does_not_unlock():
    with approval_scope(["something-else"]):
        with pytest.raises(ApprovalRequiredError):
            pause_for_approval("send-email", "Send the email?")


def test_scope_resets_on_exit():
    with approval_scope(["send-email"]):
        pause_for_approval("send-email", "x")  # granted here
    # Outside the scope the grant is gone (request-scoped, no leak).
    with pytest.raises(ApprovalRequiredError):
        pause_for_approval("send-email", "x")


def test_multiple_keys_in_one_scope():
    with approval_scope(["a", "b"]):
        pause_for_approval("a", "A?")
        pause_for_approval("b", "B?")
        with pytest.raises(ApprovalRequiredError):
            pause_for_approval("c", "C?")


def test_approval_required_error_is_ctxmesh_error():
    from ctxmesh import CtxmeshError

    err = ApprovalRequiredError("x", key="k", summary="s")
    assert isinstance(err, CtxmeshError)
    assert err.key == "k"
    assert err.summary == "s"
