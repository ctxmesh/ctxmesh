# real-reranker — offline cross-encoder reranker for the RAG retrieval path

A tiny, self-contained rerank service that re-scores retrieval candidates with a **cross-encoder**
so the semantically-best chunks are promoted above the coarse fusion (RRF) rank. It's the
retrieval-quality stage of M140 (ADR 0117), opt-in, and — like the embedder — **self-hosted +
offline** (no paid API, no provisioning) so the whole M140 bar runs deterministically.

- **Model:** `Xenova/ms-marco-MiniLM-L-6-v2` via [fastembed](https://github.com/qdrant/fastembed)'s
  `TextCrossEncoder` (ONNX runtime — no torch), **baked into the image at build time** → the pod runs
  fully offline. This is the textbook rerank baseline (the MS MARCO cross-encoder sibling of the
  embedder's MiniLM).
- **API:** the de-facto-standard Cohere/Jina **`POST /v1/rerank`** — `{query, documents[], top_n?}` →
  `{results: [{index, relevance_score}]}` sorted descending — plus `GET /healthz` (a readiness canary
  that actually scores). Standards-first, so it's gateway-routable later without a client change.
- **Called directly** (in-cluster service DNS), **not via the model gateway**: rerank is an internal
  stage of our own retrieval pipeline, not an OpenAI-compatible model call.

## Why a separate image from real-embedder

`TextCrossEncoder` needs `fastembed >= 0.4.2`, but the embedder is pinned at `0.3.6`. Bumping fastembed
in a **shared** image risks silently drifting the vectors of already-ingested corpora (a fastembed/
onnxruntime bump can move embedding outputs). Isolating the reranker's pin keeps the embedder frozen —
the deliberate deviation from the embedder's one-service pattern, recorded in ADR 0117.

## Build + deploy (dev cluster)

```sh
make docker-build-real-reranker                          # → dev.local/real-reranker:m140
kind load docker-image dev.local/real-reranker:m140 --name ctxmesh-dev
kubectl apply -f examples/real-reranker/deploy.yaml      # ns: ctxmesh
```

Then activate the opt-in rerank stage on the token-service:
`KNOWLEDGE_RERANK_URL=http://real-reranker.ctxmesh.svc:8080` + `KNOWLEDGE_RERANK=true`.

> Rerank is **fail-open**: a dead/slow reranker (2s client timeout) never breaks retrieval — the
> handler falls back to the store's fusion order.
