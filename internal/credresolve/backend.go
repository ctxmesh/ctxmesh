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

package credresolve

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxRefreshAttempts bounds the optimistic-concurrency retry loop: on a resourceVersion
// conflict we re-read and re-evaluate (the winner may have already refreshed → we adopt
// their token), but we never loop unbounded on a pathologically contended Secret.
const maxRefreshAttempts = 4

// K8sBackend is the self-hosted CredentialResolver: it resolves the invoking user's OBO
// credential from a grant Secret in the (locked) credential namespace, refreshing a
// near-expiry OAuth token server-side. It is the baseline everyone — including air-gapped
// installs — gets; external-vault backends implement the same interface for managed
// installs (ADR 0030 §4).
//
// Concurrency: a near-expiry refresh is singleflighted PER GRANT (in-process herd → one
// refresh) and written back with OPTIMISTIC CONCURRENCY (resourceVersion) so a stale
// token is never persisted (ADR 0029 §6 R3). A short-TTL cache serves repeat calls
// without touching the Secret (the sidecar latency win).
type K8sBackend struct {
	cfg   K8sBackendConfig
	cache *tokenCache
	group singleflight.Group
}

// K8sBackendConfig configures a K8sBackend.
type K8sBackendConfig struct {
	// Client reads + writes grant Secrets. At the egress hop it carries the credential-
	// component identity scoped to the locked namespace (ADR 0029 §7).
	Client client.Client
	// CredentialNamespace consolidates grant reads into the locked platform namespace
	// (must mirror the write side). "" ⇒ legacy per-source-namespace reads.
	CredentialNamespace string
	// Exchanger performs the OAuth refresh + revoke network calls. Required for refresh;
	// a nil Exchanger serves only non-expiring / still-valid tokens and skips AS revoke.
	Exchanger TokenExchanger
	// Now is the clock (default time.Now), overridable in tests.
	Now func() time.Time
	// AuthTypeIsOAuth reports whether a server needs per-user OAuth, so an absent grant is
	// ErrConsentRequired (OAuth) rather than ErrNoCredential (open). It is a seam so this
	// package stays free of the ToolRegistry CRD: the caller (sidecar / central service)
	// supplies the lookup. A nil seam, or a lookup error, is treated conservatively as
	// "not OAuth" so a transient failure never masquerades as a consent prompt.
	AuthTypeIsOAuth func(ctx context.Context, ns, server string) (bool, error)
	// Audit records credential-plane actions (never a token). Nil ⇒ no-op.
	Audit func(AuditEvent)
}

// NewK8sBackend builds a K8sBackend with a fresh cache + a default clock.
func NewK8sBackend(cfg K8sBackendConfig) *K8sBackend {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &K8sBackend{cfg: cfg, cache: newTokenCache()}
}

// Resolve implements CredentialResolver: cache fast-path → find the user's grant →
// refresh (singleflight + optimistic writeback) → cache + audit. Per-user isolation is
// structural — the grant is selected by BOTH the server AND the invoking user's hash, so
// user A can never resolve user B's grant.
func (b *K8sBackend) Resolve(ctx context.Context, ns, server, userHash string) (Credential, error) {
	readNS, name := SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash)
	cacheKey := readNS + "/" + name
	now := b.cfg.Now()

	// Fast path: a cached, still-valid token skips the Secret read + refresh entirely.
	if access, ok := b.cache.get(cacheKey, now); ok {
		b.audit(ActionUse, server, userHash, ns, ClassPersonalGrant)
		return Credential{Kind: KindBearer, Value: access}, nil
	}

	secret, found, err := b.findGrant(ctx, readNS, name, ns, server, userHash)
	if err != nil {
		return Credential{}, err
	}
	if !found {
		// No grant for this (user, server). An OAuth server needs this user's consent; an
		// open server needs no credential.
		if b.authTypeIsOAuth(ctx, ns, server) {
			return Credential{}, ErrConsentRequired
		}
		return Credential{}, ErrNoCredential
	}

	// The user has a grant. Refresh (rotate if near expiry) server-side against the
	// Secret's ACTUAL namespace so the rotated-token writeback lands on the same object.
	access, expiresAt, rErr := b.refresh(ctx, secret.Namespace, secret.Name)
	if rErr != nil {
		// A non-refreshable grant at/near expiry → the user must re-consent.
		if errors.Is(rErr, ErrNoRefreshToken) {
			return Credential{}, ErrConsentRequired
		}
		return Credential{}, rErr
	}

	b.cache.put(cacheKey, access, expiresAt, now)
	b.audit(ActionUse, server, userHash, ns, ClassPersonalGrant)
	return Credential{Kind: KindBearer, Value: access}, nil
}

