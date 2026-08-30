"""real-reranker — a genuine local cross-encoder reranker behind a Cohere-shape /v1/rerank API.

M140.2 (ADR 0117): re-scores retrieval candidates with the ms-marco-MiniLM-L-6-v2 cross-encoder via
fastembed (ONNX, no torch) — self-contained + offline (the model is baked into the image at build time).

Two design points (see README + ADR 0117):
  1. SEPARATE image from the embedder ON PURPOSE. TextCrossEncoder needs fastembed >= 0.4.2, but the
     embedder is pinned at 0.3.6; bumping fastembed in the SHARED image could silently drift the vectors of
     already-ingested corpora. Isolating the reranker's pin keeps the embedder frozen.
  2. Cohere/Jina-standard wire shape (`/v1/rerank` → {results:[{index, relevance_score}]}, sorted desc) —
     the de-facto rerank standard (TEI, vLLM, LiteLLM speak it), so it's gateway-routable later unchanged.
"""

import os
from typing import List, Union

from fastapi import FastAPI
from fastembed.rerank.cross_encoder import TextCrossEncoder
from pydantic import BaseModel

MODEL_NAME = os.getenv("RERANK_MODEL", "Xenova/ms-marco-MiniLM-L-6-v2")
CACHE_DIR = os.getenv("FASTEMBED_CACHE", "/app/models")

# Loaded at import (baked into the image at build time — no runtime HuggingFace fetch).
_model = TextCrossEncoder(model_name=MODEL_NAME, cache_dir=CACHE_DIR)

app = FastAPI(title="real-reranker", version="m140")


class RerankRequest(BaseModel):
    query: str
    documents: List[str]
    model: Union[str, None] = None
    top_n: Union[int, None] = None


@app.get("/healthz")
def healthz() -> dict:
    # Readiness canary: prove the model actually scores (not just that the process is up).
    scores = list(_model.rerank("ready?", ["a readiness probe document"]))
    return {"ok": len(scores) == 1, "model": MODEL_NAME}


@app.post("/v1/rerank")
def rerank(req: RerankRequest) -> dict:
    if not req.documents:
        return {"model": req.model or MODEL_NAME, "results": []}
    # TextCrossEncoder.rerank returns index-aligned relevance scores (higher = more relevant).
    scores = list(_model.rerank(req.query, req.documents))
    ranked = sorted(
        ({"index": i, "relevance_score": float(s)} for i, s in enumerate(scores)),
        key=lambda r: r["relevance_score"],
        reverse=True,
    )
    if req.top_n is not None and req.top_n > 0:
        ranked = ranked[: req.top_n]
    return {"model": req.model or MODEL_NAME, "results": ranked}
