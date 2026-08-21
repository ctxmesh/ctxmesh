"""Tools client — the discovery sidecar (:2999) + MCP tool invocation (M4).

``list()`` fetches the live manifest from the discovery sidecar
(``GET localhost:2999/tools``), falling back to the mounted ``tools.json``
(cold-start backing) when the sidecar is unreachable — the same precedence the
raw agent uses (mcp-tools.md "Agent consumption").

``call(name, **args)`` looks the tool up in the live manifest, then invokes it
over its MCP ``streamable-http`` endpoint. The endpoint is taken verbatim from
the manifest (it already ends in ``/mcp`` per the m4.4 finding). We speak the
MCP wire directly over stdlib rather than pulling the heavyweight ``mcp``
package in as a runtime dep — the SDK stays lean and 3.9-compatible.

**Catalog name vs MCP tool name.** The discovery manifest name is the
*ToolRegistry catalog key* (e.g. ``word-count``, hyphen), which is NOT
necessarily the name the MCP server exposes the tool under (e.g. ``word_count``,
underscore — a FastMCP function name). ``toolmanifest.Tool`` carries only the
catalog name/endpoint, so ``call`` must discover the real MCP name from the
server: it does the handshake, runs ``tools/list``, resolves the catalog name to
a server tool name (exact → hyphen/underscore-normalized → sole-tool fallback),
and only then calls with the *resolved* name. (Carrying the MCP name in the
manifest is a phase-2 M4 item; the SDK resolves it at call time for now.)
"""

from __future__ import annotations

import json
import os
from typing import Any, Dict, List, Optional

from ctxmesh import _http
from ctxmesh._approval import APPROVAL_HEADER, current_approval_voucher
from ctxmesh._capability import CAPABILITY_HEADER, current_capability
from ctxmesh._record import RECORD_HEADER, current_record_run_id
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import (
    ApprovalRequiredError,
    ConfigError,
    ConsentRequiredError,
    EndpointError,
)

#: The synthetic sub-agent-delegation tool (M64, ADR 0057). A team SUPERVISOR is given this built-in
#: tool alongside its MCP tools; calling it starts a roster member as a durable SUB-RUN (via the
#: launcher-local endpoint) and returns its result. Enabled only when the controller injects
#: ``DELEGATE_ENABLED=true`` (a supervisor); a plain agent never sees it.
DELEGATE_TOOL_NAME = "delegate_to"

#: The synthetic knowledge-retrieval tool (M68, ADR 0061 Fork 3). An agent with knowledge bases
#: bound gets this built-in tool alongside its MCP tools; calling it performs an agentic RAG
#: retrieval mid-loop and returns results with provenance (documentRef/chunkIndex) so the model
#: can cite sources. Enabled only when KNOWLEDGE_BASE_ENABLED=true (the launcher injected a KB
#: binding); a plain agent never sees it.
KNOWLEDGE_SEARCH_TOOL_NAME = "knowledge_search"

#: The synthetic HANDOFF (transfer-of-control) tool (M67, ADR 0060 §5). A team supervisor/member
#: with a roster is given this built-in tool alongside delegate_to; calling it TRANSFERS the
#: conversation to another roster member and ENDS this agent's turn (unlike delegate_to, which
#: awaits + consumes a result). Enabled by the SAME DELEGATE_ENABLED signal (a roster-bearing
#: agent) — a plain agent never sees it.
HANDOFF_TOOL_NAME = "handoff_to"

#: The launcher-local delegate endpoint (:2994). Platform-owned — the agent's user code POSTs here;
#: the launcher stamps the spawn envelope + guard, so the agent can't forge a spawn.
_DELEGATE_ENDPOINT = "http://127.0.0.1:2994/delegate"

#: The launcher-local handoff endpoint (:2994, same listener as delegate). Platform-owned — the
#: launcher fail-fast validates roster membership + relays the run capability to the BFF edge.
_HANDOFF_ENDPOINT = "http://127.0.0.1:2994/handoff"

#: A delegated sub-run executes synchronously (the launcher blocks until it is terminal), so this is
#: generous — a sub-agent may itself do several tool round-trips.
_DELEGATE_TIMEOUT = 600.0

