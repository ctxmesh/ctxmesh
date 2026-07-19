"""Model client — the OpenAI-compatible model gateway (M2/M8).

``client.model.chat(model, messages, **opts)`` POSTs to
``$MODEL_GATEWAY_URL/chat/completions`` (the LiteLLM gateway, or transparently the
launcher's in-pod budget proxy when the agent is budgeted — same wire either way,
cost-governance.md) and returns the assistant's completion text.

Around every call it emits an OpenInference **``LLM``** span
(``openinference.span.kind = LLM``) carrying ``llm.model_name`` and the token
counts from the response ``usage`` block — the same shape the base-image
OpenAI/LangChain auto-instrumentation produces. So a custom loop's model call is
a structurally identical ``LLM`` node in the trace tree, and it nests under the
current ``client.trace.step`` (via OTel context) automatically. Being a child of
the active step means it also roots under ``agent.invoke`` when the loop bound the
request context.

Wire contract (examples/echo-agent/main.go, model-gateway.md):

    POST $MODEL_GATEWAY_URL/chat/completions
      { "model": <route>, "messages": [ {"role","content"}, ... ], ...opts }
    -> 200 { "choices":[{"message":{"content": <text>}}],
             "usage":{"prompt_tokens","completion_tokens","total_tokens"} }

A non-2xx (e.g. the budget proxy's 402 over-cap, or a 502 upstream) surfaces as an
:class:`~ctxmesh.errors.EndpointError` carrying the status — never swallowed.
"""

from __future__ import annotations

import json
from typing import Any, Dict, Iterator, List

from ctxmesh import _http, _semconv
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError
from ctxmesh.trace import TraceClient

#: A model round-trip is a remote provider call — generous vs the localhost ops.
_CHAT_TIMEOUT = 60.0


class ChatResponse:
    """The parsed result of ``model.chat`` — the completion text plus usage.

    ``text`` is the assistant message content (the common case a loop wants). On
    a **tool-calling turn** the OpenAI wire sets ``content: null`` and carries a
    ``tool_calls`` array instead; ``text`` is then ``""`` and :meth:`tool_calls`
    returns the calls. ``usage`` is the raw ``usage`` block (prompt/completion/
    total token counts) when the gateway returned one, else ``{}``. ``raw`` is the
    full decoded response for callers that need more (multiple choices, the
    assistant message with its ``tool_calls``, finish reason, …).
    """

    __slots__ = ("text", "usage", "model", "raw")

    def __init__(self, text: str, usage: Dict[str, Any], model: str, raw: Dict[str, Any]):
        self.text = text
        self.usage = usage
        self.model = model
        self.raw = raw

    def __str__(self) -> str:  # so `str(resp)` / logging gives the completion
        return self.text

    @property
    def message(self) -> Dict[str, Any]:
        """The raw assistant message object (``choices[0].message``), or ``{}``.

        The managed loop appends this verbatim to the running ``messages`` list on
        a tool-calling turn — OpenAI requires the assistant message (with its
        ``tool_calls``) to precede the ``role: "tool"`` results on the follow-up
        request.
        """
        return _assistant_message(self.raw)

    @property
    def tool_calls(self) -> List[Dict[str, Any]]:
        """The assistant's ``tool_calls`` for this turn (``[]`` when none).

        Each entry is the OpenAI tool-call object
        ``{"id", "type", "function": {"name", "arguments": <json-string>}}`` — the
        exact shape the m14.2 tool-call mock emits on turn 1. Off ``raw`` so the
        managed loop can dispatch without re-parsing the body. ``[]`` (never
        raises) when the turn is a plain text completion.
        """
        message = _assistant_message(self.raw)
        calls = message.get("tool_calls")
        if isinstance(calls, list):
            return [c for c in calls if isinstance(c, dict)]
        return []

    @property
    def has_tool_calls(self) -> bool:
        """True when this turn asked to call one or more tools."""
        return len(self.tool_calls) > 0


