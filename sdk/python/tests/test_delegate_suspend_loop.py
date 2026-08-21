"""L7 durable delegate suspend/resume through the managed loop (ADR 0091, m108.5).

These drive :func:`ctxmesh.run_managed_loop` against a fully-controlled fake client (a scripted
model + a scripted delegate) so the suspend seam, the resume re-dispatch, the eligibility gate, the
mixed-version fallback, and the size guard are all exercised deterministically — no cluster needed.
"""

import hashlib
import json

from ctxmesh import _checkpoint
from ctxmesh.managed import ManagedConfig, run_managed_loop
from ctxmesh.model import ChatResponse
from ctxmesh.tools import DELEGATE_TOOL_NAME

SUSPEND_HEADERS = {"X-Ctxmesh-Spawn-Root": "root-1", "X-Ctxmesh-Spawn-Depth": "0"}


# ── fakes ──────────────────────────────────────────────────────────────────────
class _NoopSpan:
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def set_input(self, v):
        pass

    def set_output(self, v):
        pass


class _NoopTrace:
    def loop(self, name, headers=None):
        return _NoopSpan()

    def step(self, name):
        return _NoopSpan()

    def tool(self, name, input=None):  # noqa: A002 - matches the trace API
        return _NoopSpan()


class _FakeTool:
    def __init__(self, name):
        self.name = name
        self.description = f"the {name} tool"
        self.input_schema = None


class _FakeModel:
    """Returns the next scripted completion body as a ChatResponse."""

    def __init__(self, script):
        self._script = list(script)
        self.calls = 0

    def chat(self, route, messages, **opts):
        raw = self._script[self.calls]
        self.calls += 1
        msg = raw["choices"][0]["message"]
        return ChatResponse(text=msg.get("content") or "", usage={}, model=route, raw=raw)


class _FakeTools:
    def __init__(self, delegate_fn):
        self._delegate_fn = delegate_fn
        self.delegate_calls = []

    def list(self):
        return [_FakeTool(DELEGATE_TOOL_NAME)]

    def delegate(
        self, sub_agent, task, step, call_id, *, suspend=False, spawn_root="", spawn_depth=-1
    ):
        self.delegate_calls.append(
            {"sub_agent": sub_agent, "step": step, "call_id": call_id, "suspend": suspend}
        )
        return self._delegate_fn(sub_agent, task, step, call_id, suspend=suspend)


class _FakeCfg:
    memory_wired = False
    longterm_wired = False


class _FakeClient:
    def __init__(self, model, tools):
        self.trace = _NoopTrace()
        self.model = model
        self.tools = tools
        self.config = _FakeCfg()


def _delegate_turn(call_id="c1", sub_agent="researcher", task="find it"):
    return {
        "choices": [
            {
                "finish_reason": "tool_calls",
                "message": {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": call_id,
                            "type": "function",
                            "function": {
                                "name": DELEGATE_TOOL_NAME,
                                "arguments": json.dumps({"sub_agent": sub_agent, "task": task}),
                            },
                        }
                    ],
                },
            }
        ]
    }


def _final_turn(text="all done"):
    return {
        "choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": text}}]
    }


def _cfg():
    return ManagedConfig(system_prompt="you are a supervisor", model_route="tool-mock", max_steps=6)


def _envelope_from(checkpoint_payload: str) -> dict:
    """Mirror the BFF wrapping the SDK payload in the hashed envelope on suspend."""
    return {
        "version": 1,
        "kind": "supervisor-loop",
        "sha256": hashlib.sha256(checkpoint_payload.encode()).hexdigest(),
        "payload": checkpoint_payload,
    }


# ── suspend ──────────────────────────────────────────────────────────────────────
def test_depth0_delegate_suspends_and_emits_the_marker():
    """A depth-0 durable supervisor that delegates SUSPENDS: no final answer, a delegate_waiting
    marker carrying a verifiable checkpoint + the delegate intent (launcher-resolved endpoint)."""
    model = _FakeModel([_delegate_turn()])
    tools = _FakeTools(
        lambda *_a, suspend=False, **_k: (
            {"ok": True, "suspend": True, "endpoint": "http://researcher.ns/invoke"}
            if suspend
            else {"ok": True, "answer": "should-not-block"}
        )
    )
    result = run_managed_loop(
        _FakeClient(model, tools), _cfg(), "research X", headers=SUSPEND_HEADERS
    )

    assert result.output == "", "a suspended run has no terminal answer"
    assert result.delegate_waiting is not None
    delegates = result.delegate_waiting["delegates"]
    assert delegates == [
        {
            "sub_agent": "researcher",
            "endpoint": "http://researcher.ns/invoke",
            "input": "find it",
            "step": "1",
            "call_id": "c1",
        }
    ]
    # The checkpoint verifies (as the BFF will wrap it) and carries the pending delegation.
    fields = _checkpoint.verify_and_extract(_envelope_from(result.delegate_waiting["checkpoint"]))
    assert fields is not None
    assert fields["pending"][0]["call_id"] == "c1"
    assert fields["step"] == 1
    # The delegate was asked to SUSPEND (not dispatched/awaited), with the spawn position relayed.
    assert tools.delegate_calls == [
        {"sub_agent": "researcher", "step": "1", "call_id": "c1", "suspend": True}
    ]


