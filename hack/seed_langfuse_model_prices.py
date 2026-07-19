#!/usr/bin/env python3
"""Seed Langfuse with model-price definitions for the platform's ROUTE-NAMED models.

Why this exists
---------------
Langfuse computes an observation's cost from its model-price table by matching the
observation's `model` string against each definition's `matchPattern`. Agents request
models by their **ModelRoute name** — e.g. `anthropic-claude-sonnet-4-6` — and that
route name is what the LLM-SDK instrumentation records on the generation span. Langfuse
ships prices for the underlying `claude-sonnet-4-6` (and accepts an `anthropic/` slash or
`anthropic.` dot prefix), but NOT the platform's `anthropic-` HYPHEN-joined route name —
so every observation prices to $0 and the dashboard/Cost page show no spend.

This script registers a definition per route-named model so Langfuse can price it. Prices
mirror the underlying model (the same numbers already in Langfuse for `claude-sonnet-4-6`
et al., which in turn mirror the gateway budget pricer). It is IDEMPOTENT: a model whose
matchPattern already exists is skipped.

Note: Langfuse computes cost at INGESTION, so this affects NEW runs only — traces ingested
before seeding stay at their (0) cost. That is honest: re-pricing history is out of scope.

Usage
-----
    LANGFUSE_HOST=http://localhost:3999 \
    LANGFUSE_PUBLIC_KEY=pk-... LANGFUSE_SECRET_KEY=sk-... \
    python3 hack/seed_langfuse_model_prices.py

(Point LANGFUSE_HOST at a port-forward of langfuse-web, or the in-cluster URL. Creds are
the same ones the BFF uses — the `bff-adapters` secret.)

Keep MODELS in sync with the platform's ModelRoute catalog / gateway pricer when models
are added. Prices are USD per token (TOKENS unit): $3/1M input == 0.000003.
"""
from __future__ import annotations

import base64
import json
import os
import re
import sys
import urllib.error
import urllib.request

# Route-named models the platform emits, with per-token USD prices mirroring the
# underlying model. matchPattern anchors on the exact route name (case-insensitive) and
# also accepts the bare underlying name, so both spellings price identically.
MODELS = [
    # (route model name, underlying name, input $/token, output $/token)
    ("anthropic-claude-sonnet-4-6", "claude-sonnet-4-6", 0.000003, 0.000015),
    ("anthropic-claude-sonnet-5", "claude-sonnet-5", 0.000002, 0.000010),
    ("anthropic-claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", 0.000001, 0.000005),
    ("anthropic-claude-opus-4-8", "claude-opus-4-8", 0.000005, 0.000025),
]


def _match_pattern(route: str, underlying: str) -> str:
    """Case-insensitive anchored pattern matching the route name or the underlying name."""
    return rf"(?i)^({re.escape(route)}|(anthropic[-/.])?{re.escape(underlying)})$"


def _auth_header(pub: str, sec: str) -> str:
    return "Basic " + base64.b64encode(f"{pub}:{sec}".encode()).decode()


def _get(url: str, auth: str):
    req = urllib.request.Request(url, headers={"Authorization": auth, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as resp:  # noqa: S310 (trusted host)
        return json.load(resp)


def _post(url: str, auth: str, body: dict) -> int:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        url, data=data, method="POST",
        headers={"Authorization": auth, "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=15) as resp:  # noqa: S310
        return resp.status


def main() -> int:
    host = os.environ.get("LANGFUSE_HOST", "").rstrip("/")
    pub = os.environ.get("LANGFUSE_PUBLIC_KEY", "")
    sec = os.environ.get("LANGFUSE_SECRET_KEY", "")
    if not (host and pub and sec):
        print("LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY are required", file=sys.stderr)
        return 2
    auth = _auth_header(pub, sec)

    existing = {m.get("matchPattern") for m in _get(f"{host}/api/public/models?limit=100", auth).get("data", [])}

    created = skipped = 0
    for route, underlying, in_price, out_price in MODELS:
        pattern = _match_pattern(route, underlying)
        if pattern in existing:
            print(f"skip   {route} (already defined)")
            skipped += 1
            continue
        body = {
            "modelName": route,
            "matchPattern": pattern,
            "unit": "TOKENS",
            "inputPrice": in_price,
            "outputPrice": out_price,
        }
        try:
            status = _post(f"{host}/api/public/models", auth, body)
            print(f"create {route}  in=${in_price}/tok out=${out_price}/tok -> HTTP {status}")
            created += 1
        except urllib.error.HTTPError as e:  # pragma: no cover
            print(f"ERROR  {route}: HTTP {e.code} {e.read()[:200]!r}", file=sys.stderr)
            return 1

    print(f"\ndone: {created} created, {skipped} skipped")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
