"""``client.mesh`` — synchronous agent-to-agent calls through the launcher (M6, M156).

WHY THIS IS CALLED ``mesh``
---------------------------
The call surface is **AMP** (ADR 0138) and the boundary it runs inside is the **mesh** — the
closed communication scope of an ``AgentRegistry``. ``client.mesh`` is operations over that
boundary, which is why the API keeps this name while the protocol got its own.

AMP is not Google's Agent2Agent, which the platform's old name (A2A, since M6) collided with.
Theirs is an *interop* protocol for agents from different organisations — Agent Cards,
JSON-RPC, a task lifecycle, declared security schemes. Ours is *mediation* for agents the
platform already owns: the launcher stamps an envelope carrying hop depth, the traversal path
and a spend budget, so it can enforce registry isolation, cycle detection and fan-out limits.
Neither is a worse version of the other; they solve different problems.

WHAT THE LAUNCHER DOES FOR YOU
------------------------------
Your payload is opaque. The launcher stamps the platform envelope, resolves the target over
DNS, injects W3C ``traceparent`` so the callee's spans join YOUR trace, and enforces
registry isolation plus the callee's ``allowedCallers`` before the request reaches it. A
blocked, unknown or cross-registry target fails fast and typed — it never hangs.

Calling a peer's URL directly instead of using this loses all of that: the envelope, the
access control, and the joined trace. It is not an error, which is what makes it dangerous.
"""

from __future__ import annotations

from typing import Any, Dict, Optional

from ctxmesh import _http
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError

#: Bound on a target agent name, matching the launcher's own validation. A name is a DNS
#: label the launcher resolves; anything longer is a client-side error, never a request.
_MAX_TARGET = 253


class MeshClient:
    """Agent-to-agent calls, mediated by this agent's own launcher."""

    def __init__(self, config: PlaneConfig) -> None:
        self._config = config

    def _require_wired(self) -> None:
        # The launcher starts the :2997 listener ONLY for a resolved AgentRegistry member.
        # An agent that is not in a registry has no peers by construction, so this is a
        # configuration answer, not a runtime failure — say so plainly rather than letting
        # the caller meet a connection-refused on a port they never heard of.
        if not self._config.mesh_wired:
            raise ConfigError(
                "the mesh is not wired for this agent: the launcher starts its AMP listener "
                "only for a resolved AgentRegistry member (AGENT_REGISTRY_ID). Add this "
                "agent to an AgentRegistry to call peers."
            )

    def call(
        self,
        target: str,
        payload: Optional[Dict[str, Any]] = None,
        *,
        timeout: Optional[float] = None,
    ) -> Dict[str, Any]:
        """Call *target* in this agent's registry and return its response.

        *payload* is yours and opaque to the platform — the launcher wraps it in the
        envelope rather than inspecting it.

        Raises :class:`ConfigError` when the mesh is not wired or *target* is malformed, and
        :class:`EndpointError` (carrying ``.status``) when the launcher refuses the hop: 403
        for a disallowed caller or a cross-registry target, 404 for an unknown one, 502 for a
        blocked or failed peer. A refusal is an OUTCOME, delivered typed — the launcher fails
        fast precisely so a governed mesh never presents as a hang.
        """
        self._require_wired()
        name = (target or "").strip()
        if not name:
            raise ConfigError("a target agent name is required")
        if len(name) > _MAX_TARGET:
            raise ConfigError(f"target agent name too long (max {_MAX_TARGET})")
        if "/" in name or ".." in name:
            # Defence in depth on top of the launcher's own validation: a name is a path
            # segment, and a segment that can escape its position is never legitimate.
            raise ConfigError(f"invalid target agent name: {name!r}")

        resp = _http.request(
            "POST",
            # Deliberately the legacy path. The SDK ships in the customer's image and
            # can be upgraded BEFORE the platform, so posting /amp/ would 404 against a
            # launcher that predates ADR 0138. Launchers serve both; this moves to /amp/
            # once the both-paths release is everywhere. Mirror of the header ordering.
            f"{self._config.mesh_base_url}/a2a/{name}",
            body=_http.json_body(payload if payload is not None else {}),
            headers={"Content-Type": "application/json"},
            **({"timeout": timeout} if timeout is not None else {}),
            expect=(200,),
        )
        data = resp.json()
        return data if isinstance(data, dict) else {"response": data}