// Revoke implements CredentialResolver: forget the grant (delete the Secret) and
// best-effort revoke it at the AS (RFC 7009). A missing grant is a no-op.
func (b *K8sBackend) Revoke(ctx context.Context, ns, server, userHash string) error {
	readNS, name := SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash)
	cacheKey := readNS + "/" + name

	secret, found, err := b.findGrant(ctx, readNS, name, ns, server, userHash)
	if err != nil {
		return err
	}
	if !found {
		b.cache.evict(cacheKey)
		return nil
	}

	// Best-effort revoke at the AS BEFORE we forget locally (RFC 7009). A failure here is
	// advisory only — the primary effect of revoke is that WE forget the grant.
	if b.cfg.Exchanger != nil {
		if revEnd := string(secret.Data[KeyRevocationEndpoint]); revEnd != "" {
			token := string(secret.Data[KeyRefreshToken])
			if token == "" {
				token = string(secret.Data[KeyAccessToken])
			}
			_ = b.cfg.Exchanger.Revoke(ctx, revEnd, token)
		}
	}

	if err := b.cfg.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	b.cache.evict(cacheKey)
	b.audit(ActionRevoke, server, userHash, ns, ClassPersonalGrant)
	return nil
}

// findGrant returns the (user, server) grant Secret at its deterministic coordinates,
// verifying the labels match the requested (user-hash, server) — and, in locked mode, the
// source namespace — so a truncated-hash NAME collision can NEVER return another user's
// (or another namespace's) grant (the labels are the authoritative match). A missing
// Secret is (nil, false, nil).
func (b *K8sBackend) findGrant(ctx context.Context, readNS, name, sourceNS, server, userHash string) (*corev1.Secret, bool, error) {
	var secret corev1.Secret
	if err := b.cfg.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: readNS}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if secret.Labels[LabelGrantUser] != userHash || secret.Labels[LabelGrantServer] != server {
		return nil, false, nil
	}
	// In locked mode many namespaces' grants coexist in one namespace, so the source
	// namespace is also an authoritative match key (a name hash-collision must never
	// cross the namespace boundary).
	if b.cfg.CredentialNamespace != "" && secret.Labels[LabelGrantSourceNS] != sourceNS {
		return nil, false, nil
	}
	return &secret, true, nil
}

// refresh returns the fresh (or still-valid) access token + its expiry for the grant
// Secret at (ns, name), collapsing concurrent in-process callers for the SAME grant into
// ONE refresh via singleflight (the herd guard). Cross-PROCESS deduplication is the
// central token service's job (ADR 0030 increment 5); here a cross-process write conflict
// is handled by the optimistic-concurrency retry in refreshOnce.
func (b *K8sBackend) refresh(ctx context.Context, ns, name string) (string, time.Time, error) {
	type result struct {
		access    string
		expiresAt time.Time
	}
	key := ns + "/" + name
	v, err, _ := b.group.Do(key, func() (any, error) {
		access, expiresAt, rErr := b.refreshOnce(ctx, ns, name)
		return result{access: access, expiresAt: expiresAt}, rErr
	})
	r, _ := v.(result)
	return r.access, r.expiresAt, err
}

