"""Every launcher route, reached through the public SDK, against the generated fake launcher.

The fake is generated from sdk/launcher-routes.json, whose goldens cmd/launcher proves against the
real handlers. It 404s anything the launcher does not serve and records every request that breaks
a route's schema, so a client calling the wrong path, or sending the wrong field, fails here.
"""

from __future__ import annotations

import dataclasses
import json
import urllib.error
import urllib.request
from typing import Any, Callable, Dict, Iterator, Tuple

import pytest

from ctxmesh import agent
from ctxmesh._capability import CAPABILITY_HEADER
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.errors import EndpointError
from ctxmesh.testing import DelegateStub, DiscoveryStub, FeedbackStub, MemoryStub, MeshStub

from ._generated_fake_launcher import FakeLauncher, validate

CAPABILITY = "probe-run-capability"


@pytest.fixture
def fake(monkeypatch) -> Iterator[FakeLauncher]:
    with FakeLauncher() as f:
        # tools.delegate/handoff post to a fixed 127.0.0.1:2994; point them at the fake.
        monkeypatch.setattr("ctxmesh.tools._DELEGATE_ENDPOINT", f"{f.base_url}/delegate")
        monkeypatch.setattr("ctxmesh.tools._HANDOFF_ENDPOINT", f"{f.base_url}/handoff")
        monkeypatch.setenv("KNOWLEDGE_BASE_ENABLED", "true")
        monkeypatch.setenv("KNOWLEDGE_BASES", json.dumps([{"name": "handbook", "namespace": "ns"}]))
        monkeypatch.setenv("SKILL_REFS", "deploy@sha256:abc")
        yield f


def client_for(f: FakeLauncher) -> Client:
    cfg = PlaneConfig.for_test(
        memory_base_url=f.base_url,
        discovery_base_url=f.base_url,
        feedback_base_url=f.base_url,
        mesh_base_url=f.base_url,
        mesh_wired=True,
        run=RunContext(agent_name="probe-agent", conversation_id="conv-1"),
        tools_json_path="/nonexistent/tools.json",
    )
    return agent.from_config(dataclasses.replace(cfg, longterm_wired=True))


def calls(c: Client) -> Dict[str, Callable[[], Any]]:
    """One public SDK call per route the Python SDK reaches."""
    return {
        "POST /a2a/{targetAgent}": lambda: c.mesh.call("peer-agent", {"question": "ping"}),
        "POST /delegate": lambda: c.tools.delegate(
            "researcher", "Summarise the quarterly report", step="1", call_id="call-1"
        ),
        "ANY /feedback": lambda: c.feedback.score(
            "0af7651916cd43dd8448eb211c80319c", "helpfulness", 0.9, comment="clear answer"
        ),
        "POST /handoff": lambda: c.tools.handoff(
            "billing", "The customer is asking about an invoice."
        ),
        "POST /knowledge/search": lambda: c.knowledge.search(
            "When do deploys go out?", knowledge_base="handbook", top_k=3
        ),
        "POST /memory/agent/remember": lambda: c.memory.remember(
            "The user prefers metric units.", {"topic": "preferences"}
        ),
        "POST /memory/agent/search": lambda: c.memory.search_agent("units", top_k=3),
        "GET /memory/{conversationId}": lambda: c.memory.get("conv-1"),
        "PUT /memory/{conversationId}": lambda: c.memory.put(
            [{"role": "user", "content": "Let's start over."}], conversation_id="conv-2"
        ),
        "POST /memory/{conversationId}/append": lambda: c.memory.append(
            {"role": "user", "content": "And one more thing."}, conversation_id="conv-3"
        ),
        "GET /memory/{conversationId}/search": lambda: c.memory.search(
            "metric", conversation_id="conv-1"
        ),
        "GET /skills": lambda: c.skills.list(),
        "POST /skills/load": lambda: c.skills.load("deploy"),
        "GET /tools": lambda: c.tools.list(),
    }


def golden_body(f: FakeLauncher, key: str) -> Any:
    return f.golden(key)["response"]["body"]


def test_every_launcher_route_is_reached(fake: FakeLauncher):
    c = client_for(fake)
    with c.request_scope({CAPABILITY_HEADER: CAPABILITY}):
        for call in calls(c).values():
            call()
    for key in fake.route_keys():
        why = fake.waived(key)
        if why:
            assert not fake.is_covered(
                key
            ), f"{key} is waived for Python ({why}) but the SDK reaches it now: delete the waiver"
        else:
            assert fake.is_covered(key), f"the launcher serves {key} and no SDK call reached it"
    fake.assert_conforms()


def test_responses_decode_as_the_launcher_sends_them(fake: FakeLauncher):
    c = client_for(fake)
    results = {}
    with c.request_scope({CAPABILITY_HEADER: CAPABILITY}):
        for key, call in calls(c).items():
            results[key] = call()
    for key in (
        "POST /a2a/{targetAgent}",
        "POST /delegate",
        "POST /handoff",
        "GET /memory/{conversationId}",
        "GET /memory/{conversationId}/search",
    ):
        assert results[key] == golden_body(fake, key), key
    for key in ("POST /knowledge/search", "POST /memory/agent/search"):
        assert results[key] == golden_body(fake, key)["results"], key
    assert results["POST /skills/load"] == golden_body(fake, "POST /skills/load")["body"]
    skills = [dataclasses.asdict(s) for s in results["GET /skills"]]
    assert skills == golden_body(fake, "GET /skills")["skills"]
    by_name = {t.name: t for t in results["GET /tools"]}
    for want in golden_body(fake, "GET /tools")["tools"]:
        got = by_name[want["name"]]
        assert (got.mode, got.endpoint, got.transport, got.description) == (
            want["mode"],
            want["endpoint"],
            want["transport"],
            want.get("description", ""),
        )
    fake.assert_conforms()