#: The spawn-tree position headers (mirrors internal/bff/invoke.go). Relayed on a /delegate call so the
#: launcher's depth gate (L7 suspension is depth-0 only) + spawn guard key on the AUTHORITATIVE root
#: rather than defaulting to depth 0 / root "" for every SDK-driven delegation.
SPAWN_ROOT_HEADER = "X-Ctxmesh-Spawn-Root"
SPAWN_DEPTH_HEADER = "X-Ctxmesh-Spawn-Depth"

#: A handoff is NOT awaited (the transfer terminates this turn) — the launcher only relays to the
#: BFF, terminates A, and queues B, so this is a short round-trip.
_HANDOFF_TIMEOUT = 30.0


def _delegate_enabled() -> bool:
    """True when this agent is a team supervisor (the controller injected DELEGATE_ENABLED)."""
    return os.environ.get("DELEGATE_ENABLED", "").strip() == "true"


def _delegate_roster() -> List[Dict[str, str]]:
    """Parse the DELEGATE_ROSTER env (a JSON list of ``{"name","description"}``) the controller
    injects from the AgentTeam roster — teaches the model which sub-agents it can summon."""
    raw = os.environ.get("DELEGATE_ROSTER", "").strip()
    if not raw:
        return []
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return []
    out: List[Dict[str, str]] = []
    if isinstance(data, list):
        for entry in data:
            if isinstance(entry, dict) and entry.get("name"):
                out.append(
                    {"name": str(entry["name"]), "description": str(entry.get("description", ""))}
                )
    return out


def _delegate_tool() -> Tool:
    """Build the synthetic ``delegate_to`` tool: its description lists the roster so the model picks
    a sub-agent by capability, and its schema constrains ``sub_agent`` to the roster when known."""
    roster = _delegate_roster()
    names = [r["name"] for r in roster]
    listing = (
        "\n".join(f"- {r['name']}: {r['description']}" for r in roster)
        or "(configured on the AgentTeam)"
    )
    description = (
        "Delegate a subtask to a sub-agent on your team and wait for its result. Use it to break a "
        "complex task into pieces handled by the right specialist, then combine their answers. "
        f"Available sub-agents:\n{listing}"
    )
    sub_agent_schema: Dict[str, Any] = {
        "type": "string",
        "description": "The roster member to delegate to.",
    }
    if names:
        sub_agent_schema["enum"] = names
    schema: Dict[str, Any] = {
        "type": "object",
        "properties": {
            "sub_agent": sub_agent_schema,
            "task": {
                "type": "string",
                "description": "The subtask to hand to the sub-agent, in plain language.",
            },
        },
        "required": ["sub_agent", "task"],
    }
    return Tool(
        name=DELEGATE_TOOL_NAME,
        mode="delegate",
        endpoint=_DELEGATE_ENDPOINT,
        transport="http",
        input_schema=schema,
        description=description,
    )


def _handoff_tool() -> Tool:
    """Build the synthetic ``handoff_to`` tool (M67, ADR 0060 §5): its description lists the roster
    so the model picks whom to transfer TO, and its schema constrains ``target_agent`` to the roster
    when known. Unlike ``delegate_to`` (call-and-return), a ``handoff_to`` call TRANSFERS the
    conversation and ENDS this agent's turn — the agent does not wait for or consume a result; the
    target continues with the end user."""
    roster = _delegate_roster()
    names = [r["name"] for r in roster]
    listing = (
        "\n".join(f"- {r['name']}: {r['description']}" for r in roster)
        or "(configured on the AgentTeam)"
    )
    description = (
        "Hand off the ENTIRE conversation to another agent on your team and END your turn. Use it "
        "when another specialist should take over the conversation with the user from here — this "
        "is a TRANSFER, not a delegation: you do NOT get a result back and you do NOT continue "
        "after calling it. The target agent continues talking with the user directly. "
        f"Available agents:\n{listing}"
    )
    target_schema: Dict[str, Any] = {
        "type": "string",
        "description": "The roster member to hand the conversation off to.",
    }
    if names:
        target_schema["enum"] = names
    schema: Dict[str, Any] = {
        "type": "object",
        "properties": {
            "target_agent": target_schema,
            "message": {
                "type": "string",
                "description": (
                    "An optional handoff note for the receiving agent (why you are transferring, "
                    "what is needed next). By default the full conversation history transfers "
                    "automatically; set include_history=false to hand off with THIS message as a "
                    "SUMMARY instead (the receiving agent then starts from your summary rather "
                    "than replaying the whole thread — use it for long conversations)."
                ),
            },
            "include_history": {
                "type": "boolean",
                "description": (
                    "Whether the receiving agent replays the full conversation history on the "
                    "transfer turn (default true). Set false to hand off with `message` as a "
                    "summary and skip the full-history replay — cheaper for a long conversation."
                ),
            },
        },
        "required": ["target_agent"],
    }
    return Tool(
        name=HANDOFF_TOOL_NAME,
        mode="handoff",
        endpoint=_HANDOFF_ENDPOINT,
        transport="http",
        input_schema=schema,
        description=description,
    )