// refreshOnce reads the grant Secret and, if the access token is at/near expiry, rotates
// it at the token endpoint and writes the rotated tokens back to the SAME Secret with
// optimistic concurrency. On a resourceVersion conflict (another writer — typically
// another pod/replica) it re-reads and re-evaluates: if the winner already refreshed, the
// token is no longer near expiry and we adopt it WITHOUT a second authorization-server
// call (avoiding a rotating-refresh-token double-use). It returns ErrNoRefreshToken when a
// near-expiry grant has no refresh token to rotate.
func (b *K8sBackend) refreshOnce(ctx context.Context, ns, name string) (string, time.Time, error) {
	for range maxRefreshAttempts {
		var secret corev1.Secret
		if err := b.cfg.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &secret); err != nil {
			return "", time.Time{}, err
		}

		expiry := string(secret.Data[KeyExpiry])
		now := b.cfg.Now()
		if !NeedsRefresh(expiry, now) {
			// Still valid — attach the current token, no network call.
			return string(secret.Data[KeyAccessToken]), ParseExpiry(expiry), nil
		}

		refreshToken := string(secret.Data[KeyRefreshToken])
		if refreshToken == "" {
			// Near expiry and nothing to rotate with → the caller must re-consent.
			return string(secret.Data[KeyAccessToken]), ParseExpiry(expiry), ErrNoRefreshToken
		}
		if b.cfg.Exchanger == nil {
			return "", time.Time{}, fmt.Errorf("credresolve: grant needs refresh but no TokenExchanger is configured")
		}

		tokenEndpoint := string(secret.Data[KeyTokenEndpoint])
		clientID := string(secret.Data[KeyClientID])
		toks, err := b.cfg.Exchanger.Refresh(ctx, tokenEndpoint, clientID, refreshToken)
		if err != nil {
			return "", time.Time{}, err
		}
		// A token endpoint that did not rotate the refresh token → keep the existing one
		// (RFC 6749 §6). Preserve the revocation endpoint across the rewrite.
		if toks.RefreshToken == "" {
			toks.RefreshToken = refreshToken
		}
		cfg := OAuthConfig{
			TokenEndpoint:      tokenEndpoint,
			ClientID:           clientID,
			RevocationEndpoint: string(secret.Data[KeyRevocationEndpoint]),
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		maps.Copy(secret.Data, SecretData(cfg, toks))

		if err := b.cfg.Client.Update(ctx, &secret); err != nil {
			if apierrors.IsConflict(err) {
				// Another writer won; re-read + re-evaluate (they may have refreshed).
				continue
			}
			return "", time.Time{}, err
		}
		return toks.AccessToken, toks.ExpiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("credresolve: refresh writeback exhausted %d optimistic-concurrency retries", maxRefreshAttempts)
}

// authTypeIsOAuth consults the AuthTypeIsOAuth seam conservatively: a nil seam or a
// lookup error is treated as "not OAuth" so a transient failure never turns an absent
// grant into a spurious consent prompt.
func (b *K8sBackend) authTypeIsOAuth(ctx context.Context, ns, server string) bool {
	if b.cfg.AuthTypeIsOAuth == nil {
		return false
	}
	isOAuth, err := b.cfg.AuthTypeIsOAuth(ctx, ns, server)
	if err != nil {
		return false
	}
	return isOAuth
}

// audit emits one credential-plane audit event when an auditor is configured (never a token).
func (b *K8sBackend) audit(action AuditAction, server, userHash, ns string, class CredentialClass) {
	if b.cfg.Audit == nil {
		return
	}
	b.cfg.Audit(AuditEvent{
		Action:    action,
		Server:    server,
		UserHash:  userHash,
		Namespace: ns,
		Class:     class,
	})
}

// Compile-time assertion that K8sBackend satisfies the CredentialResolver seam.
var _ CredentialResolver = (*K8sBackend)(nil)
