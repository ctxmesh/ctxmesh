"""L7 supervisor-loop checkpoint (ADR 0091) — the SDK side of the durable delegate suspend/resume.

A depth-0 durable supervisor that delegates and suspends serializes its managed-loop state into an
opaque **payload** string (built here) and returns it in the ``delegate_waiting`` marker. The BFF
wraps that payload in a hashed **envelope** (``internal/run/checkpoint.go``) and stores it in the
run's cursor. On resume the worker injects the ENVELOPE back into the invoke body as
``body["checkpoint"]``; this module re-verifies it (kind / version / hash — defense in depth; the
worker already verified before injecting) and extracts the payload's fields.

The payload carries ONLY loop state — never authorization (consent/approval/OBO), which is re-derived
server-side on resume (ADR 0091 fork 3). Verification is fail-safe: a corrupt / version-skewed
envelope or payload yields ``None`` and the caller runs fresh from the request input.
"""

from __future__ import annotations

import hashlib
import json
import logging
from typing import Any, Dict, Optional

_log = logging.getLogger("ctxmesh.checkpoint")

#: The envelope contract shared with the Go store (internal/run/checkpoint.go). Kept in lockstep: a
#: bump must roll the SDKs first (a resume rejects an unknown version → fail-safe full re-invoke).
_ENVELOPE_KIND = "supervisor-loop"
_ENVELOPE_VERSION = 1

#: The payload's OWN schema version (independent of the envelope — lets the SDK evolve the loop-state
#: shape without touching the store's envelope contract). A resume rejects an unknown payload version.
PAYLOAD_VERSION = 1

#: Cap on the serialized payload size. The ``delegate_waiting`` marker rides the /invoke response,
#: which the BFF reads through a 4 MiB LimitReader (internal/bff/invoke.go) — an oversized marker is
#: silently TRUNCATED, so the loop measures the payload and falls back to blocking dispatch above this
#: threshold (a loud, graceful M64 degradation) rather than emitting a truncated, unparseable marker.
CHECKPOINT_MAX_BYTES = 1_500_000


def build_payload(
    *,
    messages: list,
    step: int,
    pending: list,
    tools_called: list,
    consent_required: list,
    spotlight_token: str,
    model_index: int,
    tool_index: int,
) -> str:
    """Serialize the resumable managed-loop state into the opaque payload string.

    Only the fields that cannot be reconstructed on resume are persisted; everything re-derivable
    (tool manifest, history/memory/knowledge, conversation/message ids, approvals, the per-run
    breaker) is intentionally omitted and rebuilt from headers/body/store on resume.
    """
    payload: Dict[str, Any] = {
        "v": PAYLOAD_VERSION,
        "messages": messages,
        "step": step,
        "pending": pending,
        "tools_called": tools_called,
        "consent_required": consent_required,
        "spotlight_token": spotlight_token,
        "model_index": model_index,
        "tool_index": tool_index,
    }
    return json.dumps(payload, separators=(",", ":"))


def verify_and_extract(envelope: Any) -> Optional[Dict[str, Any]]:
    """Verify a checkpoint ENVELOPE and return its payload fields, or ``None`` (fail-safe).

    *envelope* is ``body["checkpoint"]`` as parsed from the invoke body — the worker injects the Go
    envelope as a raw JSON object, so it normally arrives as a ``dict`` (a JSON string is tolerated
    too). ``None`` is returned — never raised — for any envelope that is absent, malformed, of an
    unknown kind/version, whose payload hash does not match, or whose payload is not a known version;
    the caller then runs the turn fresh from the request input (the SDK mirror of the worker's
    full-re-invoke fail-safe).
    """
    if envelope is None:
        return None
    env = envelope
    if isinstance(env, (str, bytes)):
        try:
            env = json.loads(env)
        except (json.JSONDecodeError, ValueError):
            _log.warning("L7 checkpoint: envelope is not valid JSON — running fresh (fail-safe)")
            return None
    if not isinstance(env, dict):
        return None
    if env.get("kind") != _ENVELOPE_KIND or env.get("version") != _ENVELOPE_VERSION:
        _log.warning("L7 checkpoint: unrecognized envelope kind/version — running fresh (fail-safe)")
        return None
    payload_str = env.get("payload")
    if not isinstance(payload_str, str):
        return None
    digest = hashlib.sha256(payload_str.encode()).hexdigest()
    if digest != env.get("sha256"):
        _log.warning("L7 checkpoint: payload hash mismatch (corruption) — running fresh (fail-safe)")
        return None
    try:
        fields = json.loads(payload_str)
    except (json.JSONDecodeError, ValueError):
        return None
    if not isinstance(fields, dict) or fields.get("v") != PAYLOAD_VERSION:
        _log.warning("L7 checkpoint: unknown payload version — running fresh (fail-safe)")
        return None
    return fields