class ModelClient:
    """Chat-completion calls against the model gateway, emitting an LLM span."""

    def __init__(self, config: PlaneConfig, trace: TraceClient):
        self._config = config
        self._trace = trace

    def chat(
        self,
        model: str,
        messages: List[Dict[str, Any]],
        **opts: Any,
    ) -> ChatResponse:
        """POST a chat completion; return a :class:`ChatResponse`.

        *model* is the gateway route (e.g. ``gpt-4o-mini``); *messages* is the
        OpenAI-style ``[{"role","content"}, ...]`` list; ``**opts`` are extra body
        fields passed through verbatim (``temperature``, ``max_tokens``, …). Emits
        an OpenInference ``LLM`` span with the model name and — from the response
        ``usage`` — the prompt/completion/total token counts.

        Raises :class:`ConfigError` when the gateway URL is not wired (not in a
        pod / no ``MODEL_GATEWAY_URL``) and :class:`EndpointError` (with ``.status``)
        for a non-200 gateway response (budget 402, upstream 502, …).
        """
        base_url = self._config.model_gateway_url
        if not base_url:
            raise ConfigError(
                "model gateway is not wired: MODEL_GATEWAY_URL is unset. The "
                "launcher injects it in-pod; for offline use set it on the "
                "PlaneConfig (agent.from_config)."
            )
        if not isinstance(messages, list):
            raise ConfigError("model.chat expects messages as a list of {role,content} dicts")

        # `timeout` is a client-side transport concern, not an OpenAI body field:
        # pop it out of the pass-through opts so it never leaks into the request.
        body_opts = dict(opts)
        raw_timeout = body_opts.pop("timeout", None)
        timeout = raw_timeout if isinstance(raw_timeout, (int, float)) else _CHAT_TIMEOUT

        payload: Dict[str, Any] = {"model": model, "messages": messages}
        payload.update(body_opts)

        # LLM span wraps the round-trip so it appears (and nests) exactly like an
        # auto-instrumented framework LLM span. Input is the messages list.
        with self._trace.llm(name=f"chat {model}", model=model, input=messages) as span:
            resp = _http.request(
                "POST",
                f"{base_url}/chat/completions",
                body=_http.json_body(payload),
                headers=self._headers(),
                timeout=timeout,
                expect=(200,),
            )
            data = resp.json()
            if not isinstance(data, dict):
                raise EndpointError(
                    f"model gateway returned a non-object body: {resp.text()[:200]}",
                    status=resp.status,
                )

            text = _completion_text(data)
            usage = data.get("usage") if isinstance(data.get("usage"), dict) else {}
            resolved_model = data.get("model") if isinstance(data.get("model"), str) else model

            # Stamp the OpenInference LLM attrs the auto-instrumentation would.
            span.set_output(text)
            span.set_attribute(_semconv.LLM_MODEL_NAME, resolved_model)
            _stamp_usage(span, usage)

            return ChatResponse(text=text, usage=usage, model=resolved_model, raw=data)

    def stream(
        self,
        model: str,
        messages: List[Dict[str, Any]],
        **opts: Any,
    ) -> Iterator[str]:
        """Yield the assistant's text DELTAS as they arrive (a streaming chat completion).

        Sets ``stream: true`` so the gateway (LiteLLM / OpenAI) returns Server-Sent Events
        (``data: {choices:[{delta:{content}}]}`` … ``data: [DONE]``); this parses each frame and
        yields ``delta.content`` chunks. Emits the same ``LLM`` span as :meth:`chat`, with the
        accumulated text as its output. Same errors as :meth:`chat` (gateway unwired → ConfigError;
        non-200 → EndpointError). Token streaming is the m32.7 source for the run event stream.
        """
        base_url = self._config.model_gateway_url
        if not base_url:
            raise ConfigError("model gateway is not wired: MODEL_GATEWAY_URL is unset.")
        if not isinstance(messages, list):
            raise ConfigError("model.stream expects messages as a list of {role,content} dicts")

        body_opts = dict(opts)
        raw_timeout = body_opts.pop("timeout", None)
        timeout = raw_timeout if isinstance(raw_timeout, (int, float)) else _CHAT_TIMEOUT
        payload: Dict[str, Any] = {"model": model, "messages": messages, "stream": True}
        payload.update(body_opts)

        with self._trace.llm(name=f"chat {model}", model=model, input=messages) as span:
            span.set_attribute(_semconv.LLM_MODEL_NAME, model)
            acc: List[str] = []
            for line in _http.stream(
                "POST",
                f"{base_url}/chat/completions",
                body=_http.json_body(payload),
                headers=self._headers(),
                timeout=timeout,
            ):
                delta = _sse_delta(line)
                if delta:
                    acc.append(delta)
                    yield delta
            span.set_output("".join(acc))

    def _headers(self) -> Dict[str, str]:
        # The gateway (LiteLLM / budget proxy) is OpenAI-compatible and expects a
        # bearer token; the launcher injects the master key in-pod. When absent
        # (offline/mock gateway) we send a harmless placeholder — the mock gateway
        # ignores it — rather than omit the header the real gateway requires.
        key = self._config.model_gateway_key or "sk-agent-engine"
        return {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {key}",
        }