@pytest.mark.parametrize(
    "key,golden,call",
    [
        (
            "POST /knowledge/search",
            "refuses-a-knowledge-base-not-granted",
            lambda c: c.knowledge.search("salaries", knowledge_base="payroll"),
        ),
        ("POST /skills/load", "refuses-a-skill-not-attached", lambda c: c.skills.load("payroll")),
        (
            "POST /a2a/{targetAgent}",
            "refuses-a-target-that-is-not-a-dns-label",
            lambda c: c.mesh.call("peer-agent", {"question": "ping"}),
        ),
        (
            "GET /memory/{conversationId}",
            "refuses-an-id-with-a-separator",
            lambda c: c.memory.get("conv-1"),
        ),
    ],
)
def test_refusals_surface_the_launcher_status(fake: FakeLauncher, key, golden, call):
    fake.use_golden(key, golden)
    want = fake.golden(key, golden)["response"]["status"]
    with pytest.raises(EndpointError) as exc:
        call(client_for(fake))
    assert exc.value.status == want


def test_delegate_and_handoff_decode_a_refusal_as_an_outcome(fake: FakeLauncher):
    """The launcher answers 200 for every delegate/handoff outcome and signals success in "ok"."""
    c = client_for(fake)
    fake.use_golden("POST /delegate", "refuses-without-the-idempotency-key")
    fake.use_golden("POST /handoff", "refuses-a-target-outside-the-roster")
    with c.request_scope({CAPABILITY_HEADER: CAPABILITY}):
        d = c.tools.delegate("researcher", "x", step="1", call_id="call-1")
        h = c.tools.handoff("stranger")
    assert d["ok"] is False and d["error"]
    assert h["ok"] is False and h["error"]


def test_the_fake_refuses_what_the_launcher_refuses(fake: FakeLauncher):
    """The fake is only worth generating if it catches what hand-written fakes let through."""
    from ctxmesh import _http

    _http.request(
        "POST",
        f"{fake.base_url}/memory/agent/remember",
        body=_http.json_body({"text": "a fact"}),
        headers={"Content-Type": "application/json"},
        expect=None,
    )
    assert fake.violations(), "a remember body without content passed the fake"
    with pytest.raises(EndpointError) as exc:
        _http.request("POST", f"{fake.base_url}/memory/agent/forget", expect=(200,))
    assert exc.value.status == 404
    with pytest.raises(EndpointError) as exc:
        _http.request("GET", f"{fake.base_url}/skills/load", expect=(200,))
    assert exc.value.status == 405


def test_respond_refuses_a_response_the_launcher_never_sends(fake: FakeLauncher):
    with pytest.raises(AssertionError):
        fake.respond("POST /knowledge/search", 500, "boom")
    with pytest.raises(AssertionError):
        fake.respond("POST /knowledge/search", 200, {"results": [{"content": "no provenance"}]})


# The public offline stubs (ctxmesh.testing) are what authors test their agents against, so they
# must answer the way the launcher does. Each is driven with its routes' success goldens.
PUBLIC_STUBS: Tuple[Tuple[type, Tuple[str, ...]], ...] = (
    (
        MemoryStub,
        (
            "GET /memory/{conversationId}",
            "PUT /memory/{conversationId}",
            "POST /memory/{conversationId}/append",
            "GET /memory/{conversationId}/search",
        ),
    ),
    (FeedbackStub, ("ANY /feedback",)),
    (DelegateStub, ("POST /delegate", "POST /handoff")),
    (MeshStub, ("POST /a2a/{targetAgent}", "POST /amp/{targetAgent}")),
    (DiscoveryStub, ("GET /tools",)),
)


def _send(base_url: str, golden: Dict[str, Any], method: str) -> Tuple[int, str, bytes]:
    req = golden["request"]
    url = base_url + req["path"]
    if req.get("query"):
        url += "?" + "&".join(f"{k}={v}" for k, v in req["query"].items())
    data = None
    if "body" in req:
        data = json.dumps(req["body"]).encode()
    elif "bodyText" in req:
        data = req["bodyText"].encode()
    headers = {"Content-Type": "application/json", **(req.get("headers") or {})}
    r = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(r, timeout=5) as resp:
            return resp.status, resp.headers.get("Content-Type", ""), resp.read()
    except urllib.error.HTTPError as err:
        return err.code, err.headers.get("Content-Type", ""), err.read()


@pytest.mark.parametrize(
    "stub_cls,key",
    [(cls, key) for cls, keys in PUBLIC_STUBS for key in keys],
    ids=lambda v: v.__name__ if isinstance(v, type) else v,
)
def test_public_stubs_answer_as_the_contract_says(fake: FakeLauncher, stub_cls, key):
    route = fake._route(key)
    golden = fake.golden(key)
    method = golden["request"].get("method", route["servedMethod"])
    with stub_cls() as stub:
        status, _ctype, body = _send(stub.base_url, golden, method)
    want = golden["response"]["status"]
    assert status == want, f"{stub_cls.__name__} answers {key} with {status}, the launcher {want}"
    decl = route["responses"][str(status)]
    if body and decl.get("schema") is not None:
        assert validate(decl["schema"], json.loads(body)) == []
