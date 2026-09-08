package ctxmesh

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

// The plane is on localhost, so these bound a hung sidecar rather than a slow network. Search
// gets longer: it may wait on an embedding call through the token-service.
const (
	defaultTimeout = 15 * time.Second
	searchTimeout  = 60 * time.Second
)

type transport struct {
	hc *http.Client
}

func newTransport() *transport {
	return &transport{hc: &http.Client{Timeout: defaultTimeout}}
}

// do issues one request and decodes into out (which may be nil to discard the body).
//
// The response body is read in FULL before decoding, so an error carries what the launcher
// actually said. Streaming the decode straight off the wire loses the message on failure,
// which is exactly when it is needed.
func (t *transport) do(ctx context.Context, method, url string, in, out any, headers map[string]string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("ctxmesh: encode request for %s: %w", url, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("ctxmesh: build request for %s: %w", url, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("ctxmesh: %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Path: pathOf(url), Body: strings.TrimSpace(string(raw))}
	}
	if readErr != nil {
		return fmt.Errorf("ctxmesh: read response from %s: %w", url, readErr)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ctxmesh: decode response from %s: %w", url, err)
	}
	return nil
}

// pathOf keeps the port out of error text: it is an implementation detail and it makes two
// otherwise-identical failures look different.
func pathOf(url string) string {
	if i := strings.Index(url, "//"); i >= 0 {
		if j := strings.Index(url[i+2:], "/"); j >= 0 {
			return url[i+2+j:]
		}
	}
	return url
}
