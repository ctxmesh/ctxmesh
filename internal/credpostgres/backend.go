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

package credpostgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ctxmesh/agentry/internal/credresolve"
)

// refreshSkew refreshes a token slightly before expiry, so a resolve never hands back a
// token about to lapse mid tool call.
const refreshSkew = 60 * time.Second

// maxSaveAttempts bounds the optimistic-concurrency retry loop (a herd of writers).
const maxSaveAttempts = 4

// sealedGrant is the plaintext form of the sealed blob: the secret tokens PLUS the
// non-secret refresh config (endpoint/clientID/revocation) a rotation needs. Sealed as one
// unit so a DB dump reveals none of it.
type sealedGrant struct {
	AccessToken        string `json:"a"`
	RefreshToken       string `json:"r,omitempty"`
	TokenEndpoint      string `json:"te,omitempty"`
	ClientID           string `json:"ci,omitempty"`
	RevocationEndpoint string `json:"re,omitempty"`
}

// Grant is the caller-facing token material for Store (from the OAuth callback).
type Grant struct {
	AccessToken        string
	RefreshToken       string
	ExpiresAt          time.Time
	TokenEndpoint      string
	ClientID           string
	RevocationEndpoint string
	Scope              string
	TokenType          string
}

// BackendConfig configures a Postgres credential backend.
type BackendConfig struct {
	Storage Storage
	Sealer  Sealer
	// TenantForNS maps a source namespace to the tenant key that selects the per-tenant
	// KEK. nil ⇒ the namespace itself is the tenant.
	TenantForNS func(ns string) string
	// Exchanger performs OAuth refresh/revoke. nil ⇒ serve only still-valid tokens.
	Exchanger credresolve.TokenExchanger
	// AuthTypeIsOAuth reports whether an absent grant is consent-required (OAuth) vs open.
	AuthTypeIsOAuth func(ctx context.Context, ns, server string) (bool, error)
	// OrgCredential resolves the admin-set shared credential when the invoker has no
	// personal grant (ADR 0029 §2). nil ⇒ never resolve org.
	OrgCredential func(ctx context.Context, ns, server string) (credresolve.Credential, error)
	// Audit records credential-plane actions (never a token). nil ⇒ no-op.
	Audit func(credresolve.AuditEvent)
	// Now is the clock (default time.Now).
	Now func() time.Time
	// CacheTTL bounds the resolved-token cache (default 30s). Zero disables the cache.
	CacheTTL time.Duration
}

// Backend is the Postgres reference credential backend: per-user grants as envelope-
// encrypted rows, refreshed with optimistic concurrency + per-grant singleflight, so grant
// reads + OAuth refresh amortize once even under a busy multi-user fleet (ADR 0030 §1).
type Backend struct {
	cfg   BackendConfig
	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	cred    credresolve.Credential
	expires time.Time
}

