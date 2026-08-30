"""real-embedder — a genuine local embedding model behind an OpenAI-compatible /v1/embeddings API.

The dev cluster's only embedder was a deterministic HASH mock (no semantics), which can't back an
HONEST RAG demo (a natural question wouldn't retrieve the right chunk). This serves REAL semantic
embeddings from all-MiniLM-L6-v2 via fastembed (ONNX, no torch) — self-contained, no external key.

Two design points (M117, ADR 0093-adjacent; see README):
  1. MiniLM is a SYMMETRIC model — correct for this bare /v1/embeddings seam, which can't tell an
     ingestion call from a query call (BGE/e5 want a query prefix and would underperform here).
  2. MiniLM is 384-dim but the pgvector column is FIXED at vector(1536) schema-wide
     (migrations/0005_knowledge_chunks.sql). We L2-normalize then ZERO-PAD 384→1536 — mathematically
     lossless for cosine (zeros change neither dot product nor norm) — so the engine needs ZERO code
     changes. The "true dim 384, stored 1536-padded" fact is intentional; don't "fix" it.
"""

import os
from typing import List, Union

import numpy as np
from fastapi import FastAPI
from fastembed import TextEmbedding
from pydantic import BaseModel

TARGET_DIM = 1536  # the pgvector knowledge_chunks.embedding column dimension (migration 0005)
MODEL_NAME = os.getenv("EMBED_MODEL", "sentence-transformers/all-MiniLM-L6-v2")
CACHE_DIR = os.getenv("FASTEMBED_CACHE", "/app/models")

# Loaded at import (baked into the image at build time — no runtime HuggingFace fetch).
_model = TextEmbedding(MODEL_NAME, cache_dir=CACHE_DIR)

app = FastAPI(title="real-embedder", version="m117")


class EmbeddingRequest(BaseModel):
    input: Union[str, List[str]]
    model: Union[str, None] = None


def _embed(texts: List[str]) -> List[List[float]]:
    out: List[List[float]] = []
    for vec in _model.embed(texts):
        v = np.asarray(vec, dtype=np.float32)
        norm = float(np.linalg.norm(v))
        if norm > 0.0:
            v = v / norm  # L2-normalize → cosine == dot product
        padded = np.zeros(TARGET_DIM, dtype=np.float32)
        padded[: v.shape[0]] = v  # zero-pad 384 → 1536 (lossless for cosine)
        out.append(padded.tolist())
    return out


@app.get("/healthz")
def healthz() -> dict:
    # Readiness canary: prove the model actually embeds (not just that the process is up).
    v = _embed(["ready?"])[0]
    return {"ok": len(v) == TARGET_DIM, "model": MODEL_NAME, "dim": TARGET_DIM}


@app.post("/v1/embeddings")
def embeddings(req: EmbeddingRequest) -> dict:
    texts = [req.input] if isinstance(req.input, str) else list(req.input)
    vectors = _embed(texts)
    data = [
        {"object": "embedding", "index": i, "embedding": vec} for i, vec in enumerate(vectors)
    ]
    return {
        "object": "list",
        "data": data,
        "model": req.model or MODEL_NAME,
        "usage": {"prompt_tokens": 0, "total_tokens": 0},
    }
