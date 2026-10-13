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

package prompt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// maxPromptBytes bounds a resolved prompt so a hostile/huge path cannot OOM the BFF.
// Prompts are text; 1 MiB is generous.
const maxPromptBytes = 1 << 20

// HTTPResolver is the production drop-in for the fixture Resolver (the same seam,
// ADR 0008): it fetches a git-pointer PromptVersion's file content over HTTPS from the
// git host's raw endpoint. Git remains the source of truth — the platform still stores
// NO prompt content; this only READS it on demand (for the diff / launcher materialise).
//
// v1 supports github.com (→ raw.githubusercontent.com), the common case; a generic
// go-git resolver would generalise to arbitrary hosts (documented-future, same
// interface). An optional bearer token (a git PAT, supplied out-of-band via a Secret —
// never committed) authorises PRIVATE repos; public repos need none.
type HTTPResolver struct {
	client *http.Client
	token  string
	// maxBytes, when set, refuses content longer than it with ErrTooLarge instead of truncating it.
	maxBytes int64
}

// ErrTooLarge is returned by a resolver built with HTTPResolverOptions.MaxBytes for content longer
// than that.
var ErrTooLarge = errors.New("prompt: the file is larger than the resolver accepts")

// NewHTTPResolver builds an HTTPResolver. token is optional (empty ⇒ public-repo only).
func NewHTTPResolver(token string) *HTTPResolver {
	return &HTTPResolver{
		client: &http.Client{Timeout: 15 * time.Second},
		token:  strings.TrimSpace(token),
	}
}

// HTTPResolverOptions tunes an HTTPResolver for a caller that fetches inside a reconcile.
type HTTPResolverOptions struct {
	// MaxBytes refuses content longer than this with ErrTooLarge. Zero truncates at 1 MiB, as
	// NewHTTPResolver does.
	MaxBytes int64
	// DialTimeout bounds connecting, and the TLS handshake and the wait for response headers each, so
	// an unreachable host fails in seconds. Zero leaves the transport's defaults.
	DialTimeout time.Duration
	// Timeout bounds the whole request. Zero is 15 seconds.
	Timeout time.Duration
}

// NewHTTPResolverWithOptions builds an HTTPResolver for public repositories only: it sends no token.
// The transport honours HTTPS_PROXY and NO_PROXY like http.DefaultTransport.
func NewHTTPResolverWithOptions(opts HTTPResolverOptions) *HTTPResolver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.DialTimeout > 0 {
		transport.DialContext = (&net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = opts.DialTimeout
		transport.ResponseHeaderTimeout = opts.DialTimeout
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &HTTPResolver{
		client:   &http.Client{Timeout: timeout, Transport: transport},
		maxBytes: opts.MaxBytes,
	}
}

// Resolve fetches the prompt content for the git pointer. A 404 (bad ref / missing
// path) maps to ErrNotFound (a user error, not a requeue); any other non-200 or a
// transport error is a transient/infra failure. Never fabricates content.
func (h *HTTPResolver) Resolve(ctx context.Context, src agentsv1beta1.GitPromptSource) (Resolved, error) {
	rawURL, err := rawContentURL(src)
	if err != nil {
		// An unsupported host is a user-fixable pointer error, not transient.
		return Resolved{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Resolved{}, fmt.Errorf("prompt: build request for %s: %w", rawURL, err)
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return Resolved{}, fmt.Errorf("prompt: fetch %s: %w", rawURL, err) // transient
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return Resolved{}, ErrNotFound
	default:
		return Resolved{}, fmt.Errorf("prompt: fetch %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	limit := int64(maxPromptBytes)
	if h.maxBytes > 0 {
		limit = h.maxBytes + 1 // one byte more tells a file at the limit from a longer one
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return Resolved{}, fmt.Errorf("prompt: read %s: %w", rawURL, err)
	}
	if h.maxBytes > 0 && int64(len(body)) > h.maxBytes {
		return Resolved{}, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, rawURL, h.maxBytes)
	}
	content := string(body)
	return Resolved{Content: content, Version: Version(src, content)}, nil
}

// rawContentURL maps a git pointer to the host's raw-file URL. github.com repos map to
// raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}. Other hosts are unsupported in
// v1 (a go-git resolver would generalise) — reported as a pointer error the caller maps
// to a user-facing 404.
func rawContentURL(src agentsv1beta1.GitPromptSource) (string, error) {
	repo := strings.TrimSpace(src.Repo)
	ref := strings.TrimSpace(src.Ref)
	path := strings.TrimPrefix(strings.TrimSpace(src.Path), "/")
	if repo == "" || ref == "" || path == "" {
		return "", fmt.Errorf("incomplete git pointer (repo=%q ref=%q path=%q)", src.Repo, src.Ref, src.Path)
	}
	repo = strings.TrimSuffix(repo, ".git")
	u, err := url.Parse(repo)
	if err != nil {
		return "", fmt.Errorf("unparseable repo URL %q: %w", src.Repo, err)
	}
	if !strings.EqualFold(u.Host, "github.com") {
		return "", fmt.Errorf("unsupported repo host %q (v1 supports github.com only)", u.Host)
	}
	ownerRepo := strings.Trim(u.Path, "/")
	if ownerRepo == "" || !strings.Contains(ownerRepo, "/") {
		return "", fmt.Errorf("repo URL %q is not github.com/{owner}/{repo}", src.Repo)
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", ownerRepo, ref, path), nil
}