def _knowledge_search_tool() -> "Tool":
    """Build the synthetic ``knowledge_search`` tool (M68, ADR 0061 Fork 3).

    The description lists the granted corpora (from the KNOWLEDGE_BASES roster) so the model
    knows what it can search and must attribute results to a source. The schema is intentionally
    open on knowledge_base (no enum) so the model can name a KB that happens to be unavailable
    and get a clear error from the launcher rather than a silent schema rejection.
    """
    from ctxmesh.knowledge import _roster_names  # local import to avoid circular deps

    names = _roster_names()
    if names:
        corpora_listing = "\n".join(f"- {n}" for n in names)
        kb_description = (
            f"The knowledge base to search. Available corpora:\n{corpora_listing}\n"
            "Omit to use the default when only one corpus is available."
        )
    else:
        kb_description = "The knowledge base to search (configured on the AgentDeployment)."
    description = (
        "Search a knowledge base for relevant passages and return them with source provenance "
        "(documentRef, chunkIndex) for citation. Use this to retrieve grounding context mid-loop "
        "before answering questions that require factual recall from a corpus. ALWAYS cite the "
        "documentRef when you use retrieved content in your answer. "
    )
    if names:
        description += f"Available corpora: {', '.join(names)}."
    schema: Dict[str, Any] = {
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "The search query — a natural-language question or phrase.",
            },
            "knowledge_base": {
                "type": "string",
                "description": kb_description,
            },
            "top_k": {
                "type": "integer",
                "description": "Maximum number of results to return (default 10).",
                "default": 10,
            },
        },
        "required": ["query"],
    }
    return Tool(
        name=KNOWLEDGE_SEARCH_TOOL_NAME,
        mode="knowledge",
        endpoint="",  # dispatched locally via KnowledgeClient, not over MCP
        transport="internal",
        input_schema=schema,
        description=description,
    )


#: MCP protocol version the SDK negotiates (the version the M4 fixture speaks).
_MCP_PROTOCOL_VERSION = "2025-03-26"

#: Manifest fetch timeout — a cheap localhost GET, but generous for a waking pod.
_MANIFEST_TIMEOUT = 2.0

#: Tool-call timeout — a remote MCP round-trip may be slower than a localhost op.
_TOOL_CALL_TIMEOUT = 30.0

#: The manifest returned when a managed agent has NO tools bound: no discovery
#: sidecar and no tools.json are present, so there is simply nothing to discover.
#: "No tools" is a first-class agent (a plain chat agent), not a broken one, so
#: this is a valid empty manifest — not an error (spec console-runs U-run-manifest).
_EMPTY_MANIFEST: Dict[str, Any] = {"tools": []}