// NewBackend builds a Postgres backend. Storage and Sealer are required.
func NewBackend(cfg BackendConfig) (*Backend, error) {
	if cfg.Storage == nil || cfg.Sealer == nil {
		return nil, errors.New("credpostgres: Storage and Sealer are required (a Postgres backend must encrypt)")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	return &Backend{cfg: cfg, cache: map[string]cacheEntry{}}, nil
}

func (b *Backend) tenant(ns string) string {
	if b.cfg.TenantForNS != nil {
		return b.cfg.TenantForNS(ns)
	}
	return ns
}

func rowKey(ns, boundary, server, userHash string) string {
	return ns + "|" + boundary + "|" + server + "|" + userHash
}

// Resolve returns the invoking user's fresh access token, refreshing near-expiry grants.
// Precedence (ADR 0029 §2): personal grant → org credential → consent-required / none.
// boundary scopes the personal grant to the invoking agent's registry (ADR 0033).
func (b *Backend) Resolve(ctx context.Context, ns, boundary, server, userHash string) (credresolve.Credential, error) {
	if c, ok := b.cacheGet(rowKey(ns, boundary, server, userHash)); ok {
		return c, nil
	}

	st, found, err := b.cfg.Storage.load(ctx, ns, boundary, server, userHash)
	if err != nil {
		return credresolve.Credential{}, err
	}
	if !found && boundary != "" {
		// Migration bridge (ADR 0033, m30.6): fall back to the user's legacy unscoped grant so
		// connected accounts keep working during the boundary cutover. Resolve it AS the legacy
		// grant (boundary "") so refresh + cache key match where it actually lives.
		lst, lok, lerr := b.cfg.Storage.load(ctx, ns, "", server, userHash)
		if lerr != nil {
			return credresolve.Credential{}, lerr
		}
		if lok {
			st, found, boundary = lst, true, ""
		}
	}
	if found {
		cred, err := b.resolveGrant(ctx, ns, boundary, server, userHash, st)
		if err != nil {
			return credresolve.Credential{}, err
		}
		b.audit(credresolve.ActionUse, ns, server, userHash, credresolve.ClassPersonalGrant)
		b.cacheSet(rowKey(ns, boundary, server, userHash), cred)
		return cred, nil
	}

	// No personal grant → org credential (only for an OAuth server), else consent/none.
	oauth := false
	if b.cfg.AuthTypeIsOAuth != nil {
		if v, aErr := b.cfg.AuthTypeIsOAuth(ctx, ns, server); aErr == nil {
			oauth = v
		}
	}
	if oauth && b.cfg.OrgCredential != nil {
		cred, oErr := b.cfg.OrgCredential(ctx, ns, server)
		switch {
		case oErr == nil:
			b.audit(credresolve.ActionUse, ns, server, userHash, credresolve.ClassOrgCredential)
			return cred, nil
		case !errors.Is(oErr, credresolve.ErrNoCredential):
			return credresolve.Credential{}, oErr
		}
	}
	if oauth {
		return credresolve.Credential{}, credresolve.ErrConsentRequired
	}
	return credresolve.Credential{}, credresolve.ErrNoCredential
}

// resolveGrant decrypts a stored grant and refreshes it if near expiry.
func (b *Backend) resolveGrant(ctx context.Context, ns, boundary, server, userHash string, st stored) (credresolve.Credential, error) {
	g, err := b.unseal(ctx, ns, st)
	if err != nil {
		return credresolve.Credential{}, err
	}
	if !b.needsRefresh(st.expiresAt) {
		return credresolve.Credential{Kind: credresolve.KindBearer, Value: g.AccessToken}, nil
	}
	if g.RefreshToken == "" || b.cfg.Exchanger == nil {
		// Near expiry, nothing to rotate with → hand back what we have (the sidecar may
		// still succeed; a subsequent 401 surfaces as consent).
		return credresolve.Credential{Kind: credresolve.KindBearer, Value: g.AccessToken}, nil
	}
	token, sfErr, _ := b.group.Do(rowKey(ns, boundary, server, userHash), func() (any, error) {
		return b.refresh(ctx, ns, boundary, server, userHash)
	})
	if sfErr != nil {
		return credresolve.Credential{}, sfErr
	}
	return credresolve.Credential{Kind: credresolve.KindBearer, Value: token.(string)}, nil
}

// refresh rotates a near-expiry grant at the token endpoint and writes it back with
// optimistic concurrency; on conflict it re-reads and adopts the winner (no double refresh
// of a rotating refresh token).
func (b *Backend) refresh(ctx context.Context, ns, boundary, server, userHash string) (string, error) {
	for range maxSaveAttempts {
		st, found, err := b.cfg.Storage.load(ctx, ns, boundary, server, userHash)
		if err != nil {
			return "", err
		}
		if !found {
			return "", credresolve.ErrConsentRequired
		}
		g, err := b.unseal(ctx, ns, st)
		if err != nil {
			return "", err
		}
		if !b.needsRefresh(st.expiresAt) {
			return g.AccessToken, nil // a concurrent writer already refreshed — adopt it
		}
		if g.RefreshToken == "" {
			return g.AccessToken, nil
		}
		toks, err := b.cfg.Exchanger.Refresh(ctx, g.TokenEndpoint, g.ClientID, g.RefreshToken)
		if err != nil {
			return "", err
		}
		if toks.RefreshToken == "" {
			toks.RefreshToken = g.RefreshToken // endpoint did not rotate → keep (RFC 6749 §6)
		}
		g.AccessToken, g.RefreshToken = toks.AccessToken, toks.RefreshToken
		newSt, err := b.seal(ctx, ns, g, toks.ExpiresAt, st)
		if err != nil {
			return "", err
		}
		switch err := b.cfg.Storage.save(ctx, ns, boundary, server, userHash, newSt, st.version); {
		case err == nil:
			b.cacheDel(rowKey(ns, boundary, server, userHash))
			return toks.AccessToken, nil
		case errors.Is(err, errConflict):
			continue // another writer won; re-read + re-evaluate
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("credpostgres: refresh writeback exhausted %d optimistic retries", maxSaveAttempts)
}

// StoreGrant adapts the SPI's common write payload (credresolve.Grant) to this backend's
// Store, so the Postgres backend is a config-selected GrantWriter (ADR 0032).
func (b *Backend) StoreGrant(ctx context.Context, ns, boundary, server, userHash string, g credresolve.Grant) error {
	return b.Store(ctx, ns, boundary, server, userHash, Grant{
		AccessToken:        g.Tokens.AccessToken,
		RefreshToken:       g.Tokens.RefreshToken,
		ExpiresAt:          g.Tokens.ExpiresAt,
		TokenEndpoint:      g.Config.TokenEndpoint,
		ClientID:           g.Config.ClientID,
		RevocationEndpoint: g.Config.RevocationEndpoint,
	})
}

// Store persists (or overwrites) a user's grant — the write path from the OAuth callback.
func (b *Backend) Store(ctx context.Context, ns, boundary, server, userHash string, g Grant) error {
	sg := sealedGrant{
		AccessToken: g.AccessToken, RefreshToken: g.RefreshToken,
		TokenEndpoint: g.TokenEndpoint, ClientID: g.ClientID, RevocationEndpoint: g.RevocationEndpoint,
	}
	for range maxSaveAttempts {
		_, _, expectedVersion, err := b.loadVersion(ctx, ns, boundary, server, userHash)
		if err != nil {
			return err
		}
		st, err := b.sealGrant(ctx, ns, sg, g.ExpiresAt, g.Scope, g.TokenType)
		if err != nil {
			return err
		}
		switch err := b.cfg.Storage.save(ctx, ns, boundary, server, userHash, st, expectedVersion); {
		case err == nil:
			b.cacheDel(rowKey(ns, boundary, server, userHash))
			return nil
		case errors.Is(err, errConflict):
			continue
		default:
			return err
		}
	}
	return fmt.Errorf("credpostgres: store exhausted %d optimistic retries", maxSaveAttempts)
}

// Revoke best-effort revokes at the AS then deletes the grant (RFC 7009: forget locally +
// best-effort at the AS).
func (b *Backend) Revoke(ctx context.Context, ns, boundary, server, userHash string) error {
	st, found, err := b.cfg.Storage.load(ctx, ns, boundary, server, userHash)
	if err != nil {
		return err
	}
	if !found {
		return nil // already gone
	}
	if b.cfg.Exchanger != nil {
		if g, uErr := b.unseal(ctx, ns, st); uErr == nil && g.RevocationEndpoint != "" {
			tok := g.RefreshToken
			if tok == "" {
				tok = g.AccessToken
			}
			_ = b.cfg.Exchanger.Revoke(ctx, g.RevocationEndpoint, tok) // advisory
		}
	}
	b.cacheDel(rowKey(ns, boundary, server, userHash))
	if err := b.cfg.Storage.del(ctx, ns, boundary, server, userHash); err != nil {
		return err
	}
	b.audit(credresolve.ActionRevoke, ns, server, userHash, credresolve.ClassPersonalGrant)
	return nil
}

// SweepExpired deletes grants past expiry — the TTL sweeper (run on a schedule).
func (b *Backend) SweepExpired(ctx context.Context) (int64, error) {
	return b.cfg.Storage.sweepExpired(ctx, b.cfg.Now())
}

func (b *Backend) loadVersion(ctx context.Context, ns, boundary, server, userHash string) (stored, bool, int64, error) {
	st, found, err := b.cfg.Storage.load(ctx, ns, boundary, server, userHash)
	if err != nil {
		return stored{}, false, 0, err
	}
	if !found {
		return stored{}, false, 0, nil
	}
	return st, true, st.version, nil
}

func (b *Backend) unseal(ctx context.Context, ns string, st stored) (sealedGrant, error) {
	blob, err := b.cfg.Sealer.Unseal(ctx, Sealed{KeyID: st.keyID, WrappedDEK: st.wrappedDEK, Nonce: st.nonce, Ciphertext: st.ciphertext}, b.tenant(ns))
	if err != nil {
		return sealedGrant{}, err
	}
	var g sealedGrant
	if err := json.Unmarshal(blob, &g); err != nil {
		return sealedGrant{}, fmt.Errorf("credpostgres: decode sealed grant: %w", err)
	}
	return g, nil
}

// seal re-seals a mutated grant preserving the row's scope/tokenType/class.
func (b *Backend) seal(ctx context.Context, ns string, g sealedGrant, expiresAt time.Time, prev stored) (stored, error) {
	return b.sealGrant(ctx, ns, g, expiresAt, prev.scope, prev.tokenType)
}

func (b *Backend) sealGrant(ctx context.Context, ns string, g sealedGrant, expiresAt time.Time, scope, tokenType string) (stored, error) {
	blob, err := json.Marshal(g)
	if err != nil {
		return stored{}, fmt.Errorf("credpostgres: encode grant: %w", err)
	}
	sealed, err := b.cfg.Sealer.Seal(ctx, blob, b.tenant(ns))
	if err != nil {
		return stored{}, err
	}
	return stored{
		keyID: sealed.KeyID, wrappedDEK: sealed.WrappedDEK, nonce: sealed.Nonce, ciphertext: sealed.Ciphertext,
		expiresAt: expiresAt, tokenType: tokenType, scope: scope, class: string(credresolve.ClassPersonalGrant),
	}, nil
}

func (b *Backend) needsRefresh(expiresAt time.Time) bool {
	return !expiresAt.IsZero() && b.cfg.Now().Add(refreshSkew).After(expiresAt)
}

func (b *Backend) audit(action credresolve.AuditAction, ns, server, userHash string, class credresolve.CredentialClass) {
	if b.cfg.Audit != nil {
		b.cfg.Audit(credresolve.AuditEvent{Action: action, Server: server, UserHash: userHash, Namespace: ns, Class: class})
	}
}

func (b *Backend) cacheGet(key string) (credresolve.Credential, bool) {
	if b.cfg.CacheTTL <= 0 {
		return credresolve.Credential{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.cache[key]
	if !ok || b.cfg.Now().After(e.expires) {
		return credresolve.Credential{}, false
	}
	return e.cred, true
}

func (b *Backend) cacheSet(key string, cred credresolve.Credential) {
	if b.cfg.CacheTTL <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache[key] = cacheEntry{cred: cred, expires: b.cfg.Now().Add(b.cfg.CacheTTL)}
}

func (b *Backend) cacheDel(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cache, key)
}

// Compile-time assertions: a drop-in resolver AND a config-selected grant writer.
var (
	_ credresolve.CredentialResolver = (*Backend)(nil)
	_ credresolve.GrantWriter        = (*Backend)(nil)
)
