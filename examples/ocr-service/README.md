# ocr-service — offline OCR for scanned-PDF ingestion

A tiny, self-contained service that extracts text from **scanned (image-only) PDFs** so they become
retrievable. A scanned PDF has no text layer, so the born-digital extractor yields ~empty text and the
corpus goes `PartiallyIngested`; this service renders the pages and OCRs them, recovering the text. It is
M140's **multimodal dimension 1** (ADR 0119), opt-in, and — like the embedder/reranker — **self-hosted +
offline** (no paid API, no provisioning) so the whole M140 bar runs deterministically.

- **Engine:** [Tesseract](https://github.com/tesseract-ocr/tesseract) via `pytesseract`, with
  [poppler](https://poppler.freedesktop.org/) (`pdf2image`) rendering PDF pages to images — both system
  packages **baked into the image**, so the pod runs fully offline.
- **API:** `POST /ocr` with the raw PDF (or image) as the request body → `{"text": "..."}`; `GET /healthz`
  (a canary that proves tesseract is callable). Pages are capped (`MAX_PAGES=50`) so a pathological scan
  can't wedge a worker.
- **Called directly** (in-cluster service DNS) by the run-worker's ingestion executor — an internal stage of
  the extract pipeline, not a model call.

## Build + deploy (dev cluster)

```sh
make docker-build-ocr-service                          # → dev.local/ocr-service:m140
kind load docker-image dev.local/ocr-service:m140 --name ctxmesh-dev
kubectl apply -f examples/ocr-service/deploy.yaml      # ns: ctxmesh
```

Then activate the opt-in OCR fallback on the run-worker: `INGEST_OCR_URL=http://ocr-service.ctxmesh.svc:8080`.

> OCR runs **only** when a PDF's text layer is insufficient (the existing `MinSufficientChars` guard) — it's
> expensive, so it never runs on a born-digital PDF. If OCR also yields insufficient text (a truly blank
> page), the doc stays honestly `PartiallyIngested`.
>
> Scope: **dimension 1 = OCR/scanned-PDF**. Audio (Whisper) + image/vision are their own dimensions,
> deferred (carded m52).