class Tool:
    """One entry of the discovery manifest (mcp-tools.md manifest shape).

    ``input_schema`` is the tool's argument JSON Schema as the manifest carries it
    (the ``inputSchema`` key), captured from the MCP server's ``tools/list`` and
    stored on the ToolRegistry entry (m14.6, plumbed through in m14.6b). It is the
    parsed JSON object *verbatim* — the managed loop advertises it to the model as
    the tool's ``parameters`` so the model produces correct ``arguments``. It is
    ``None`` when the manifest omits it (a curated/legacy entry with no captured
    schema); the loop then falls back to a permissive object-parameters schema.

    ``description`` is the tool's human-readable description as the manifest carries
    it (the ``description`` key, from the ToolRegistry entry — FUNC-10). The managed
    loop advertises it as the model's function ``description`` so the model selects a
    tool by what it does, not by name alone. ``""`` when the manifest omits it (a
    curated/legacy entry); the loop then synthesises a generic description.
    """

    __slots__ = ("name", "mode", "endpoint", "transport", "input_schema", "description")

    def __init__(
        self,
        name: str,
        mode: str,
        endpoint: str,
        transport: str,
        input_schema: Optional[Dict[str, Any]] = None,
        description: str = "",
    ):
        self.name = name
        self.mode = mode
        self.endpoint = endpoint
        self.transport = transport
        self.input_schema = input_schema
        self.description = description

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> Tool:
        # The manifest carries inputSchema verbatim as a JSON object. Anything
        # that is not a JSON object (absent, null, or a malformed non-object) is
        # treated as "no schema" so the loop takes the permissive fallback rather
        # than handing the model a schema it can't use.
        raw_schema = d.get("inputSchema")
        input_schema = raw_schema if isinstance(raw_schema, dict) else None
        raw_desc = d.get("description")
        description = raw_desc if isinstance(raw_desc, str) else ""
        return cls(
            name=d.get("name", ""),
            mode=d.get("mode", ""),
            endpoint=d.get("endpoint", ""),
            transport=d.get("transport", ""),
            input_schema=input_schema,
            description=description,
        )

    def __repr__(self) -> str:  # pragma: no cover - debug aid
        return f"Tool(name={self.name!r}, mode={self.mode!r}, endpoint={self.endpoint!r})"


