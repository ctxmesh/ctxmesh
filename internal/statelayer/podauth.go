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

package statelayer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrTokenRejected is returned when a pod token is definitively invalid (bad
// signature, wrong audience, expired, or the SA no longer exists) — the caller
// maps it to 401. Any OTHER error from PodAuthenticator is an auth-INFRA failure
// (the TokenReview call itself failed) and maps to 503: the distinction lets the
// launcher fail budget CLOSED while failing rate/concurrency OPEN (ADR 0050 Amд 3).
var ErrTokenRejected = errors.New("statelayer: pod token rejected")

// saUsernamePrefix is the username the API server assigns a ServiceAccount token:
// system:serviceaccount:<namespace>:<name>. The namespace is authoritative — it
// comes from the verified token, never from caller input.
const saUsernamePrefix = "system:serviceaccount:"

// podTokenSkew backs off the cache expiry from the token's own exp so a review is
// refreshed slightly before the token actually expires.
const podTokenSkew = 30 * time.Second

// podTokenFallbackTTL caches a verified token this long when its exp can't be
// parsed — conservative, so a malformed-but-accepted token isn't trusted forever.
const podTokenFallbackTTL = 60 * time.Second

// PodAuthenticator authenticates a launcher's pod-identity token and returns the
// pod's namespace. It is the second principal type on the proxy (alongside the
// memory runcap) for the quota/async paths, which carry no per-user capability.
type PodAuthenticator interface {
	// Namespace verifies the bearer token and returns the pod's namespace. It
	// returns ErrTokenRejected for an invalid token (→ 401) and any other error for
	// an auth-infra failure (→ 503).
	Namespace(ctx context.Context, token string) (string, error)
}

// tokenReviewer performs a Kubernetes TokenReview. An interface so tests can inject
// a fake without a live API server.
type tokenReviewer interface {
	// review returns whether the token authenticated and, if so, its username.
	// A non-nil error is an infra failure (the review could not be completed);
	// authenticated=false is a clean rejection.
	review(ctx context.Context, token string, audiences []string) (authenticated bool, username string, err error)
}

// clientTokenReviewer runs TokenReview through client-go (the cluster
// authenticator — authoritative, honours SA deletion/rotation).
type clientTokenReviewer struct {
	client kubernetes.Interface
}

func (c clientTokenReviewer) review(
	ctx context.Context, token string, audiences []string,
) (bool, string, error) {
	if len(audiences) == 0 {
		return false, "", errors.New("tokenreview: no audience requested") // caller invariant
	}
	tr, err := c.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		// The review call itself did not complete (network / apiserver down / RBAC
		// denied) — the ONLY genuine infra signal → 503, so budget fails CLOSED and
		// rate/concurrency fail OPEN (ADR 0050 Amд 3).
		return false, "", err
	}
	if !tr.Status.Authenticated {
		// A clean rejection → 401. NOTE (verified against the real apiserver): the
		// apiserver puts the REASON — "invalid bearer token", expiry, or an audience
		// mismatch — in Status.Error on an authenticated=false response, so Status.Error
		// is NOT an infra signal and must not map to 503; that would misclassify every
		// rejection as infra. For our projected SA tokens the built-in authenticator
		// validates them (no external webhook), so authenticated=false is always a real
		// rejection.
		return false, "", nil
	}
	// Audience-awareness (K8s TokenReview contract): a client that sets spec.audiences
	// MUST confirm a compatible audience is echoed in status.audiences. An EMPTY
	// status.audiences on an authenticated response means the apiserver validated the
	// token against the KUBE-API audience, not ours — so any API-valid token (e.g. a
	// kubectl token) would pass. Refuse that (fail closed, → 503): we can't confirm the
	// token was minted for this proxy.
	if len(tr.Status.Audiences) == 0 {
		return false, "", errors.New("tokenreview: apiserver is not audience-aware " +
			"(empty status.audiences); refusing to accept an unscoped token")
	}
	// Authenticated + audience-aware, but our audience wasn't among those the token is
	// valid for → a clean rejection (defensive: the apiserver returns authenticated=false
	// for a mismatch, but never trust an unmatched audience).
	if !slices.Contains(tr.Status.Audiences, audiences[0]) {
		return false, "", nil
	}
	return true, tr.Status.User.Username, nil
}