def _first_choice(data: Dict[str, Any]) -> Dict[str, Any]:
    """Return ``choices[0]`` as a dict, raising if the response has no choices."""
    choices = data.get("choices")
    if not isinstance(choices, list) or not choices:
        raise EndpointError("model gateway response has no choices")
    first = choices[0]
    if not isinstance(first, dict):
        raise EndpointError("model gateway choice is not an object")
    return first


def _assistant_message(data: Dict[str, Any]) -> Dict[str, Any]:
    """Return ``choices[0].message`` as a dict (``{}`` when absent/malformed).

    Never raises — the tool_calls/message accessors on :class:`ChatResponse`
    degrade to empty rather than crash a loop reading a slightly-off body.
    """
    choices = data.get("choices")
    if not isinstance(choices, list) or not choices:
        return {}
    first = choices[0]
    if not isinstance(first, dict):
        return {}
    message = first.get("message")
    return message if isinstance(message, dict) else {}


def _sse_delta(line: str) -> str:
    """Extract the text delta from one SSE line of a streaming chat completion. A non-``data:``
    line, the ``[DONE]`` sentinel, malformed JSON, or a content-less delta yields ``""``."""
    if not line.startswith("data:"):
        return ""
    payload = line[len("data:") :].strip()
    if not payload or payload == "[DONE]":
        return ""
    try:
        obj = json.loads(payload)
        choices = obj.get("choices") or []
        content = (choices[0].get("delta") or {}).get("content")
    except (json.JSONDecodeError, ValueError, AttributeError, IndexError, TypeError):
        return ""
    return content if isinstance(content, str) else ""


def _completion_text(data: Dict[str, Any]) -> str:
    """Extract ``choices[0].message.content`` from an OpenAI-style response.

    A **tool-calling turn** legitimately has ``content: null`` and a
    ``tool_calls`` array (the m14.2 turn-1 shape): that is NOT an error — the
    managed loop reads the calls off :attr:`ChatResponse.tool_calls` and this
    returns ``""`` for the (absent) text. Only a response that has neither text
    NOR tool_calls is a malformed body and raises.
    """
    first = _first_choice(data)
    message = first.get("message")
    if isinstance(message, dict):
        content = message.get("content")
        if isinstance(content, str):
            return content
        # A tool-calling turn: content is null, tool_calls carries the intent.
        # Return "" (no text) rather than raising — the loop dispatches the calls.
        if message.get("tool_calls"):
            return ""
    # Some gateways/streamed shapes put the text on `text`; accept that too.
    if isinstance(first.get("text"), str):
        return first["text"]
    raise EndpointError("model gateway choice has no message content")


def _stamp_usage(span: Any, usage: Dict[str, Any]) -> None:
    """Copy OpenAI ``usage`` token counts onto the LLM span (OpenInference keys)."""
    mapping = {
        "prompt_tokens": _semconv.LLM_TOKEN_COUNT_PROMPT,
        "completion_tokens": _semconv.LLM_TOKEN_COUNT_COMPLETION,
        "total_tokens": _semconv.LLM_TOKEN_COUNT_TOTAL,
    }
    for src, attr in mapping.items():
        value = usage.get(src)
        if isinstance(value, int):
            span.set_attribute(attr, value)