class ToolsClient:
    """Tool discovery + invocation against the localhost plane."""

    def __init__(self, config: PlaneConfig):
        self._config = config

    # ── discovery ────────────────────────────────────────────────────────────
    def list(self) -> List[Tool]:
        """Return the live tool manifest (sidecar first, tools.json fallback).

        A team supervisor/member with a roster also gets the synthetic ``delegate_to`` (M64) +
        ``handoff_to`` (M67) tools alongside its MCP tools.

        An agent with knowledge bases bound (M68, ADR 0061 Fork 3) also gets the synthetic
        ``knowledge_search`` tool — enabled when KNOWLEDGE_BASE_ENABLED=true and the KNOWLEDGE_BASES
        roster is non-empty. A plain agent (no KB binding) never sees it.
        """
        from ctxmesh.knowledge import _knowledge_enabled, _roster_names  # avoid circular at import

        manifest = self._fetch_manifest()
        tools = [Tool.from_dict(t) for t in manifest.get("tools", [])]
        if _delegate_enabled():
            tools.append(_delegate_tool())
            tools.append(_handoff_tool())
        if _knowledge_enabled() and _roster_names():
            tools.append(_knowledge_search_tool())
        return tools

    def _fetch_manifest(self) -> Dict[str, Any]:
        url = f"{self._config.discovery_base_url}/tools"
        try:
            resp = _http.request("GET", url, timeout=_MANIFEST_TIMEOUT, expect=(200,))
            data = resp.json()
            if isinstance(data, dict):
                return data
        except EndpointError:
            # Sidecar unreachable — fall through to the durable backing file.
            pass

        # Durable backing: the ConfigMap mount (cold-start only).
        try:
            with open(self._config.tools_json_path) as fh:
                data = json.load(fh)
            if isinstance(data, dict):
                return data
        except FileNotFoundError:
            # Neither a discovery sidecar (localhost:2999) NOR a tools.json is
            # present. For a managed agent with NO tools bound this is the
            # EXPECTED state, not a failure: the controller injects the discovery
            # sidecar + tools ConfigMap only when the agent has bindings
            # (toolinject.go `hasBindings`), so a zero-tools agent has nothing to
            # discover. Return an empty manifest so the managed loop advertises no
            # tools and answers like a plain chat agent (managed.py: "Absent/empty
            # is fine"). A tools-bound agent always has the ConfigMap mounted, so
            # this branch never silently drops its tools.
            return _EMPTY_MANIFEST
        except (OSError, json.JSONDecodeError):
            # A tools.json that EXISTS but cannot be read or parsed (bad mount,
            # corrupt JSON) is a genuinely broken manifest — surface it loudly
            # rather than silently running tool-less.
            pass

        raise EndpointError(
            f"tool manifest unavailable: {url} was unreachable and "
            f"{self._config.tools_json_path} could not be read"
        )

    def _find(self, name: str) -> Tool:
        for tool in self.list():
            if tool.name == name:
                return tool
        raise ConfigError(f"tool {name!r} is not in the current manifest")

    # ── invocation ───────────────────────────────────────────────────────────
    def call(self, name: str, *, timeout: Optional[float] = None, **args: Any) -> Any:
        """Invoke a bound MCP tool by its *catalog* name; return its result.

        *name* is the discovery-manifest catalog key. The client resolves the
        endpoint from the manifest, then resolves the real MCP tool name via the
        server's ``tools/list`` (catalog names may differ from MCP names — e.g.
        ``word-count`` vs ``word_count``) before issuing ``tools/call``. The
        tool's text result is returned parsed as JSON when it is a JSON document,
        else as the raw string.

        *timeout* (m65.7, ADR 0058) is a keyword-only per-tool-call socket timeout
        in seconds, plumbed to every MCP round-trip of this call; ``None`` (the
        default) keeps the historical :data:`_TOOL_CALL_TIMEOUT`. The managed loop's
        per-turn resilience (``ManagedConfig.resilience.toolCall.timeoutSeconds``)
        supplies it. It is keyword-only so it can never collide with a tool
        argument literally named ``timeout``.
        """
        tool = self._find(name)
        if not tool.endpoint:
            raise ConfigError(f"tool {name!r} has no endpoint in the manifest")
        raw_text = _mcp_call_tool(tool.endpoint, name, args, timeout=timeout)
        try:
            return json.loads(raw_text)
        except (json.JSONDecodeError, TypeError):
            return raw_text

    def delegate(
        self,
        sub_agent: str,
        task: str,
        step: str,
        call_id: str,
        *,
        suspend: bool = False,
        spawn_root: str = "",
        spawn_depth: int = -1,
    ) -> Dict[str, Any]:
        """Delegate a subtask to a roster sub-agent via the launcher-local endpoint (M64, ADR 0057).

        The launcher applies the spawn guard, starts the sub-agent as a durable SUB-RUN, waits for
        it to finish, and returns ``{"ok": bool, "answer": str, "error": str}``. The invoking user's
        run capability is relayed so the sub-run acts on-behalf-of the SAME user (no re-consent).
        ``step`` + ``call_id`` are the idempotency key — a reclaimed supervisor re-issuing the same
        call resolves the SAME sub-run. A denial or a failure comes back as ``ok=false`` (an outcome
        the model reads), never an exception — the loop threads it as the tool result.

        *suspend* (L7, ADR 0091) asks the launcher, at depth 0, for a durable-suspend SIGNAL instead
        of a blocking await: the launcher budget-checks + resolves the target endpoint and returns
        ``{"ok": true, "suspend": true, "endpoint": ...}`` WITHOUT spawning or blocking — the caller
        (the managed loop) then suspends and the BFF worker creates the sub-run. An older launcher
        that doesn't know the flag simply blocks and returns a normal ``{ok, answer}`` — the caller
        detects the missing ``suspend`` and threads that result inline (graceful mixed-version
        fallback). *spawn_root* / *spawn_depth* relay this run's spawn-tree position so the launcher's
        depth gate (suspension is depth-0 only) and spawn guard key on the AUTHORITATIVE root.
        """
        payload: Dict[str, Any] = {
            "subAgent": sub_agent,
            "input": task,
            "step": step,
            "callId": call_id,
        }
        if suspend:
            payload["suspend"] = True
        body = json.dumps(payload).encode()
        headers = {"Content-Type": "application/json"}
        capability = current_capability()
        if capability:
            headers[CAPABILITY_HEADER] = capability
        # Relay the spawn-tree position (m108.5): the launcher's depth gate + guard key are otherwise
        # blind to SDK-driven delegations (they default to depth 0 / root ""), making the depth-0
        # suspension gate vacuous. Only relayed when known (a root supervisor passes spawn_depth=0).
        if spawn_depth >= 0:
            headers[SPAWN_DEPTH_HEADER] = str(spawn_depth)
        if spawn_root:
            headers[SPAWN_ROOT_HEADER] = spawn_root
        resp = _http.request(
            "POST",
            _DELEGATE_ENDPOINT,
            body=body,
            headers=headers,
            timeout=_DELEGATE_TIMEOUT,
            expect=(200,),
        )
        data = resp.json()
        if isinstance(data, dict):
            return data
        return {"ok": False, "error": "malformed delegate response"}

    def handoff(
        self, target_agent: str, message: str = "", include_history: bool = True
    ) -> Dict[str, Any]:
        """Hand the conversation off to a roster member via the launcher-local endpoint (M67).

        This is a TRANSFER, not a delegation: the launcher fail-fast validates roster membership +
        relays the run capability to the BFF handoff edge, which TERMINATES this run and creates a
        NEW run for the target agent on the SAME conversation (OBO minted fresh for the conversation
        owner against the target's boundary — no capability transfer). There is NO await + NO result
        to consume — the target continues with the end user. Returns ``{"ok": bool, "runId": str,
        "sourceRun": str, "handedOffTo": str, "error": str}``. A refusal (a non-member target, a
        missing capability) comes back as ``ok=false`` (an outcome the loop records), never a raise.

        *include_history* (m83.6) defaults True (the target replays the full conversation history on
        the transfer turn — today's behavior). Pass False to hand off with *message* as a SUMMARY:
        the target skips the full-history replay on that first turn and starts from the summary.
        """
        body = json.dumps(
            {"targetAgent": target_agent, "message": message, "includeHistory": include_history}
        ).encode()
        headers = {"Content-Type": "application/json"}
        capability = current_capability()
        if capability:
            headers[CAPABILITY_HEADER] = capability
        resp = _http.request(
            "POST",
            _HANDOFF_ENDPOINT,
            body=body,
            headers=headers,
            timeout=_HANDOFF_TIMEOUT,
            expect=(200,),
        )
        data = resp.json()
        if isinstance(data, dict):
            return data
        return {"ok": False, "error": "malformed handoff response"}


