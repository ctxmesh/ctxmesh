# real-reranker — a self-contained, OFFLINE cross-encoder reranker for the RAG retrieval
# path (M140.2, ADR 0117). fastembed TextCrossEncoder (ONNX, no torch) serving
# ms-marco-MiniLM-L-6-v2 behind a Cohere-shape /v1/rerank — no external key, no HF fetch at
# runtime (the model is BAKED IN at build time).
#
# SEPARATE image from real-embedder on purpose (ADR 0117): TextCrossEncoder needs
# fastembed >= 0.4.2 but the embedder is pinned at 0.3.6 — isolating the pins avoids silently
# drifting already-ingested embeddings. Full python:3.11 (not -slim): a fastembed dep
# (PyStemmer) compiles a C extension needing the build toolchain.
FROM python:3.11

WORKDIR /app
ENV FASTEMBED_CACHE=/app/models \
    PIP_NO_CACHE_DIR=1 \
    RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# Bake the cross-encoder into the image so runtime is fully offline. This is the ONLY build
# step that needs network; the pod does not.
RUN python -c "from fastembed.rerank.cross_encoder import TextCrossEncoder; import os; TextCrossEncoder(model_name=os.environ['RERANK_MODEL'], cache_dir=os.environ['FASTEMBED_CACHE'])"

COPY server.py ./

EXPOSE 8080
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080"]
