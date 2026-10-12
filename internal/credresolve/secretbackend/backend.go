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

// Package secretbackend stores and resolves OBO grants as Kubernetes Secrets in the locked
// credential namespace (the grant wire format is credresolve's). The token service and the BFF
// use it; the per-pod egress sidecar must not, which is why it is not part of credresolve.
package secretbackend

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// maxRefreshAttempts bounds the optimistic-concurrency retry loop: on a resourceVersion
// conflict we re-read and re-evaluate (the winner may have already refreshed → we adopt
// their token), but we never loop unbounded on a pathologically contended Secret.
const maxRefreshAttempts = 4

// Backend is the self-hosted CredentialResolver: it resolves the invoking user's OBO
// credential from a grant Secret in the (locked) credential namespace, refreshing a
// near-expiry OAuth token server-side. It is the baseline everyone — including air-gapped
// installs — gets; external-vault backends implement the same interface for managed
// installs (ADR 0030 §4).
//
// Concurrency: a near-expiry refresh is singleflighted PER GRANT (in-process herd → one
// refresh) and written back with OPTIMISTIC CONCURRENCY (resourceVersion) so a stale
// token is never persisted (ADR 0029 §6 R3). A short-TTL cache serves repeat calls
// without touching the Secret (the sidecar latency win).
type Backend struct {
	cfg   Config
	cache *tokenCache
	group singleflight.Group
}

// Config configures a Backend.
type Config struct {
	// Client reads + writes grant Secrets. At the egress hop it carries the credential-
	// component identity scoped to the locked namespace (ADR 0029 §7).
	Client client.Client
	// CredentialNamespace consolidates grant reads into the locked platform namespace
	// (must mirror the write side). "" ⇒ legacy per-source-namespace reads.
	CredentialNamespace string
	// Exchanger performs the OAuth refresh + revoke network calls. Required for refresh;
	// a nil Exchanger serves only non-expiring / still-valid tokens and skips AS revoke.
	Exchanger credresolve.TokenExchanger
	// Now is the clock (default time.Now), overridable in tests.
	Now func() time.Time
	// AuthTypeIsOAuth reports whether a server needs per-user OAuth, so an absent grant is
	// ErrConsentRequired (OAuth) rather than ErrNoCredential (open). It is a seam so this
	// package stays free of the ToolRegistry CRD: the caller (sidecar / central service)
	// supplies the lookup. A nil seam, or a lookup error, is treated conservatively as
	// "not OAuth" so a transient failure never masquerades as a consent prompt.
	AuthTypeIsOAuth func(ctx context.Context, ns, server string) (bool, error)
	// OrgCredential resolves the ADMIN-SET SHARED org credential for a server (ADR 0029 §2):
	// consulted when the invoking user has NO personal grant. It returns the shared bearer
	// when the server is org-scoped and an org credential is set, else ErrNoCredential (so
	// resolution falls through to personal-consent / public). nil ⇒ never resolve org.
	// Personal-before-org holds: this is only reached when no personal grant was found.
	OrgCredential func(ctx context.Context, ns, server string) (credresolve.Credential, error)
	// Audit records credential-plane actions (never a token). Nil ⇒ no-op.
	Audit func(credresolve.AuditEvent)
}

// New builds a Backend with a fresh cache + a default clock.
func New(cfg Config) *Backend {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Backend{cfg: cfg, cache: newTokenCache()}
}