# ── minimal MCP streamable-http client (stdlib only) ───────────────────────────
#
# streamable-http transport (mcp-tools.md): the client POSTs JSON-RPC to the
# endpoint. The server responds either application/json or a text/event-stream
# SSE frame ("data: <json>"). The sequence is:
#   1. POST initialize        -> result + an Mcp-Session-Id response header
#   2. POST notifications/initialized (notification; no id, no response body)
#   3. POST tools/list        -> the server's real tool names (to resolve the
#                                catalog name -> the MCP name)
#   4. POST tools/call        -> the tool result (with the resolved MCP name)
# We keep this deliberately small — just enough to discover + invoke a tool.


def _mcp_headers(session_id: Optional[str]) -> Dict[str, str]:
    headers = {
        "Content-Type": "application/json",
        # A streamable-http server may reply with either representation.
        "Accept": "application/json, text/event-stream",
    }
    if session_id:
        headers["Mcp-Session-Id"] = session_id
    # Relay the invoking user's run capability (ADR 0030 §3) on every tool-call egress so
    # the egress sidecar can resolve THAT user's credential. Bound per-request in a
    # ContextVar (managed.run_managed_loop); absent ⇒ an unattended run (org/public only).
    capability = current_capability()
    if capability:
        headers[CAPABILITY_HEADER] = capability
    # Relay the approval VOUCHER (ADR 0074 §3, m82.4) on every tool-call egress so the egress
    # sidecar forwards a require-approval tool the human GRANTED. Bound per-request in a ContextVar
    # (request_scope) from the resumed run's inbound X-Ctxmesh-Approval header; absent ⇒ no granted
    # require-approval tool (the sidecar returns 403 approval_required). The voucher is bound to one
    # {run, tool}; relaying it on every call is safe (a mismatched tool just 403s at the sidecar).
    voucher = current_approval_voucher()
    if voucher:
        headers[APPROVAL_HEADER] = voucher
    # Relay the record-mode capture toggle (M78, ADR 0071 §1/C1) on every tool-call egress —
    # the SAME request-scoped signal the model relay attaches (model.py). It lets the egress
    # sidecar capture this call's tool I/O (pre-injection request + verbatim upstream response)
    # into the run's replay fixture (TOOL channel). Bound per-request in a ContextVar
    # (run_managed_loop / request_scope); absent ⇒ a non-recorded run — omit it, capture nothing.
    record_run_id = current_record_run_id()
    if record_run_id:
        headers[RECORD_HEADER] = record_run_id
    return headers


