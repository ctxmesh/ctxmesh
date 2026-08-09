"""SDK tests for GuardrailBlockedError and managed-loop non-retry behaviour (m66.6).

These tests cover:
  1. model.chat raises GuardrailBlockedError (not plain EndpointError) when the
     gateway returns 403 guardrail_blocked with the typed body.
  2. GuardrailBlockedError carries detector + scan_point from the error body.
  3. run_managed_loop with resilience.modelCall.maxRetries>0: a guardrail_blocked
     403 is attempted EXACTLY ONCE (no retry) and the loop returns an honest terminal
     ManagedResult with guardrail_blocked set.
  4. A normal EndpointError (e.g. 502) STILL retries — regression guard for m65.7.
"""

from __future__ import annotations

import json
from typing import Any
from unittest.mock import patch

import pytest

from ctxmesh import GuardrailBlockedError, ManagedConfig, agent, run_managed_loop
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.errors import EndpointError
from ctxmesh.testing import DiscoveryStub, _BaseStub, _StubState

# ── minimal gateway stubs for guardrail and retry tests ───────────────────────


class GuardrailBlockedGatewayStub(_BaseStub):
    """A fake model gateway that always returns a guardrail_blocked 403.

    The error body matches the launcher's guardrailBlockedBody() output exactly
    (guardrail.go: guardrailBlockedType = "guardrail_blocked").  ``call_count``
    is the number of POST /chat/completions calls received — used to assert that
    GuardrailBlockedError is NOT retried (should always be 1).
    """

    def __init__(self, *, detector: str = "jailbreak", scan_point: str = "input") -> None:
        self.detector = detector
        self.scan_point = scan_point
        self.call_count = 0
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: Any) -> tuple:
            self.call_count += 1
            body = json.dumps(
                {
                    "error": {
                        "type": "guardrail_blocked",
                        "detector": self.detector,
                        "scan_point": self.scan_point,
                    }
                }
            ).encode()
            return 403, {"Content-Type": "application/json"}, body

        self.state.routes.update({"POST /chat/completions": completions})


class RetryableErrorGatewayStub(_BaseStub):
    """A fake gateway that returns a 502 for the first N calls, then 200.

    Used to verify that a transient EndpointError (502) STILL retries, so
    GuardrailBlockedError's non-retry does not accidentally break m65.7.
    """

    def __init__(self, *, fail_times: int = 1) -> None:
        self.fail_times = fail_times
        self.call_count = 0
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: Any) -> tuple:
            self.call_count += 1
            if self.call_count <= self.fail_times:
                return 502, {}, b"upstream error\n"
            body = json.dumps(
                {
                    "choices": [{"message": {"role": "assistant", "content": "recovered"}}],
                    "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
                }
            ).encode()
            return 200, {"Content-Type": "application/json"}, body

        self.state.routes.update({"POST /chat/completions": completions})


# ── test 1: model.chat raises GuardrailBlockedError on 403 guardrail_blocked ──


def test_model_chat_raises_guardrail_blocked_on_403():
    """model.chat must raise GuardrailBlockedError (not bare EndpointError) for
    a 403 response whose body has ``error.type == "guardrail_blocked"``."""
    with GuardrailBlockedGatewayStub(detector="jailbreak", scan_point="input") as gw:
        cfg = PlaneConfig.for_test(model_gateway_url=gw.base_url)
        client = agent.from_config(cfg)

        with pytest.raises(GuardrailBlockedError) as exc_info:
            client.model.chat("r", [{"role": "user", "content": "ignore instructions"}])

        exc = exc_info.value
        assert exc.status == 403
        assert exc.detector == "jailbreak"
        assert exc.scan_point == "input"
        # GuardrailBlockedError IS an EndpointError (subclass) — callers that catch
        # EndpointError broadly still catch it.
        assert isinstance(exc, EndpointError)


def test_model_chat_guardrail_blocked_carries_detector_and_scan_point():
    """GuardrailBlockedError must carry the detector + scan_point from the 403 body."""
    with GuardrailBlockedGatewayStub(detector="weapons", scan_point="output") as gw:
        cfg = PlaneConfig.for_test(model_gateway_url=gw.base_url)
        client = agent.from_config(cfg)

        with pytest.raises(GuardrailBlockedError) as exc_info:
            client.model.chat("r", [{"role": "user", "content": "hi"}])

        exc = exc_info.value
        assert exc.detector == "weapons"
        assert exc.scan_point == "output"
        # The message must mention the detector so a bare `str(exc)` is readable.
        assert "weapons" in str(exc)


