"""L7 checkpoint envelope round-trip + fail-safe verification (ADR 0091, m108.5).

The SDK builds the opaque PAYLOAD; the BFF wraps it in a hashed ENVELOPE
(internal/run/checkpoint.go, NewSupervisorCheckpoint). These tests mirror that wrap in Python and
prove verify_and_extract accepts a good envelope and rejects — never raises on — every corrupt form.
"""

import hashlib
import json

from ctxmesh import _checkpoint


def _wrap(payload: str) -> dict:
    """Mirror the BFF's NewSupervisorCheckpoint envelope (internal/run/checkpoint.go:54)."""
    return {
        "version": 1,
        "kind": "supervisor-loop",
        "sha256": hashlib.sha256(payload.encode()).hexdigest(),
        "payload": payload,
    }


def _payload(**overrides) -> str:
    base = {
        "messages": [{"role": "user", "content": "hi"}],
        "step": 3,
        "pending": [{"call_id": "c1", "step": "3", "sub_agent": "researcher", "task": "find it"}],
        "tools_called": ["delegate_to"],
        "consent_required": [],
        "spotlight_token": "tok-123",
        "model_index": 2,
        "tool_index": 1,
    }
    base.update(overrides)
    return _checkpoint.build_payload(**base)


def test_payload_round_trips_through_the_envelope():
    fields = _checkpoint.verify_and_extract(_wrap(_payload()))
    assert fields is not None
    assert fields["v"] == _checkpoint.PAYLOAD_VERSION
    assert fields["step"] == 3
    assert fields["spotlight_token"] == "tok-123"
    assert fields["pending"][0]["call_id"] == "c1"
    assert fields["pending"][0]["task"] == "find it"
    assert fields["model_index"] == 2 and fields["tool_index"] == 1
    assert fields["tools_called"] == ["delegate_to"]


def test_json_string_envelope_is_accepted():
    # The worker injects a raw JSON object, but a stringified envelope is tolerated defensively.
    assert _checkpoint.verify_and_extract(json.dumps(_wrap(_payload()))) is not None


def test_verify_is_fail_safe_on_every_bad_envelope():
    good = _wrap(_payload())
    assert _checkpoint.verify_and_extract(None) is None, "absent → None"
    assert _checkpoint.verify_and_extract("not json at all") is None, "non-JSON string → None"
    assert _checkpoint.verify_and_extract(42) is None, "non-dict → None"

    wrong_kind = dict(good, kind="workflow-cursor")
    assert (
        _checkpoint.verify_and_extract(wrong_kind) is None
    ), "a workflow cursor is not a supervisor loop"

    wrong_version = dict(good, version=2)
    assert (
        _checkpoint.verify_and_extract(wrong_version) is None
    ), "unknown envelope version → fail-safe"

    corrupt = dict(good, payload=good["payload"] + " tampered")
    assert (
        _checkpoint.verify_and_extract(corrupt) is None
    ), "a hash mismatch (corruption) → fail-safe"

    no_payload = {"version": 1, "kind": "supervisor-loop", "sha256": "x"}
    assert _checkpoint.verify_and_extract(no_payload) is None, "a missing payload → None"


def test_unknown_payload_version_is_rejected():
    # The payload carries its OWN version, evolvable independently of the envelope.
    payload = json.dumps({"v": 999, "messages": []})
    assert _checkpoint.verify_and_extract(_wrap(payload)) is None


def test_checkpoint_size_cap_is_a_sane_guard():
    # The cap must sit safely under the BFF's 4 MiB /invoke response LimitReader.
    assert 0 < _checkpoint.CHECKPOINT_MAX_BYTES < 4 * 1024 * 1024