def _raise_if_consent_required(exc: EndpointError) -> None:
    """Re-raise *exc* as a ConsentRequiredError naming the server when it is the egress
    sidecar's structured consent_required (a 403 whose JSON body carries
    ``{"error":"consent_required","server":...}``); otherwise return so the caller re-raises
    the original error. String-free detection — keys on the status + machine-readable code."""
    if exc.status != 403 or not exc.body:
        return
    try:
        parsed = json.loads(exc.body)
    except (ValueError, TypeError):
        return
    if isinstance(parsed, dict) and parsed.get("error") == "consent_required":
        server = str(parsed.get("server", ""))
        raise ConsentRequiredError(
            f"consent required: connect your account for MCP server {server!r}",
            server=server,
            status=exc.status,
            body=exc.body,
        )


def _raise_if_approval_required(exc: EndpointError) -> None:
    """Re-raise *exc* as an ApprovalRequiredError when it is the egress sidecar's structured
    ``approval_required`` (ADR 0074 §3, m82.4) — a 403 whose JSON body carries
    ``{"error":"approval_required","tool":...,"run":...}``; otherwise return so the caller
    re-raises the original error. String-free detection — keys on the status + machine-readable.

    This makes the WIRE the enforcement point for require-approval even inside the managed loop: a
    require-approval tool that reaches egress WITHOUT a valid voucher pauses the run for a human
    (the same requires_action outcome pause_for_approval produces). A CUSTOM loop that ignores the
    raise is simply denied — the floor. The key mirrors the managed loop's ``tool:<name>`` so an
    approval resolves the SAME decision point on resume."""
    if exc.status != 403 or not exc.body:
        return
    try:
        parsed = json.loads(exc.body)
    except (ValueError, TypeError):
        return
    if isinstance(parsed, dict) and parsed.get("error") == "approval_required":
        tool = str(parsed.get("tool", ""))
        raise ApprovalRequiredError(
            f"approval required for tool {tool!r}",
            key=f"tool:{tool}",
            summary=f"Approve tool {tool!r}?",
        )


def _mcp_post(
    endpoint: str,
    payload: Dict[str, Any],
    session_id: Optional[str],
    *,
    expect_body: bool,
    timeout: float = _TOOL_CALL_TIMEOUT,
) -> tuple[Optional[Dict[str, Any]], Optional[str]]:
    """POST one JSON-RPC message; return (parsed-result, session-id-header).

    *timeout* (m65.7) is the per-request socket timeout; it defaults to the
    historical :data:`_TOOL_CALL_TIMEOUT` so an un-plumbed caller is unchanged.
    """
    try:
        resp = _http.request(
            "POST",
            endpoint,
            body=_http.json_body(payload),
            headers=_mcp_headers(session_id),
            timeout=timeout,
            expect=(200, 202),
        )
    except EndpointError as exc:
        # The egress sidecar may answer any forwarded request with a structured 403 — turn each into
        # a distinct, catchable outcome: consent_required (ADR 0029 §2 / m25.9 — connect your
        # account) or approval_required (ADR 0074 §3 / m82.4 — a human must approve this tool).
        _raise_if_consent_required(exc)
        _raise_if_approval_required(exc)
        raise
    new_session = resp.headers.get("mcp-session-id")
    if not expect_body:
        return None, new_session
    message = _parse_jsonrpc(resp)
    if "error" in message:
        err = message["error"]
        raise EndpointError(
            f"MCP error from {endpoint}: {err.get('message', err)}",
            status=resp.status,
        )
    return message.get("result"), new_session


def _parse_jsonrpc(resp: _http.Response) -> Dict[str, Any]:
    """Extract the JSON-RPC message from a JSON or SSE (text/event-stream) body."""
    content_type = resp.headers.get("content-type", "")
    text = resp.text()
    if "text/event-stream" in content_type:
        # SSE frames: lines beginning "data:"; take the last data payload.
        data_line = None
        for line in text.splitlines():
            if line.startswith("data:"):
                data_line = line[len("data:") :].strip()
        if data_line is None:
            raise EndpointError("MCP SSE response contained no data frame")
        text = data_line
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise EndpointError(f"MCP response was not valid JSON: {exc}") from exc