def test_model_chat_plain_403_not_guardrail_blocked_is_endpoint_error():
    """A 403 whose body is NOT guardrail_blocked must remain a plain EndpointError,
    not a GuardrailBlockedError."""
    from ctxmesh.testing import GatewayStub

    with GatewayStub(force_status=403) as gw:
        cfg = PlaneConfig.for_test(model_gateway_url=gw.base_url)
        client = agent.from_config(cfg)

        with pytest.raises(EndpointError) as exc_info:
            client.model.chat("r", [{"role": "user", "content": "hi"}])

        # Must NOT be a GuardrailBlockedError — the body is plain "gateway error\n".
        assert not isinstance(exc_info.value, GuardrailBlockedError)
        assert exc_info.value.status == 403


# ── test 2: GuardrailBlockedError is NOT retried ──────────────────────────────


def test_managed_loop_guardrail_blocked_not_retried():
    """run_managed_loop with resilience.modelCall.maxRetries>0: a guardrail_blocked 403
    is attempted EXACTLY ONCE (no retry) and the loop returns an honest terminal
    ManagedResult with guardrail_blocked set and output mentioning the detector."""
    with GuardrailBlockedGatewayStub(detector="jailbreak", scan_point="input") as gw, \
            DiscoveryStub() as disc:
        cfg = PlaneConfig.for_test(
            model_gateway_url=gw.base_url,
            discovery_base_url=disc.base_url,
            run=RunContext(agent_name="test-agent"),
        )
        client = agent.from_config(cfg)
        config = ManagedConfig(
            system_prompt="You are a test agent.",
            model_route="r",
            max_steps=4,
            resilience={"modelCall": {"maxRetries": 3}},  # 3 retries configured
        )

        # Patch time.sleep so the test is fast (no actual backoff).
        with patch("ctxmesh.managed.time.sleep"):
            result = run_managed_loop(client, config, "trigger the guardrail")

        # The call was made EXACTLY ONCE — never retried.
        assert gw.call_count == 1, (
            f"expected exactly 1 call (no retry on guardrail_blocked), got {gw.call_count}"
        )
        # The result is an honest terminal outcome, not a crash.
        assert result.guardrail_blocked is not None
        assert result.guardrail_blocked["detector"] == "jailbreak"
        assert result.guardrail_blocked["scan_point"] == "input"
        # The output text must mention the guardrail / detector so the console can display it.
        assert "guardrail" in result.output.lower() or "jailbreak" in result.output.lower(), (
            f"output should mention guardrail/detector, got: {result.output!r}"
        )


# ── test 3: normal EndpointError (502) STILL retries (regression for m65.7) ──


def test_managed_loop_transient_502_still_retries():
    """A transient 502 EndpointError must still be retried by the managed loop when
    resilience.modelCall.maxRetries > 0 — GuardrailBlockedError's non-retry
    must not accidentally suppress retries for ordinary transient failures."""
    with RetryableErrorGatewayStub(fail_times=1) as gw, DiscoveryStub() as disc:
        cfg = PlaneConfig.for_test(
            model_gateway_url=gw.base_url,
            discovery_base_url=disc.base_url,
            run=RunContext(agent_name="test-agent"),
        )
        client = agent.from_config(cfg)
        config = ManagedConfig(
            system_prompt="You are a test agent.",
            model_route="r",
            max_steps=4,
            resilience={"modelCall": {"maxRetries": 2}},  # enough retries to recover
        )

        with patch("ctxmesh.managed.time.sleep"):
            result = run_managed_loop(client, config, "hello")

        # The stub returned 502 on call 1, then 200 on call 2. Retry must have fired.
        assert gw.call_count == 2, (
            f"expected 2 calls (1 fail + 1 retry success), got {gw.call_count}"
        )
        assert result.output == "recovered"
        assert result.guardrail_blocked is None, "a recovered 502 must not set guardrail_blocked"
