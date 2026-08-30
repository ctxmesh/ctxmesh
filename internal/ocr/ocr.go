/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ocr is the client seam for the offline OCR service (M140.5, ADR 0119): the run-worker's ingestion
// executor calls it when a PDF has no text layer (a scanned/image-only PDF). It lives OUTSIDE internal/ingest
// on purpose — internal/ingest is a network-free, stateless extract library (its package doctrine); the HTTP
// call composes in the executor, not in Extract.
package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OCR extracts text from a scanned (image-only) PDF via a self-hosted, OFFLINE OCR service (tesseract +
// poppler; examples/ocr-service). It is an ENHANCEMENT over the born-digital extractor — a failure falls back
// to the honest PartiallyIngested state, never failing the ingestion.
type OCR interface {
	OCRPDF(ctx context.Context, data []byte) (string, error)
}

type ocrResponse struct {
	Text  string `json:"text"`
	Pages int    `json:"pages"`
}

// httpOCR posts the raw PDF bytes to the OCR service's POST /ocr and returns the extracted text. Raw body (not
// multipart, not base64): a single-purpose internal endpoint with one file — no base64 33% bloat, no multipart
// framing. Called directly (in-cluster service DNS), not via the gateway — an internal stage of the extract
// pipeline, not a model call.
type httpOCR struct {
	baseURL string
	client  *http.Client
}

// NewHTTPOCR builds an OCR client over the service at baseURL. A nil client gets a default with a GENEROUS
// timeout — a 50-page scan is minutes, not seconds. Callers should bound OCR under the per-document budget so a
// slow OCR degrades one doc, not the run.
func NewHTTPOCR(baseURL string, client *http.Client) OCR {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &httpOCR{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (o *httpOCR) OCRPDF(ctx context.Context, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/ocr", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}
	req.Header.Set("Content-Type", "application/pdf")
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call ocr service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ocr service status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out ocrResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode ocr response: %w", err)
	}
	return out.Text, nil
}
