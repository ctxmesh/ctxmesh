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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// Sender-constrained run capabilities, launcher side (ADR 0124).
//
// The BFF mints a run capability before this pod has a key, so the launcher exchanges it, once, for one
// bound to a key that exists only in this process's memory. Every call the launcher then makes to a BFF
// edge carries a proof signed by that key, so a copy of the capability — from a log, a trace, a crash
// dump — cannot be spent.
//
// The exchange is single-use: whoever binds first owns the capability. The launcher binds at its front
// door, before the request reaches the agent's code, so that owner is never the agent's code.

// errRuncapClaimed means another holder bound this run's capability first. The capability has leaked, and
// this launcher can no longer spend it.
var errRuncapClaimed = errors.New("this run's capability was bound by another holder")

const (
	runcapBindPath    = "/api/internal/runcap/bind"
	runcapBindTimeout = 5 * time.Second
	// runcapBindRetryAfter is how long a BFF without the bind endpoint is left alone before asking again,
	// so an install that cannot bind costs one round trip a minute, not one per invoke.
	runcapBindRetryAfter = time.Minute
	// runcapBindBackoff does the same after the BFF could not be reached: a network policy that drops the
	// packets would otherwise cost every invoke the full front-door timeout.
	runcapBindBackoff = 30 * time.Second
	// runcapFrontDoorTimeout bounds the bind on the invoke path. On expiry the capability passes through
	// unbound and the BFF's posture decides what it may still do.
	runcapFrontDoorTimeout = 2 * time.Second
)

// runcapBinder holds this process's sender key and the capabilities it has bound.
type runcapBinder struct {
	signer  *runcap.ProofSigner
	bffHost string // the only host bound capabilities and proofs are sent to
	bindURL string
	hc      *http.Client
	now     func() time.Time
	logf    func(string, ...any)

	mu        sync.Mutex
	bound     map[string]boundCapability // sha256(bearer) -> its bound replacement
	skipUntil time.Time                  // no bind attempts before this (BFF without the exchange, or unreachable)
}

type boundCapability struct {
	token string
	exp   time.Time
}

// newRuncapBinder returns nil when there is no BFF to bind with: no BFF edge is reachable then either.
func newRuncapBinder(bffURL string, logf func(string, ...any)) (*runcapBinder, error) {
	bffURL = strings.TrimRight(strings.TrimSpace(bffURL), "/")
	if bffURL == "" {
		return nil, nil
	}
	u, err := url.Parse(bffURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("BFF_INTERNAL_URL %q is not a URL", bffURL)
	}
	signer, err := runcap.NewProofSigner()
	if err != nil {
		return nil, err
	}
	return &runcapBinder{
		signer:  signer,
		bffHost: u.Host,
		bindURL: bffURL + runcapBindPath,
		hc:      &http.Client{Timeout: runcapBindTimeout, CheckRedirect: refuseRedirect},
		now:     time.Now,
		logf:    logf,
		bound:   map[string]boundCapability{},
	}, nil
}

// Bind returns the capability to use in place of tok. A bearer capability is exchanged for one bound to
// this process's key; a capability already bound (to this key, or relayed bound to another holder's) is
// returned unchanged. When the BFF offers no exchange, or is unreachable, tok is returned unchanged and
// the BFF's own posture decides whether a bearer capability is still accepted. The one refusal is
// errRuncapClaimed.
func (b *runcapBinder) Bind(ctx context.Context, tok string) (string, error) {
	u, err := runcap.InspectUnverified(tok)
	if err != nil || u.KeyThumbprint != "" {
		return tok, nil
	}
	sum := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(sum[:])

	b.mu.Lock()
	now := b.now()
	if c, ok := b.bound[key]; ok && now.Before(c.exp) {
		b.mu.Unlock()
		return c.token, nil
	}
	skip := now.Before(b.skipUntil)
	b.mu.Unlock()
	if skip {
		return tok, nil
	}

	bound, status, err := b.exchange(ctx, tok)
	switch {
	case err != nil:
		b.mu.Lock()
		b.skipUntil = now.Add(runcapBindBackoff)
		b.mu.Unlock()
		b.logf("runcap: binding the run capability failed, sending it unbound for %s: %v", runcapBindBackoff, err)
		return tok, nil
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		b.mu.Lock()
		b.skipUntil = now.Add(runcapBindRetryAfter)
		b.mu.Unlock()
		b.logf("runcap: the BFF offers no bind exchange; run capabilities stay bearer tokens")
		return tok, nil
	case status == http.StatusConflict:
		return "", errRuncapClaimed
	case status != http.StatusOK:
		b.logf("runcap: the BFF refused to bind the run capability (HTTP %d), sending it unbound", status)
		return tok, nil
	}

	bu, err := runcap.InspectUnverified(bound)
	if err != nil || bu.KeyThumbprint != b.signer.Thumbprint() {
		return tok, fmt.Errorf("runcap: the BFF returned a capability not bound to this launcher's key")
	}
	b.mu.Lock()
	for k, c := range b.bound {
		if !now.Before(c.exp) {
			delete(b.bound, k)
		}
	}
	b.bound[key] = boundCapability{token: bound, exp: bu.ExpiresAt}
	b.mu.Unlock()
	return bound, nil
}

