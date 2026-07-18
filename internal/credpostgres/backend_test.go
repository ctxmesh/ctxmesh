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
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// --- fakes ---

type fakeStorage struct {
	mu   sync.Mutex
	rows map[string]stored
}

func newFakeStorage() *fakeStorage { return &fakeStorage{rows: map[string]stored{}} }

func (f *fakeStorage) load(_ context.Context, ns, boundary, server, userHash string) (stored, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.rows[rowKey(ns, boundary, server, userHash)]
	return st, ok, nil
}

func (f *fakeStorage) save(_ context.Context, ns, boundary, server, userHash string, st stored, expectedVersion int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := rowKey(ns, boundary, server, userHash)
	cur, exists := f.rows[k]
	if expectedVersion == 0 {
		if exists {
			return errConflict
		}
		st.version = 1
		f.rows[k] = st
		return nil
	}
	if !exists || cur.version != expectedVersion {
		return errConflict
	}
	st.version = expectedVersion + 1
	f.rows[k] = st
	return nil
}

func (f *fakeStorage) del(_ context.Context, ns, boundary, server, userHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, rowKey(ns, boundary, server, userHash))
	return nil
}

func (f *fakeStorage) sweepExpired(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, st := range f.rows {
		if !st.expiresAt.IsZero() && st.expiresAt.Before(before) {
			delete(f.rows, k)
			n++
		}
	}
	return n, nil
}

type fakeExchanger struct {
	mu        sync.Mutex
	refreshed int
	revoked   []string
	newExpiry time.Time
}

func (f *fakeExchanger) Refresh(_ context.Context, _, _, _ string) (credresolve.Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed++
	return credresolve.Tokens{AccessToken: "access-2", RefreshToken: "refresh-2", ExpiresAt: f.newExpiry}, nil
}

func (f *fakeExchanger) Revoke(_ context.Context, _, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
}

func testSealer(t *testing.T) *LocalSealer {
	t.Helper()
	s, err := NewLocalSealer(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("NewLocalSealer: %v", err)
	}
	return s
}

func newBackend(t *testing.T, cfg BackendConfig) *Backend {
	t.Helper()
	if cfg.Storage == nil {
		cfg.Storage = newFakeStorage()
	}
	if cfg.Sealer == nil {
		cfg.Sealer = testSealer(t)
	}
	b, err := NewBackend(cfg)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	return b
}

// --- sealer ---

// TestLocalSealer_RoundTripAndTenantBinding: seal→unseal recovers the plaintext; a wrong
// tenant fails (per-tenant key binding).
func TestLocalSealer_RoundTripAndTenantBinding(t *testing.T) {
	t.Parallel()
	s := testSealer(t)
	ctx := context.Background()
	pt := []byte("super-secret-token")

	sealed, err := s.Seal(ctx, pt, "tenant-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, pt) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	got, err := s.Unseal(ctx, sealed, "tenant-a")
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("Unseal = (%q, %v), want %q", got, err, pt)
	}
	if _, err := s.Unseal(ctx, sealed, "tenant-b"); err == nil {
		t.Fatal("unseal under the wrong tenant must fail")
	}
}

// --- backend ---

