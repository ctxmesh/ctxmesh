"""ocr-service — extract text from a scanned (image-only) PDF via Tesseract OCR.

M140.5 (multimodal dimension 1): a scanned PDF has no text layer, so the born-digital extractor yields ~empty
text and the corpus goes PartiallyIngested. This service renders the PDF pages to images (pdf2image/poppler)
and OCRs them (pytesseract/tesseract) so the text becomes retrievable. Self-hosted + OFFLINE (tesseract +
poppler baked into the image) — matching the embedder/reranker pattern: no external key, no paid API.

Contract: POST /ocr with the raw PDF (or image) as the request body → {"text": "..."} (200). GET /healthz.
"""

import io

from fastapi import FastAPI, Request
from pdf2image import convert_from_bytes
from PIL import Image
import pytesseract

app = FastAPI(title="ocr-service", version="m140")

# Cap pages OCR'd per document — OCR is slow; a pathological 1000-page scan shouldn't wedge a worker.
MAX_PAGES = 50
# Render at 200 DPI — enough for OCR accuracy without ballooning per-page image memory.
DPI = 200


@app.get("/healthz")
def healthz() -> dict:
    # Prove tesseract is actually callable (not just the process up).
    return {"ok": True, "tesseract": pytesseract.get_tesseract_version().__str__()}


@app.post("/ocr")
async def ocr(request: Request) -> dict:
    data = await request.body()
    if not data:
        return {"text": "", "pages": 0}

    parts: list[str] = []
    pages = 0
    # PDF path: render + OCR PAGE-BY-PAGE (never rasterize the whole PDF into RAM at once — a big scan would
    # OOM the pod, Fable's headline gotcha). Loop first_page/last_page; free each page image before the next.
    page = 1
    while page <= MAX_PAGES:
        try:
            imgs = convert_from_bytes(data, dpi=DPI, first_page=page, last_page=page)
        except Exception:
            break
        if not imgs:
            break
        parts.append(pytesseract.image_to_string(imgs[0]))
        imgs[0].close()
        pages += 1
        page += 1

    # Not a PDF (or nothing rendered) → try a single raster image.
    if pages == 0:
        try:
            with Image.open(io.BytesIO(data)) as img:
                parts.append(pytesseract.image_to_string(img))
                pages = 1
        except Exception:
            return {"text": "", "pages": 0}

    return {"text": "\n\n".join(parts).strip(), "pages": pages}
