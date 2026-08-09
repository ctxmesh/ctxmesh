"""Multimodal content-parts helpers — thin builders for OpenAI wire format (M68, ADR 0061 Fork 5).

These helpers assemble the content-parts list that ``model.chat`` accepts as the ``content``
field of a message. The model gateway and provider already relay content-parts verbatim — these
are purely client-side conveniences so users don't hand-assemble the wire format.

Usage::

    from ctxmesh import text_part, image_url, content

    messages = [
        {"role": "user", "content": content(
            text_part("What is in this image?"),
            image_url("https://example.com/photo.jpg"),
        )}
    ]
    response = client.model.chat("gpt-4o", messages)

Both ``https://`` URLs and ``data:`` URLs (base64-encoded inline images) are accepted —
pass them straight through; the gateway relays content-parts without inspecting the URL type.

Note: retrieved-IMAGE-chunk plumbing is deferred. v1 ingestion is text-only, so
``mimeType`` for knowledge-search results will be ``"text/plain"``. The helpers
here support user-supplied images (multimodal I/O), not retrieved image chunks.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union


def text_part(text: str) -> Dict[str, Any]:
    """Build an OpenAI ``text`` content part.

    Returns ``{"type": "text", "text": text}``.

        >>> text_part("Hello, world!")
        {'type': 'text', 'text': 'Hello, world!'}
    """
    return {"type": "text", "text": text}


def image_url(url: str, detail: Optional[str] = None) -> Dict[str, Any]:
    """Build an OpenAI ``image_url`` content part.

    *url* may be an ``https://`` URL or a ``data:image/...;base64,...`` inline URL.
    *detail* is the optional OpenAI detail level (``"low"``, ``"high"``, or ``"auto"``);
    omit it to let the provider use its default.

    Returns::

        {"type": "image_url", "image_url": {"url": url}}
        # or, with detail:
        {"type": "image_url", "image_url": {"url": url, "detail": detail}}

    Example::

        >>> image_url("https://example.com/photo.jpg")
        {'type': 'image_url', 'image_url': {'url': 'https://example.com/photo.jpg'}}

        >>> image_url("https://example.com/photo.jpg", detail="high")
        {'type': 'image_url', 'image_url': {'url': 'https://...',
        'detail': 'high'}}
    """
    inner: Dict[str, Any] = {"url": url}
    if detail is not None:
        inner["detail"] = detail
    return {"type": "image_url", "image_url": inner}


def content(*parts: Union[Dict[str, Any], List[Dict[str, Any]]]) -> List[Dict[str, Any]]:
    """Assemble a content-parts list from positional part dicts (or a single list).

    Accepts either:
    - multiple positional dicts (the common case): ``content(text_part("…"), image_url("…"))``
    - a single list argument: ``content([text_part("…"), image_url("…")])``

    Returns the assembled list for use as the ``content`` field of a message dict::

        messages = [{"role": "user", "content": content(
            text_part("Describe this image."),
            image_url("https://example.com/photo.jpg"),
        )}]

    Example::

        >>> content(text_part("hi"), image_url("https://x.com/img.png"))
        [{'type': 'text', 'text': 'hi'}, {'type': 'image_url', 'image_url': {'url': 'https://x.com/img.png'}}]
    """
    if len(parts) == 1 and isinstance(parts[0], list):
        return list(parts[0])
    result: List[Dict[str, Any]] = []
    for part in parts:
        if isinstance(part, list):
            result.extend(part)
        else:
            result.append(part)
    return result
