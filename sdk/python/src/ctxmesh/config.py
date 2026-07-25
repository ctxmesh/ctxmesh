"""Launcher-plane configuration: the env the SDK reads to find the localhost plane.

The launcher (PID 1) injects a fixed set of env vars into every agent container
describing the localhost platform plane and the run context (ADR 0002, spec
"The localhost plane the SDK wraps"). ``PlaneConfig.from_env`` reads them; the
per-capability clients consult the resulting config for their base URLs.

Ports (spec table):

    memory     localhost:$MEMORY_PORT     (default 2998)   gate: MEMORY_PORT set
    tools      localhost:2999             (fixed)          always addressable
    feedback   localhost:$FEEDBACK_PORT   (default 2995)   gate: FEEDBACK_PORT set

Run context: AGENT_NAME (+ version/role/registry id) and the conversationId.
conversationId is NOT a launcher env — the agent extracts it from its inbound
request payload and stamps it on outbound calls via the ``X-Conversation-Id``
header (the same convention the memory/gateway/A2A paths already use). So it is
supplied per-call or set once on the client via ``with_conversation(...)``.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Callable, Dict, Optional

from ctxmesh.errors import NotInPodError

# The discovery sidecar always listens on this fixed localhost port (spec: no
# env, fixed port). tools.json is its durable cold-start backing.
DISCOVERY_PORT = 2999
DEFAULT_TOOLS_JSON_PATH = "/etc/agent/tools.json"

DEFAULT_MEMORY_PORT = 2998
DEFAULT_FEEDBACK_PORT = 2995

# Any one of these env vars being present is proof we are running under the
# launcher (some plane, some run context). Their combined absence means we are
# not in a launcher pod and from_env() must fail fast.
_LAUNCHER_MARKERS = (
    "AGENT_NAME",
    "MEMORY_PORT",
    "MEMORY_BACKEND_ADDR",
    "FEEDBACK_PORT",
    "LANGFUSE_HOST",
)


def _parse_port(raw: Optional[str], default: int, name: str) -> int:
    """Parse a port env var, mirroring the launcher's own 1..65535 rule."""
    if raw is None or raw == "":
        return default
    try:
        port = int(raw)
    except ValueError as exc:
        raise NotInPodError(f"{name}={raw!r} is not a valid port") from exc
    if port < 1 or port > 65535:
        raise NotInPodError(f"{name}={raw!r} out of range (1..65535)")
    return port


@dataclass(frozen=True)
class RunContext:
    """Identity + correlation the SDK stamps on plane calls (spec "Run context")."""

    agent_name: str = ""
    agent_version: str = ""
    agent_role: str = ""
    agent_registry_id: str = ""
    prompt_version: str = ""
    #: Default conversationId, if the agent chose to inject one via env. Usually
    #: empty — conversationId is normally supplied per-call from the request.
    conversation_id: str = ""