// Resolve implements CredentialResolver: cache fast-path → find the user's grant →
// refresh (singleflight + optimistic writeback) → cache + audit. Per-user isolation is
// structural — the grant is selected by BOTH the server AND the invoking user's hash, so
// user A can never resolve user B's grant.
func (b *Backend) Resolve(ctx context.Context, ns, boundary, server, userHash string) (credresolve.Credential, error) {
	boundaryHash := credresolve.BoundaryHash(boundary)
	readNS, name := credresolve.SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash, boundaryHash)
	cacheKey := readNS + "/" + name
	now := b.cfg.Now()

	// Fast path: a cached, still-valid token skips the Secret read + refresh entirely.
	if access, ok := b.cache.get(cacheKey, now); ok {
		b.audit(credresolve.ActionUse, server, userHash, ns, credresolve.ClassPersonalGrant)
		return credresolve.Credential{Kind: credresolve.KindBearer, Value: access}, nil
	}

	secret, found, err := b.findGrant(ctx, readNS, name, ns, server, userHash, boundaryHash)
	if err != nil {
		return credresolve.Credential{}, err
	}
	if !found && boundaryHash != "" {
		// Migration bridge (ADR 0033, m30.6): no boundary-scoped grant yet, but the user may
		// hold a pre-ADR-0033 unscoped grant. Fall back to it so connected accounts keep working
		// during the cutover; a later boundary-scoped write (re-consent for this agent's
		// registry) supersedes it. This is what makes it safe to introduce the boundary on the
		// read side before every write is boundary-scoped.
		legacyNS, legacyName := credresolve.SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash, "")
		ls, lok, lerr := b.findGrant(ctx, legacyNS, legacyName, ns, server, userHash, "")
		if lerr != nil {
			return credresolve.Credential{}, lerr
		}
		if lok {
			secret, found, cacheKey = ls, true, legacyNS+"/"+legacyName
		}
	}
	if !found {
		// No personal grant. Try the admin-set shared ORG credential (ADR 0029 §2) —
		// personal-before-org holds because we only reach here when the invoker has none.
		if b.cfg.OrgCredential != nil {
			cred, oErr := b.cfg.OrgCredential(ctx, ns, server)
			switch {
			case oErr == nil:
				b.audit(credresolve.ActionUse, server, userHash, ns, credresolve.ClassOrgCredential)
				return cred, nil
			case errors.Is(oErr, credresolve.ErrNoCredential):
				// Not org-scoped / no org credential set — fall through to consent/open.
			default:
				return credresolve.Credential{}, oErr
			}
		}
		// An OAuth (personal) server needs this user's consent; an open server needs none.
		if b.authTypeIsOAuth(ctx, ns, server) {
			return credresolve.Credential{}, credresolve.ErrConsentRequired
		}
		return credresolve.Credential{}, credresolve.ErrNoCredential
	}

	// The user has a grant. Refresh (rotate if near expiry) server-side against the
	// Secret's ACTUAL namespace so the rotated-token writeback lands on the same object.
	access, expiresAt, rErr := b.refresh(ctx, secret.Namespace, secret.Name)
	if rErr != nil {
		// A DEAD grant → the user must re-consent (surface a Connect CTA), NOT a hard
		// resolve_failed. Two cases mean the stored grant can no longer produce a token and
		// only re-authorization fixes it:
		//   - ErrNoRefreshToken: at/near expiry with nothing to rotate with; and
		//   - invalid_grant: the token endpoint REJECTED the refresh because the stored
		//     refresh token is expired/revoked (RFC 6749 §5.2).
		// Both are indistinguishable from "no grant" to the user, so they get the same
		// consent_required outcome — the run re-prompts to reconnect instead of erroring.
		if errors.Is(rErr, credresolve.ErrNoRefreshToken) || credresolve.IsInvalidGrant(rErr) {
			return credresolve.Credential{}, credresolve.ErrConsentRequired
		}
		return credresolve.Credential{}, rErr
	}

	b.cache.put(cacheKey, access, expiresAt, now)
	b.audit(credresolve.ActionUse, server, userHash, ns, credresolve.ClassPersonalGrant)
	return credresolve.Credential{Kind: credresolve.KindBearer, Value: access}, nil
}

// Revoke implements CredentialResolver: forget the grant (delete the Secret) and
// best-effort revoke it at the AS (RFC 7009). A missing grant is a no-op.
func (b *Backend) Revoke(ctx context.Context, ns, boundary, server, userHash string) error {
	boundaryHash := credresolve.BoundaryHash(boundary)
	readNS, name := credresolve.SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash, boundaryHash)
	cacheKey := readNS + "/" + name

	secret, found, err := b.findGrant(ctx, readNS, name, ns, server, userHash, boundaryHash)
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
		if revEnd := string(secret.Data[credresolve.KeyRevocationEndpoint]); revEnd != "" {
			token := string(secret.Data[credresolve.KeyRefreshToken])
			if token == "" {
				token = string(secret.Data[credresolve.KeyAccessToken])
			}
			_ = b.cfg.Exchanger.Revoke(ctx, revEnd, token)
		}
	}

	if err := b.cfg.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	b.cache.evict(cacheKey)
	b.audit(credresolve.ActionRevoke, server, userHash, ns, credresolve.ClassPersonalGrant)
	return nil
}

