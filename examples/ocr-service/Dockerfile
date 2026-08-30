# ocr-service — self-contained, OFFLINE OCR for scanned PDFs (M140.5). Tesseract +
# poppler (for pdf2image) are system packages baked into the image, so the pod runs
# fully offline (no external key, no paid API) — matching the embedder/reranker pattern.
FROM python:3.11-slim

# tesseract-ocr = the OCR engine; poppler-utils = pdftoppm/pdftocairo that pdf2image shells out to.
RUN apt-get update \
    && apt-get install -y --no-install-recommends tesseract-ocr poppler-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
ENV PIP_NO_CACHE_DIR=1

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py ./

EXPOSE 8080
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080"]
