# real-embedder — offline semantic embedder for the dev-cluster RAG path

A tiny, self-contained embedding service that serves **real** semantic embeddings so
the Knowledge-Base ingestion + retrieval path is honest (a natural-language question
actually retrieves the right chunk). It replaces the dev cluster's deterministic
**hash mock** (no semantics).

- **Model:** `sentence-transformers/all-MiniLM-L6-v2` via [fastembed](https://github.com/qdrant/fastembed)
  (ONNX runtime — no torch), **baked into the image at build time** → the pod runs
  fully **offline** (no HuggingFace fetch, no external key, no paid API).
- **API:** OpenAI-compatible `POST /v1/embeddings` (so the model gateway's embedding
  route can point at it unchanged, ADR 0045) + `GET /healthz` (a readiness canary that
  actually embeds).
- **Dimensionality:** MiniLM is 384-dim, but `knowledge_chunks.embedding` is fixed at
  `vector(1536)` schema-wide (migration 0005). The service **L2-normalizes then
  zero-pads 384→1536** — mathematically lossless for cosine (zeros change neither the
  dot product nor the norm) — so the engine needs **zero** code changes. The
  "true dim 384, stored 1536-padded" fact is intentional.

MiniLM is a **symmetric** model — correct for this bare `/v1/embeddings` seam, which
can't distinguish an ingestion call from a query call (an asymmetric BGE/e5 model
wants a query prefix and would underperform here).

## Build + deploy (dev cluster)

```sh
make docker-build-real-embedder                         # → dev.local/real-embedder:m117
kind load docker-image dev.local/real-embedder:m117 --name agentry-dev
kubectl apply -f examples/real-embedder/deploy.yaml     # ns: agentry
```

The model gateway routes its embedding hop at `svc real-embedder.agentry:8080`; the
KB's `embeddingRoute` selects that route so ingestion + retrieval use real vectors.

> Scope: this is a **dev/CI** semantic backend (M140 — makes the live semantic-RAG
> proof reproducible without a paid embedding API). A production install points the
> embedding route at whatever managed/self-hosted embedder the operator runs.