def _mcp_call_tool(
    endpoint: str,
    catalog_name: str,
    arguments: Dict[str, Any],
    *,
    timeout: Optional[float] = None,
) -> str:
    """Full MCP session: handshake -> tools/list -> resolve name -> tools/call.

    *catalog_name* is the discovery-manifest key; the actual MCP tool name is
    discovered from the server and may differ (see the module docstring). Returns
    the first text content item of the tool result.

    *timeout* (m65.7, ADR 0058) is the per-round-trip socket timeout applied to
    every POST of this session; ``None`` keeps the historical
    :data:`_TOOL_CALL_TIMEOUT` so an un-plumbed caller is byte-for-byte unchanged.
    """
    call_timeout = (
        timeout if isinstance(timeout, (int, float)) and timeout > 0 else _TOOL_CALL_TIMEOUT
    )
    # 1. initialize.
    init_result, session_id = _mcp_post(
        endpoint,
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": _MCP_PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "ctxmesh", "version": "0.1.0"},
            },
        },
        session_id=None,
        expect_body=True,
        timeout=call_timeout,
    )
    _ = init_result  # negotiated capabilities unused for a single tools/call

    # 2. notifications/initialized (a notification: no id, no response expected).
    _mcp_post(
        endpoint,
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        session_id=session_id,
        expect_body=False,
        timeout=call_timeout,
    )

    # 3. tools/list -> discover the server's real tool names.
    list_result, _ = _mcp_post(
        endpoint,
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
        session_id=session_id,
        expect_body=True,
        timeout=call_timeout,
    )
    server_names = _server_tool_names(list_result, endpoint)
    mcp_name = _resolve_tool_name(catalog_name, server_names, endpoint)

    # 4. tools/call with the RESOLVED MCP name.
    result, _ = _mcp_post(
        endpoint,
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": mcp_name, "arguments": arguments},
        },
        session_id=session_id,
        expect_body=True,
        timeout=call_timeout,
    )

    return _first_text_content(result, endpoint)


def _server_tool_names(list_result: Optional[Dict[str, Any]], endpoint: str) -> List[str]:
    """Extract the tool-name list from an MCP tools/list result."""
    if not isinstance(list_result, dict):
        raise EndpointError(f"MCP tools/list at {endpoint} returned no result object")
    tools = list_result.get("tools")
    if not isinstance(tools, list):
        raise EndpointError(f"MCP tools/list at {endpoint} returned no tools array")
    names = [t.get("name", "") for t in tools if isinstance(t, dict) and t.get("name")]
    if not names:
        raise EndpointError(f"MCP server at {endpoint} advertises no tools")
    return names


def _normalize(name: str) -> str:
    """Fold hyphen/underscore so catalog `word-count` matches MCP `word_count`."""
    return name.replace("-", "_")


def _resolve_tool_name(catalog_name: str, server_names: List[str], endpoint: str) -> str:
    """Map a catalog name to a server MCP tool name.

    Precedence: exact match -> hyphen/underscore-normalized match -> if the
    server advertises exactly one tool, use it -> otherwise raise a clear error
    listing what the server actually exposes.
    """
    # 1. Exact.
    if catalog_name in server_names:
        return catalog_name
    # 2. Normalized (hyphen<->underscore). Only accept an unambiguous match.
    target = _normalize(catalog_name)
    normalized_matches = [n for n in server_names if _normalize(n) == target]
    if len(normalized_matches) == 1:
        return normalized_matches[0]
    # 3. Sole-tool fallback.
    if len(server_names) == 1:
        return server_names[0]
    # 4. Give up with an actionable error.
    raise ConfigError(
        f"tool {catalog_name!r} could not be resolved to an MCP tool at "
        f"{endpoint}: the server exposes {server_names!r}. "
        f"(The discovery-catalog name may differ from the MCP tool name; a "
        f"normalized match was ambiguous or absent.)"
    )


def _first_text_content(result: Optional[Dict[str, Any]], endpoint: str) -> str:
    """Pull the first text content item out of an MCP CallToolResult."""
    if not isinstance(result, dict):
        raise EndpointError(f"MCP tools/call at {endpoint} returned no result object")
    content = result.get("content")
    if not isinstance(content, list) or not content:
        raise EndpointError(f"MCP tools/call at {endpoint} returned empty content")
    first = content[0]
    if isinstance(first, dict) and "text" in first:
        return first["text"]
    raise EndpointError(f"MCP tools/call at {endpoint} returned non-text content")