func (b *runcapBinder) exchange(ctx context.Context, tok string) (string, int, error) {
	body, err := json.Marshal(map[string]string{"jkt": b.signer.Thumbprint()})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.bindURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, tok)
	resp, err := b.hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", resp.StatusCode, nil
	}
	var out struct {
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decoding the bind response: %w", err)
	}
	return out.Capability, http.StatusOK, nil
}

// runcapProofTransport sends each BFF-bound request's run capability in its bound form, with a proof of
// possession for that exact request. Requests to any other host pass through untouched: a bound
// capability and its proofs go only where they are verified.
type runcapProofTransport struct {
	base   http.RoundTripper
	binder *runcapBinder
}

// withRuncapProof wraps c's transport. A nil binder (no BFF configured) leaves c as it is.
func withRuncapProof(c *http.Client, b *runcapBinder) *http.Client {
	if b == nil {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = runcapProofTransport{base: base, binder: b}
	return c
}

func (t runcapProofTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := strings.TrimSpace(req.Header.Get(runcap.HeaderName))
	if tok == "" || req.URL.Host != t.binder.bffHost {
		return t.base.RoundTrip(req)
	}
	bound, err := t.binder.Bind(req.Context(), tok)
	if err != nil {
		return nil, err
	}
	u, err := runcap.InspectUnverified(bound)
	if err != nil || u.KeyThumbprint != t.binder.signer.Thumbprint() {
		// Bearer (no exchange offered) or bound to another holder: send as given, and let the BFF decide.
		return t.base.RoundTrip(req)
	}
	proof, err := t.binder.signer.Proof(req.Method, req.URL.String())
	if err != nil {
		return nil, err
	}
	// A RoundTripper must not modify the caller's request.
	out := req.Clone(req.Context())
	out.Header.Set(runcap.HeaderName, bound)
	out.Header.Set(runcap.PoPHeaderName, proof)
	return t.base.RoundTrip(out)
}

var (
	processBinderOnce sync.Once
	processBinder     *runcapBinder
)

// processRuncapBinder is this process's binder: one sender key per launcher process (ADR 0124), shared by
// every client that calls the BFF and by the front door. nil when BFF_INTERNAL_URL is unset.
func processRuncapBinder() *runcapBinder {
	processBinderOnce.Do(func() {
		logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
		b, err := newRuncapBinder(os.Getenv("BFF_INTERNAL_URL"), logf)
		if err != nil {
			logf("runcap: run capabilities stay bearer tokens: %v", err)
			return
		}
		processBinder = b
	})
	return processBinder
}

// bindInboundCapability replaces an inbound bearer capability with this process's bound one, before the
// request reaches the agent's code. It returns false, having written the response, only when another
// holder bound the capability first: it has leaked, and this agent can no longer spend it.
func bindInboundCapability(ctx context.Context, w http.ResponseWriter, req *http.Request, b *runcapBinder) bool {
	tok := strings.TrimSpace(req.Header.Get(runcap.HeaderName))
	if b == nil || tok == "" {
		return true
	}
	bctx, cancel := context.WithTimeout(ctx, runcapFrontDoorTimeout)
	defer cancel()
	bound, err := b.Bind(bctx, tok)
	switch {
	case errors.Is(err, errRuncapClaimed):
		http.Error(w, "the run capability was bound by another holder before this agent received it", http.StatusUnauthorized)
		return false
	case err != nil:
		b.logf("runcap: %v; passing the run capability through as received", err)
		return true
	}
	req.Header.Set(runcap.HeaderName, bound)
	return true
}