// TestBackend_StoreResolveRoundTrip: a stored grant resolves to its token, and the sealed
// row at rest contains NONE of the plaintext (dump-is-inert).
func TestBackend_StoreResolveRoundTrip(t *testing.T) {
	t.Parallel()
	fs := newFakeStorage()
	b := newBackend(t, BackendConfig{Storage: fs})
	ctx := context.Background()

	if err := b.Store(ctx, "ns", "", "srv", "uh", Grant{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cred, err := b.Resolve(ctx, "ns", "", "srv", "uh")
	if err != nil || cred.Value != "access-1" {
		t.Fatalf("Resolve = (%+v, %v), want access-1", cred, err)
	}
	// Dump-is-inert: the persisted row must not contain the plaintext tokens.
	row := fs.rows[rowKey("ns", "", "srv", "uh")]
	if bytes.Contains(row.ciphertext, []byte("access-1")) || bytes.Contains(row.ciphertext, []byte("refresh-1")) {
		t.Fatal("plaintext token leaked into the stored ciphertext")
	}
}

// TestBackend_StoreGrant: the SPI write payload (credresolve.Grant) round-trips through the
// Postgres backend's GrantWriter adapter.
func TestBackend_StoreGrant(t *testing.T) {
	t.Parallel()
	b := newBackend(t, BackendConfig{})
	ctx := context.Background()

	g := credresolve.Grant{
		Tokens: credresolve.Tokens{AccessToken: "pg-tok", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)},
		Config: credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
	}
	if err := b.StoreGrant(ctx, "ns", "", "srv", "uh", g); err != nil {
		t.Fatalf("StoreGrant: %v", err)
	}
	cred, err := b.Resolve(ctx, "ns", "", "srv", "uh")
	if err != nil || cred.Value != "pg-tok" {
		t.Fatalf("Resolve after StoreGrant = (%+v, %v), want pg-tok", cred, err)
	}
}

// TestBackend_RefreshesNearExpiry: a near-expiry grant is refreshed once at resolve time and
// the rotated token is written back.
func TestBackend_RefreshesNearExpiry(t *testing.T) {
	t.Parallel()
	fs := newFakeStorage()
	ex := &fakeExchanger{newExpiry: time.Now().Add(time.Hour)}
	b := newBackend(t, BackendConfig{Storage: fs, Exchanger: ex})
	ctx := context.Background()

	// Stored token already past the refresh skew.
	if err := b.Store(ctx, "ns", "", "srv", "uh", Grant{
		AccessToken: "access-1", RefreshToken: "refresh-1", TokenEndpoint: "https://as/token", ClientID: "cid",
		ExpiresAt: time.Now().Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cred, err := b.Resolve(ctx, "ns", "", "srv", "uh")
	if err != nil || cred.Value != "access-2" {
		t.Fatalf("Resolve = (%+v, %v), want the refreshed access-2", cred, err)
	}
	if ex.refreshed != 1 {
		t.Fatalf("refreshed %d times, want exactly 1", ex.refreshed)
	}
}

// TestBackend_Precedence: no personal grant → org credential when the server is OAuth+org;
// consent-required when OAuth with no org; no-credential when not OAuth.
func TestBackend_Precedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// OAuth, org credential set → org credential.
	bOrg := newBackend(t, BackendConfig{
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
		OrgCredential: func(context.Context, string, string) (credresolve.Credential, error) {
			return credresolve.Credential{Kind: credresolve.KindBearer, Value: "org-tok"}, nil
		},
	})
	if cred, err := bOrg.Resolve(ctx, "ns", "", "srv", "uh"); err != nil || cred.Value != "org-tok" {
		t.Fatalf("org-path Resolve = (%+v, %v), want org-tok", cred, err)
	}

	// OAuth, no org → consent required.
	bConsent := newBackend(t, BackendConfig{
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
		OrgCredential: func(context.Context, string, string) (credresolve.Credential, error) {
			return credresolve.Credential{}, credresolve.ErrNoCredential
		},
	})
	if _, err := bConsent.Resolve(ctx, "ns", "", "srv", "uh"); !errors.Is(err, credresolve.ErrConsentRequired) {
		t.Fatalf("consent-path err = %v, want ErrConsentRequired", err)
	}

	// Not OAuth → no credential (open server).
	bOpen := newBackend(t, BackendConfig{
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return false, nil },
	})
	if _, err := bOpen.Resolve(ctx, "ns", "", "srv", "uh"); !errors.Is(err, credresolve.ErrNoCredential) {
		t.Fatalf("open-path err = %v, want ErrNoCredential", err)
	}
}

// TestBackend_Revoke: deletes the grant and best-effort revokes at the AS.
func TestBackend_Revoke(t *testing.T) {
	t.Parallel()
	fs := newFakeStorage()
	ex := &fakeExchanger{}
	b := newBackend(t, BackendConfig{Storage: fs, Exchanger: ex})
	ctx := context.Background()

	if err := b.Store(ctx, "ns", "", "srv", "uh", Grant{AccessToken: "a", RefreshToken: "r", RevocationEndpoint: "https://as/revoke", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := b.Revoke(ctx, "ns", "", "srv", "uh"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := fs.rows[rowKey("ns", "", "srv", "uh")]; ok {
		t.Fatal("grant not deleted on revoke")
	}
	if len(ex.revoked) != 1 || ex.revoked[0] != "r" {
		t.Fatalf("revoked = %v, want the refresh token revoked at the AS", ex.revoked)
	}
}