// findGrant returns the (user, server) grant Secret at its deterministic coordinates,
// verifying the labels match the requested (user-hash, server) — and, in locked mode, the
// source namespace — so a truncated-hash NAME collision can NEVER return another user's
// (or another namespace's) grant (the labels are the authoritative match). A missing
// Secret is (nil, false, nil).
func (b *Backend) findGrant(ctx context.Context, readNS, name, sourceNS, server, userHash, boundaryHash string) (*corev1.Secret, bool, error) {
	var secret corev1.Secret
	if err := b.cfg.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: readNS}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if secret.Labels[credresolve.LabelGrantUser] != userHash || secret.Labels[credresolve.LabelGrantServer] != server {
		return nil, false, nil
	}
	// The boundary is an authoritative match key too (ADR 0033): a boundary-scoped lookup
	// must never return a grant from a different boundary (or the legacy unscoped grant),
	// and an unscoped lookup (boundaryHash "") must not return a boundary-scoped grant.
	if secret.Labels[credresolve.LabelGrantBoundary] != boundaryHash {
		return nil, false, nil
	}
	// In locked mode many namespaces' grants coexist in one namespace, so the source
	// namespace is also an authoritative match key (a name hash-collision must never
	// cross the namespace boundary).
	if b.cfg.CredentialNamespace != "" && secret.Labels[credresolve.LabelGrantSourceNS] != sourceNS {
		return nil, false, nil
	}
	return &secret, true, nil
}

// refresh returns the fresh (or still-valid) access token + its expiry for the grant
// Secret at (ns, name), collapsing concurrent in-process callers for the SAME grant into
// ONE refresh via singleflight (the herd guard). Cross-PROCESS deduplication is the
// central token service's job (ADR 0030 increment 5); here a cross-process write conflict
// is handled by the optimistic-concurrency retry in refreshOnce.
func (b *Backend) refresh(ctx context.Context, ns, name string) (string, time.Time, error) {
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

// Compile-time assertion: the kubernetes backend is a config-selected grant writer.
var _ credresolve.GrantWriter = (*Backend)(nil)

// MigratableGrant is a legacy k8s-Secret grant reconstructed for migration into another
// backend — the (identity) coordinates plus the full token material.
type MigratableGrant struct {
	Namespace string
	// Boundary is the trust boundary the grant is scoped to (ADR 0033); "" for a legacy
	// unscoped grant. Preserved across a backfill so a migrated grant keeps its scope.
	Boundary string
	Server   string
	UserHash string
	Grant    credresolve.Grant
}

// ListGrants enumerates the per-user grant Secrets in the locked credential namespace and
// reconstructs each as a MigratableGrant — the READ side of a backfill into another backend
// (m28.2). It requires a locked credential namespace (the normal OBO deployment).
func (b *Backend) ListGrants(ctx context.Context) ([]MigratableGrant, error) {
	if b.cfg.CredentialNamespace == "" {
		return nil, errors.New("credresolve: ListGrants requires a locked credential namespace")
	}
	var secs corev1.SecretList
	if err := b.cfg.Client.List(ctx, &secs, client.InNamespace(b.cfg.CredentialNamespace)); err != nil {
		return nil, fmt.Errorf("credresolve: list grant Secrets: %w", err)
	}
	out := make([]MigratableGrant, 0, len(secs.Items))
	for i := range secs.Items {
		s := &secs.Items[i]
		userHash, server := s.Labels[credresolve.LabelGrantUser], s.Labels[credresolve.LabelGrantServer]
		if userHash == "" || server == "" {
			continue // not a grant Secret
		}
		ns := s.Labels[credresolve.LabelGrantSourceNS]
		if ns == "" {
			ns = s.Namespace
		}
		out = append(out, MigratableGrant{
			Namespace: ns, Boundary: s.Annotations[credresolve.AnnGrantBoundary], Server: server, UserHash: userHash,
			Grant: credresolve.Grant{
				Tokens: credresolve.Tokens{
					AccessToken:  string(s.Data[credresolve.KeyAccessToken]),
					RefreshToken: string(s.Data[credresolve.KeyRefreshToken]),
					ExpiresAt:    credresolve.ParseExpiry(string(s.Data[credresolve.KeyExpiry])),
				},
				Config: credresolve.OAuthConfig{
					TokenEndpoint:      string(s.Data[credresolve.KeyTokenEndpoint]),
					ClientID:           string(s.Data[credresolve.KeyClientID]),
					RevocationEndpoint: string(s.Data[credresolve.KeyRevocationEndpoint]),
				},
				ServerURL: s.Annotations[credresolve.AnnGrantServerURL],
			},
		})
	}
	return out, nil
}

// StoreGrant upserts the user's grant as a Secret — the kubernetes backend's write side of
// the SPI (ADR 0032). A re-consent for the same (user, server) REPLACES the stored grant
// (rotated tokens) rather than failing. This is the same shape the BFF OAuth callback writes,
// so grants are interchangeable across the two write paths.
func (b *Backend) StoreGrant(ctx context.Context, ns, boundary, server, userHash string, g credresolve.Grant) error {
	sourceNsLabel := ""
	if b.cfg.CredentialNamespace != "" {
		sourceNsLabel = ns
	}
	boundaryHash := credresolve.BoundaryHash(boundary)
	grantNS, grantName := credresolve.SecretCoordinates(b.cfg.CredentialNamespace, ns, server, userHash, boundaryHash)
	data := credresolve.SecretData(g.Config, g.Tokens)
	labels := credresolve.SecretLabels(server, userHash, sourceNsLabel, boundaryHash)
	ann := map[string]string{}
	if g.ServerURL != "" {
		ann[credresolve.AnnGrantServerURL] = g.ServerURL
	}
	if boundary != "" {
		ann[credresolve.AnnGrantBoundary] = boundary
	}

	var sec corev1.Secret
	err := b.cfg.Client.Get(ctx, client.ObjectKey{Namespace: grantNS, Name: grantName}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: grantName, Namespace: grantNS, Labels: labels, Annotations: ann},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		return b.cfg.Client.Create(ctx, &sec)
	case err != nil:
		return err
	default:
		sec.Data = data
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		maps.Copy(sec.Labels, labels)
		if len(ann) > 0 {
			if sec.Annotations == nil {
				sec.Annotations = map[string]string{}
			}
			maps.Copy(sec.Annotations, ann)
		}
		return b.cfg.Client.Update(ctx, &sec)
	}
}

