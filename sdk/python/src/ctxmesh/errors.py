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
