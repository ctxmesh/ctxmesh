# real-embedder — a genuine, self-contained, OFFLINE semantic embedder for the dev
# cluster's RAG path (M140; reconstructed from the out-of-band m117 image so it is
# reproducible from the tree). fastembed (ONNX, no torch) serving all-MiniLM-L6-v2
# behind an OpenAI-compatible /v1/embeddings API — no external key, no HF fetch at
# runtime (the model is BAKED IN at build time).
#
# Full python:3.11 (not -slim) on purpose: a fastembed dep (PyStemmer) compiles a C
# extension, which needs the build toolchain the slim image lacks.
FROM python:3.11

WORKDIR /app
ENV FASTEMBED_CACHE=/app/models \
    PIP_NO_CACHE_DIR=1 \
    EMBED_MODEL=sentence-transformers/all-MiniLM-L6-v2

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# Bake the model into the image so runtime is fully offline (no HuggingFace fetch on
# first request). This is the ONLY build step that needs network; the pod does not.
RUN python -c "from fastembed import TextEmbedding; import os; TextEmbedding(os.environ['EMBED_MODEL'], cache_dir=os.environ['FASTEMBED_CACHE'])"

COPY server.py ./

EXPOSE 8080
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080"]