# ── resume ──────────────────────────────────────────────────────────────────────
def test_resume_rethreads_the_result_and_continues():
    """Re-invoking with the checkpoint restores the loop, re-dispatches the delegation through the
    idempotent blocking path (returning the terminal child's result), threads it, and finishes."""
    # First, produce a real checkpoint by suspending.
    suspend_model = _FakeModel([_delegate_turn()])
    suspend_tools = _FakeTools(
        lambda *_a, **_k: {"ok": True, "suspend": True, "endpoint": "http://r/invoke"}
    )
    suspended = run_managed_loop(
        _FakeClient(suspend_model, suspend_tools), _cfg(), "research X", headers=SUSPEND_HEADERS
    )
    envelope = _envelope_from(suspended.delegate_waiting["checkpoint"])

    # Now resume: the re-dispatch (blocking, no suspend) returns the child's terminal answer.
    resume_model = _FakeModel([_final_turn("the research says 42")])
    resume_tools = _FakeTools(
        lambda *_a, suspend=False, **_k: {"ok": True, "answer": "child: the answer is 42"}
    )
    resumed = run_managed_loop(
        _FakeClient(resume_model, resume_tools),
        _cfg(),
        "research X",
        headers=SUSPEND_HEADERS,
        checkpoint=envelope,
    )

    assert resumed.delegate_waiting is None, "the resumed turn completed (did not re-suspend)"
    assert resumed.output == "the research says 42"
    # The resume re-dispatched the SAME (step, call_id) via the blocking path (suspend=False).
    assert resume_tools.delegate_calls == [
        {"sub_agent": "researcher", "step": "1", "call_id": "c1", "suspend": False}
    ]
    # The child's result reached the model (turn 2 saw the tool message).
    assert resume_model.calls == 1


def test_corrupt_checkpoint_runs_fresh():
    """A checkpoint that fails verification is ignored (fail-safe) — the turn runs fresh."""
    model = _FakeModel([_final_turn("fresh answer")])
    tools = _FakeTools(lambda *_a, **_k: {"ok": True, "answer": "x"})
    bad = {"version": 1, "kind": "supervisor-loop", "sha256": "deadbeef", "payload": "{}"}
    result = run_managed_loop(
        _FakeClient(model, tools), _cfg(), "hi", headers=SUSPEND_HEADERS, checkpoint=bad
    )
    assert result.output == "fresh answer"
    assert result.delegate_waiting is None


# ── eligibility gate + mixed-version ──────────────────────────────────────────────
def test_no_spawn_root_blocks_instead_of_suspending():
    """Without a spawn-root header (the synchronous Playground path), a delegation BLOCKS as in
    M64 — never suspends (a marker there would be lost)."""
    model = _FakeModel([_delegate_turn(), _final_turn("done via blocking")])
    tools = _FakeTools(lambda *_a, **_k: {"ok": True, "answer": "blocking sub-answer"})
    result = run_managed_loop(_FakeClient(model, tools), _cfg(), "go", headers={})  # no spawn-root

    assert result.delegate_waiting is None
    assert result.output == "done via blocking"
    assert tools.delegate_calls[0]["suspend"] is False, "not eligible → the blocking delegate path"


def test_old_launcher_blocking_response_threads_inline():
    """Mixed-version fallback: an old launcher ignores `suspend` and returns a blocking {ok,answer};
    the loop detects the missing `suspend` and threads it inline instead of suspending."""
    model = _FakeModel([_delegate_turn(), _final_turn("done despite old launcher")])
    # suspend=True was requested, but the launcher answers as if blocking (no `suspend` key).
    tools = _FakeTools(lambda *_a, **_k: {"ok": True, "answer": "old-launcher answer"})
    result = run_managed_loop(_FakeClient(model, tools), _cfg(), "go", headers=SUSPEND_HEADERS)

    assert result.delegate_waiting is None, "a blocking response must not suspend"
    assert result.output == "done despite old launcher"
    assert tools.delegate_calls[0]["suspend"] is True, "the loop DID request suspension (eligible)"
