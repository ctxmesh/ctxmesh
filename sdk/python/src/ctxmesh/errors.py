"""Exception hierarchy for the ctxmesh SDK.

The SDK never swallows a launcher-plane error: a bad configuration, an absent
launcher env, or a non-2xx endpoint response all surface as a typed exception
the caller can catch (spec "Edge cases": endpoint down → surface, not silent).
"""

from __future__ import annotations


class CtxmeshError(Exception):
    """Base class for every error raised by the SDK."""


class ConfigError(CtxmeshError):
    """The SDK was asked to do something its configuration cannot support.

    Raised, for example, when a client is used but its endpoint was not wired
    (the launcher did not inject the corresponding env), so there is no address
    to talk to.
    """


class NotInPodError(ConfigError):
    """``agent.from_env()`` was called outside a launcher pod.

    The launcher-injected env that identifies the localhost plane is absent, so
    the SDK cannot know where memory / tools / feedback live. This fails fast
    with a clear message rather than silently no-oping (spec: "never silently
    no-ops"). For tests / offline use, build a ``PlaneConfig`` explicitly or use
    ``agent.from_config(...)``.
    """


class EndpointError(CtxmeshError):
    """A launcher-plane endpoint returned an error or was unreachable.

    Carries the HTTP status (when there was a response) so the caller can react
    to, e.g., a feedback 400 (bad request) vs a 502 (upstream/Langfuse down)
    without string-matching. ``status`` is ``None`` for transport-level failures
    (connection refused, timeout) where no HTTP response was received.
    """

    def __init__(self, message: str, *, status: int | None = None, body: str | None = None):
        super().__init__(message)
        self.status = status
        self.body = body


class ConsentRequiredError(EndpointError):
    """A tool call reached an MCP server the invoking user has not connected an account to.

    The injecting egress sidecar (ADR 0029 §2) returned a structured ``consent_required``:
    the user must connect their OWN account to ``server`` before the agent can call the tool
    on their behalf. The managed loop turns this into a run OUTCOME (a "Connect your account"
    signal the console renders as a CTA), not a crash — the model is told to report + stop.
    """

    def __init__(
        self, message: str, *, server: str, status: int | None = None, body: str | None = None
    ):
        super().__init__(message, status=status, body=body)
        self.server = server


class GuardrailBlockedError(EndpointError):
    """A model call was refused by the launcher's in-path guardrail engine (M66, ADR 0059 §8).

    The guardrail proxy returned HTTP 403 with ``{"error":{"type":"guardrail_blocked",
    "detector":"…","scan_point":"…"}}``.  This is a **terminal content-policy decision**,
    not a transient failure: the request was examined and refused on policy grounds, so it
    MUST NOT be retried (re-generating blocked content burns budget without changing the
    outcome).  The managed loop surfaces it as an honest run failure rather than crashing
    or silently succeeding.

    Attributes:
        detector: the name of the guardrail rule (detector) that triggered the block.
        scan_point: where the block originated — ``"input"``, ``"toolOutput"``, or
            ``"output"``.
        status: always 403 (inherited from :class:`EndpointError`).
    """

    def __init__(
        self,
        message: str,
        *,
        detector: str,
        scan_point: str,
        status: int = 403,
        body: str | None = None,
    ):
        super().__init__(message, status=status, body=body)
        self.detector = detector
        self.scan_point = scan_point


class ApprovalRequiredError(CtxmeshError):
    """A run reached a step gated on human approval (human-in-the-loop, ADR 0034 §HITL, m32.4).

    Raised by :func:`ctxmesh.pause_for_approval` when the step's ``key`` has not (yet) been
    approved. The managed loop turns it into a run OUTCOME — a ``requires_action`` (approval) the
    console renders as an approve/deny affordance — not a crash. When the approver resolves it, the
    run resumes: the re-invoke carries the approved key, so ``pause_for_approval`` proceeds instead
    of raising.
    """

    def __init__(self, message: str, *, key: str, summary: str):
        super().__init__(message)
        self.key = key
        self.summary = summary