// refreshOnce reads the grant Secret and, if the access token is at/near expiry, rotates
// it at the token endpoint and writes the rotated tokens back to the SAME Secret with
// optimistic concurrency. On a resourceVersion conflict (another writer — typically
// another pod/replica) it re-reads and re-evaluates: if the winner already refreshed, the
// token is no longer near expiry and we adopt it WITHOUT a second authorization-server
// call (avoiding a rotating-refresh-token double-use). It returns ErrNoRefreshToken when a
// near-expiry grant has no refresh token to rotate.
func (b *Backend) refreshOnce(ctx context.Context, ns, name string) (string, time.Time, error) {
	for range maxRefreshAttempts {
		var secret corev1.Secret
		if err := b.cfg.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &secret); err != nil {
			return "", time.Time{}, err
		}

		expiry := string(secret.Data[credresolve.KeyExpiry])
		now := b.cfg.Now()
		if !credresolve.NeedsRefresh(expiry, now) {
			// Still valid — attach the current token, no network call.
			return string(secret.Data[credresolve.KeyAccessToken]), credresolve.ParseExpiry(expiry), nil
		}

		refreshToken := string(secret.Data[credresolve.KeyRefreshToken])
		if refreshToken == "" {
			// Near expiry and nothing to rotate with → the caller must re-consent.
			return string(secret.Data[credresolve.KeyAccessToken]), credresolve.ParseExpiry(expiry), credresolve.ErrNoRefreshToken
		}
		if b.cfg.Exchanger == nil {
			return "", time.Time{}, fmt.Errorf("credresolve: grant needs refresh but no TokenExchanger is configured")
		}

		tokenEndpoint := string(secret.Data[credresolve.KeyTokenEndpoint])
		clientID := string(secret.Data[credresolve.KeyClientID])
		toks, err := b.cfg.Exchanger.Refresh(ctx, tokenEndpoint, clientID, refreshToken)
		if err != nil {
			return "", time.Time{}, err
		}
		// A token endpoint that did not rotate the refresh token → keep the existing one
		// (RFC 6749 §6). Preserve the revocation endpoint across the rewrite.
		if toks.RefreshToken == "" {
			toks.RefreshToken = refreshToken
		}
		cfg := credresolve.OAuthConfig{
			TokenEndpoint:      tokenEndpoint,
			ClientID:           clientID,
			RevocationEndpoint: string(secret.Data[credresolve.KeyRevocationEndpoint]),
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		maps.Copy(secret.Data, credresolve.SecretData(cfg, toks))

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
func (b *Backend) authTypeIsOAuth(ctx context.Context, ns, server string) bool {
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
func (b *Backend) audit(action credresolve.AuditAction, server, userHash, ns string, class credresolve.CredentialClass) {
	if b.cfg.Audit == nil {
		return
	}
	b.cfg.Audit(credresolve.AuditEvent{
		Action:    action,
		Server:    server,
		UserHash:  userHash,
		Namespace: ns,
		Class:     class,
	})
}

// Compile-time assertion that Backend satisfies the CredentialResolver seam.
var _ credresolve.CredentialResolver = (*Backend)(nil)