@dataclass(frozen=True)
class PlaneConfig:
    """Resolved addresses + run context for the launcher localhost plane."""

    memory_base_url: str
    discovery_base_url: str
    feedback_base_url: str
    tools_json_path: str
    run: RunContext = field(default_factory=RunContext)
    #: Whether each capability was actually wired by the launcher. A client for
    #: an unwired capability raises ConfigError rather than hitting a dead port.
    memory_wired: bool = False
    #: Whether long-term (agent-scope, ADR 0045) memory is enabled — the launcher
    #: exposes /memory/agent/{remember,search}. Gate: MEMORY_LONGTERM_ENABLED=true.
    longterm_wired: bool = False
    feedback_wired: bool = False
    #: Model gateway base URL ($MODEL_GATEWAY_URL) — LiteLLM directly, or the
    #: launcher's in-pod budget proxy when the agent is budgeted (transparent).
    #: Empty when unwired (not in a pod); model.chat then raises ConfigError.
    model_gateway_url: str = ""
    #: Bearer token the gateway expects; the launcher injects the master key
    #: in-pod. Empty offline (the mock gateway ignores auth).
    model_gateway_key: str = ""
    #: OTLP/gRPC collector endpoint ($OTEL_EXPORTER_OTLP_ENDPOINT, :4317). Empty
    #: offline → the trace client runs in no-op/in-memory export mode.
    otlp_endpoint: str = ""

    @classmethod
    def from_env(
        cls,
        environ: Optional[Dict[str, str]] = None,
        *,
        require_launcher: bool = True,
    ) -> PlaneConfig:
        """Build a PlaneConfig from the launcher-injected env.

        Fails fast with :class:`NotInPodError` when no launcher marker env is
        present and ``require_launcher`` is True — never silently no-ops (spec
        edge case). Tests set ``require_launcher=False`` (or pass an ``environ``
        that includes a marker) to build an offline config against a stub.
        """
        env: Callable[[str], Optional[str]] = (
            os.environ.get if environ is None else environ.get
        )

        if require_launcher and not any(env(marker) for marker in _LAUNCHER_MARKERS):
            raise NotInPodError(
                "ctxmesh.agent.from_env(): no launcher environment detected "
                "(none of "
                + ", ".join(_LAUNCHER_MARKERS)
                + " is set). The SDK reads the launcher-injected localhost "
                "plane and only works inside an agent-engine pod. For tests or "
                "offline use, build a PlaneConfig explicitly and call "
                "agent.from_config(config)."
            )

        memory_port = _parse_port(env("MEMORY_PORT"), DEFAULT_MEMORY_PORT, "MEMORY_PORT")
        feedback_port = _parse_port(env("FEEDBACK_PORT"), DEFAULT_FEEDBACK_PORT, "FEEDBACK_PORT")

        # Memory is wired iff the launcher started the :2998 listener, i.e. a
        # MemoryBinding injected MEMORY_PORT / MEMORY_BACKEND_ADDR. Feedback is
        # wired iff LANGFUSE_HOST/FEEDBACK_PORT were injected (M9). Tools/:2999
        # is always addressable when a discovery sidecar is present; we treat it
        # as always-wired and let the manifest fetch surface an unreachable port.
        memory_wired = bool(env("MEMORY_PORT") or env("MEMORY_BACKEND_ADDR"))
        feedback_wired = bool(env("FEEDBACK_PORT") or env("LANGFUSE_HOST"))

        run = RunContext(
            agent_name=env("AGENT_NAME") or "",
            agent_version=env("AGENT_VERSION") or "",
            agent_role=env("AGENT_ROLE") or "",
            agent_registry_id=env("AGENT_REGISTRY_ID") or "",
            prompt_version=env("PROMPT_VERSION") or "",
            conversation_id=env("CONVERSATION_ID") or "",
        )

        # Model gateway ($MODEL_GATEWAY_URL): LiteLLM directly, or the launcher's
        # budget proxy when budgeted — the SDK does not care which, same wire. The
        # gateway bearer token is a dummy in dev mode (LiteLLM holds real keys);
        # honour an explicit MODEL_GATEWAY_KEY/OPENAI_API_KEY if the operator set one.
        model_gateway_url = (env("MODEL_GATEWAY_URL") or "").rstrip("/")
        model_gateway_key = env("MODEL_GATEWAY_KEY") or env("OPENAI_API_KEY") or ""

        return cls(
            memory_base_url=f"http://localhost:{memory_port}",
            discovery_base_url=f"http://localhost:{DISCOVERY_PORT}",
            feedback_base_url=f"http://localhost:{feedback_port}",
            tools_json_path=env("TOOLS_JSON_PATH") or DEFAULT_TOOLS_JSON_PATH,
            run=run,
            memory_wired=memory_wired,
            longterm_wired=(env("MEMORY_LONGTERM_ENABLED") or "").strip().lower() == "true",
            feedback_wired=feedback_wired,
            model_gateway_url=model_gateway_url,
            model_gateway_key=model_gateway_key,
            otlp_endpoint=env("OTEL_EXPORTER_OTLP_ENDPOINT") or "",
        )

    @classmethod
    def for_test(
        cls,
        *,
        memory_base_url: str = "http://localhost:2998",
        discovery_base_url: str = "http://localhost:2999",
        feedback_base_url: str = "http://localhost:2995",
        model_gateway_url: str = "http://localhost:2996",
        model_gateway_key: str = "",
        otlp_endpoint: str = "",
        run: Optional[RunContext] = None,
        tools_json_path: str = DEFAULT_TOOLS_JSON_PATH,
    ) -> PlaneConfig:
        """Build a fully-wired config pointing at explicit URLs (the launcher stub).

        The documented offline/test mode: point the base URLs at a fake localhost
        plane so the clients can be exercised without a live launcher.
        ``otlp_endpoint`` defaults empty so the trace client runs in offline/no-op
        export mode unless a test opts into a live/in-memory processor.
        """
        return cls(
            memory_base_url=memory_base_url.rstrip("/"),
            discovery_base_url=discovery_base_url.rstrip("/"),
            feedback_base_url=feedback_base_url.rstrip("/"),
            tools_json_path=tools_json_path,
            run=run or RunContext(agent_name="test-agent"),
            memory_wired=True,
            feedback_wired=True,
            model_gateway_url=model_gateway_url.rstrip("/"),
            model_gateway_key=model_gateway_key,
            otlp_endpoint=otlp_endpoint,
        )
