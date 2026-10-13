"""Drive the Python SDK's public API against a real launcher, one call per contract route.

Prints "PASS|FAIL|MISSING <route>[: reason]" for each route; hack/sdk-conformance runs it and checks
every PASS against what the launcher actually received.
"""

from __future__ import annotations

import dataclasses
import json
import os
import sys
import traceback
from typing import Any, Callable, Dict, Tuple

from ctxmesh import agent
from ctxmesh._capability import CAPABILITY_HEADER


def segment(path: str, i: int) -> str:
    parts = path.strip("/").split("/")
    return parts[i] if i < len(parts) else ""


def within(got: Any, want: Any, at: str = "$") -> Tuple[str, bool]:
    """Every value the SDK decoded must appear, equal, at the same place in the launcher's answer;
    arrays must have the same length. A key the SDK left empty and the launcher never sent is not
    a difference."""
    if isinstance(got, dict):
        if not isinstance(want, dict):
            return at, False
        for k, v in got.items():
            if v in (None, "", False, 0, {}) and k not in want:
                continue
            where, ok = within(v, want.get(k), f"{at}.{k}")
            if not ok:
                return where, False
        return "", True
    if isinstance(got, list):
        if not isinstance(want, list) or len(got) != len(want):
            return at, False
        for i, (g, w) in enumerate(zip(got, want)):
            where, ok = within(g, w, f"{at}[{i}]")
            if not ok:
                return where, False
        return "", True
    return at, got == want and type(got) is type(want) or (
        isinstance(got, (int, float)) and isinstance(want, (int, float)) and got == want
    )


def expect(what: str, got: Any, want: Any) -> None:
    where, ok = within(got, want)
    if not ok:
        raise AssertionError(
            f"{what} decoded as {json.dumps(got, default=str)}, but the launcher sent "
            f"{json.dumps(want)} (differs at {where})"
        )


def calls(c, capability: str) -> Dict[str, Callable[[Dict[str, Any]], None]]:
    def mesh(g):
        expect(
            "reply",
            c.mesh.call(segment(g["request"]["path"], 1), g["request"].get("body")),
            g["response"]["body"],
        )

    def delegate(g):
        b = g["request"]["body"]
        out = c.tools.delegate(b["subAgent"], b["input"], step=b["step"], call_id=b["callId"])
        expect("delegation", out, g["response"]["body"])

    def feedback(g):
        b = g["request"]["body"]
        c.feedback.score(b["traceId"], b["name"], b["value"], comment=b.get("comment"))

    def handoff(g):
        b = g["request"]["body"]
        out = c.tools.handoff(
            b["targetAgent"], b.get("message", ""), include_history=b.get("includeHistory", True)
        )
        expect("handoff", out, g["response"]["body"])

    def knowledge(g):
        b = g["request"]["body"]
        out = c.knowledge.search(b["query"], knowledge_base=b["knowledgeBase"], top_k=b["topK"])
        expect("chunks", {"results": out}, g["response"]["body"])

    def remember(g):
        b = g["request"]["body"]
        c.memory.remember(b["content"], b.get("tags"))

    def search_agent(g):
        b = g["request"]["body"]
        out = c.memory.search_agent(b["query"], top_k=b["topK"])
        expect("facts", {"results": out}, g["response"]["body"])

    def memory_get(g):
        expect("entries", c.memory.get(segment(g["request"]["path"], 1)), g["response"]["body"])

    def memory_put(g):
        c.memory.put(g["request"]["body"], conversation_id=segment(g["request"]["path"], 1))

    def memory_append(g):
        c.memory.append(g["request"]["body"], conversation_id=segment(g["request"]["path"], 1))

    def memory_search(g):
        out = c.memory.search(
            g["request"]["query"]["q"], conversation_id=segment(g["request"]["path"], 1)
        )
        expect("entries", out, g["response"]["body"])

    def skills(g):
        out = [dataclasses.asdict(s) for s in c.skills.list()]
        expect("skills", {"skills": out}, g["response"]["body"])

    def skill_load(g):
        expect(
            "skill", {"body": c.skills.load(g["request"]["body"]["name"])}, g["response"]["body"]
        )

    def tools(g):
        # Synthetic tools (delegate_to, handoff_to, knowledge_search) are the SDK's own; only the
        # manifest's tools come from the launcher.
        got = {t.name: t for t in c.tools.list()}
        want = g["response"]["body"]["tools"]
        for w in want:
            t = got.get(w["name"])
            if t is None:
                raise AssertionError(f"tools.list() has no {w['name']!r}; got {sorted(got)}")
            expect(
                f"tool {w['name']}",
                {
                    "name": t.name,
                    "mode": t.mode,
                    "endpoint": t.endpoint,
                    "transport": t.transport,
                    "description": t.description,
                },
                w,
            )
        extra = (
            set(got) - {w["name"] for w in want} - {"delegate_to", "handoff_to", "knowledge_search"}
        )
        if extra:
            raise AssertionError(f"tools.list() returned tools the launcher never served: {extra}")

    return {
        "POST /a2a/{targetAgent}": mesh,
        "POST /delegate": delegate,
        "ANY /feedback": feedback,
        "POST /handoff": handoff,
        "POST /knowledge/search": knowledge,
        "POST /memory/agent/remember": remember,
        "POST /memory/agent/search": search_agent,
        "GET /memory/{conversationId}": memory_get,
        "PUT /memory/{conversationId}": memory_put,
        "POST /memory/{conversationId}/append": memory_append,
        "GET /memory/{conversationId}/search": memory_search,
        "GET /skills": skills,
        "POST /skills/load": skill_load,
        "GET /tools": tools,
    }


def main() -> int:
    with open(os.environ["CTXMESH_PROBE_CONTRACT"]) as fh:
        contract = json.load(fh)
    capability = os.environ.get("CTXMESH_PROBE_CAPABILITY", "")
    # The env the launcher publishes is part of what is checked, so the client comes from it. The
    # mounted tools.json is left out: it is the SDK's fallback when /tools is unreachable, and would
    # let a broken /tools pass.
    env = {k: v for k, v in os.environ.items() if k != "TOOLS_JSON_PATH"}
    env["TOOLS_JSON_PATH"] = os.path.join(os.sep, "nonexistent", "tools.json")
    try:
        client = agent.from_env(environ=env)
        checks = calls(client, capability)
    except Exception as exc:  # noqa: BLE001 - every failure is reported per route
        for r in contract["routes"]:
            print(
                f"FAIL {r['method']} {r['path']}: the SDK does not configure from the launcher's "
                f"env: {exc}"
            )
        return 0
    with client.request_scope({CAPABILITY_HEADER: capability}):
        for r in contract["routes"]:
            key = f"{r['method']} {r['path']}"
            check = checks.get(key)
            if check is None:
                print(f"MISSING {key}: the Python SDK has no call for it")
                continue
            try:
                check(r["goldens"][0])
            except Exception as exc:  # noqa: BLE001 - a probe reports, it does not stop
                reason = f"{type(exc).__name__}: {exc}".replace("\n", " ")
                print(f"FAIL {key}: {reason}")
                traceback.print_exc(file=sys.stderr)
            else:
                print(f"PASS {key}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
