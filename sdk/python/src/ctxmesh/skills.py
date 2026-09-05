"""Skills — the agent's attached procedural knowledge (ADR 0137).

A SKILL is procedural knowledge with progressive disclosure. Its name and description sit in
the model's context permanently and cost almost nothing; the BODY is fetched only when the
model judges the task relevant. That is the whole reason skills exist rather than a longer
prompt: context is scarce, and ten attached skills should cost ten short lines until one of
them is actually needed.

So this module deliberately offers two calls, not one:

    list()          the always-affordable part — names and descriptions
    load(name)      the expensive part, fetched on demand

Calling ``load`` for every skill up front would defeat the design. It is available because an
agent that KNOWS it needs a skill should not have to pretend otherwise.

Gating:
    SKILL_REFS      comma-separated "<name>@sha256:…" — the agent's PINNED skills

Every ref is a digest, never an alias. The controller resolves aliases once, at deploy time,
and records the digest (AgentDeployment.status.resolvedSkills), so a skill cannot change
underneath a running agent — which is what keeps a replay fixture honest.

When SKILL_REFS is unset every call raises ConfigError immediately, rather than returning an
empty list: "this agent has no skills" and "skills are not wired here" are different facts.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import List, Optional

from ctxmesh import _http
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError

#: Launcher-local skill endpoints (the same :2998 listener as memory and knowledge).
_SKILL_LIST_PATH = "/skills"
_SKILL_LOAD_PATH = "/skills/load"

#: Fetching a body may resolve a git ref or pull from the object store.
_SKILL_TIMEOUT = 30.0


@dataclass(frozen=True)
class Skill:
    """One attached skill.

    ``description`` is the always-in-context line the model matches against to decide whether
    ``body`` is worth loading. ``digest`` is the version's identity — pinned, so two runs that
    report the same digest genuinely saw the same content.
    """

    name: str
    digest: str
    description: str


def _refs() -> List[str]:
    raw = os.environ.get("SKILL_REFS", "").strip()
    return [r.strip() for r in raw.split(",") if r.strip()]


def _require_enabled() -> None:
    if not _refs():
        raise ConfigError(
            "no skills are attached to this agent (SKILL_REFS is unset). "
            "Attach them with AgentDeployment.spec.skillRefs."
        )


def attached() -> List[str]:
    """The raw pinned refs, ``["<name>@sha256:…", …]``.

    Cheap and offline — it reads the injected env and makes no call. Useful for a trace
    attribute or an assertion that a run used the version you expected.
    """
    return _refs()


def list(*, config: Optional[PlaneConfig] = None) -> List[Skill]:
    """Names and descriptions of every attached skill — NOT their bodies.

    This is the call an agent makes on every run. It stays affordable no matter how many
    skills are attached, which is the property that makes progressive disclosure work.
    """
    _require_enabled()
    cfg = config or PlaneConfig.from_env()
    resp = _http.request(
        "GET",
        f"{cfg.memory_base_url}{_SKILL_LIST_PATH}",
        timeout=_SKILL_TIMEOUT,
        expect=(200,),
    )
    data = resp.json()
    items = data.get("skills") if isinstance(data, dict) else None
    out: List[Skill] = []
    for item in items or []:
        out.append(
            Skill(
                name=item.get("name", ""),
                digest=item.get("digest", ""),
                description=item.get("description", ""),
            )
        )
    return out


def load(name: str, *, config: Optional[PlaneConfig] = None) -> str:
    """Fetch one skill's BODY, by name.

    Call this when the model has decided a skill applies — not up front for everything. The
    launcher resolves the name against the agent's pinned refs, so a body that is not attached
    cannot be fetched however the name is spelled.
    """
    _require_enabled()
    if not name or not name.strip():
        raise ConfigError("load() needs a skill name")
    cfg = config or PlaneConfig.from_env()
    resp = _http.request(
        "POST",
        f"{cfg.memory_base_url}{_SKILL_LOAD_PATH}",
        body=_http.json_body({"name": name.strip()}),
        headers={"Content-Type": "application/json"},
        timeout=_SKILL_TIMEOUT,
        expect=(200,),
    )
    data = resp.json()
    body = data.get("body") if isinstance(data, dict) else None
    return body if isinstance(body, str) else ""


class SkillsClient:
    """Client bound to a plane config, matching the TypeScript ``client.skills`` surface.

    The module-level ``list``/``load`` remain for agents that hold no Client.
    """

    def __init__(self, config: PlaneConfig) -> None:
        self._config = config

    def attached(self) -> List[str]:
        """The raw pinned refs. Offline — no call."""
        return attached()

    def list(self) -> List[Skill]:
        """Names and descriptions only. The affordable call."""
        return list(config=self._config)

    def load(self, name: str) -> str:
        """One skill's body, on demand."""
        return load(name, config=self._config)