type podCacheEntry struct {
	namespace string
	expiry    time.Time
}

// tokenReviewAuthenticator verifies pod tokens via a cached TokenReview (ADR 0050
// Amд 3). The launcher presents the same projected token until kubelet rotates it
// (~hourly), so the review result is cached on the raw token string with a TTL
// derived from the token's exp — steady state is ~one review per pod per rotation.
type tokenReviewAuthenticator struct {
	reviewer tokenReviewer
	audience string
	now      func() time.Time

	mu    sync.RWMutex
	cache map[string]podCacheEntry
}

// NewTokenReviewAuthenticator builds a PodAuthenticator over a Kubernetes client,
// requiring tokens minted for the given audience.
func NewTokenReviewAuthenticator(client kubernetes.Interface, audience string, now func() time.Time) PodAuthenticator {
	return newTokenReviewAuthenticator(clientTokenReviewer{client: client}, audience, now)
}

func newTokenReviewAuthenticator(r tokenReviewer, audience string, now func() time.Time) *tokenReviewAuthenticator {
	if now == nil {
		now = time.Now
	}
	return &tokenReviewAuthenticator{
		reviewer: r,
		audience: audience,
		now:      now,
		cache:    make(map[string]podCacheEntry),
	}
}

func (a *tokenReviewAuthenticator) Namespace(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrTokenRejected
	}
	if ns, ok := a.cachedNamespace(token); ok {
		return ns, nil
	}

	authenticated, username, err := a.reviewer.review(ctx, token, []string{a.audience})
	if err != nil {
		return "", fmt.Errorf("tokenreview: %w", err) // infra → 503
	}
	if !authenticated {
		return "", ErrTokenRejected // rejected → 401
	}
	ns, ok := namespaceFromSAUsername(username)
	if !ok {
		// Authenticated, but not a ServiceAccount (e.g. a user token) — not a valid
		// pod identity for quota. Reject rather than guess a namespace.
		return "", ErrTokenRejected
	}
	a.store(token, ns)
	return ns, nil
}

func (a *tokenReviewAuthenticator) cachedNamespace(token string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.cache[token]
	if !ok || !a.now().Before(e.expiry) {
		return "", false
	}
	return e.namespace, true
}

func (a *tokenReviewAuthenticator) store(token, namespace string) {
	now := a.now()
	expiry := now.Add(podTokenFallbackTTL)
	if exp, ok := tokenExpiry(token); ok {
		if backed := exp.Add(-podTokenSkew); backed.After(now) {
			expiry = backed
		} else {
			// Already within the skew of expiry — cache only briefly.
			expiry = now.Add(podTokenSkew)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Sweep expired entries so the cache stays bounded to live tokens.
	for k, e := range a.cache {
		if !now.Before(e.expiry) {
			delete(a.cache, k)
		}
	}
	a.cache[token] = podCacheEntry{namespace: namespace, expiry: expiry}
}

// namespaceFromSAUsername extracts <ns> from system:serviceaccount:<ns>:<sa>.
func namespaceFromSAUsername(username string) (string, bool) {
	rest, ok := strings.CutPrefix(username, saUsernamePrefix)
	if !ok {
		return "", false
	}
	ns, _, ok := strings.Cut(rest, ":")
	if !ok || ns == "" {
		return "", false
	}
	return ns, true
}

// tokenExpiry reads the exp claim from a JWT WITHOUT verifying the signature — the
// TokenReview has already attested the token; this only sizes the cache TTL, so a
// forged exp can at worst shorten/extend the cache within the token's real life
// (which the authenticator re-checks after expiry anyway).
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
