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

// Package credconformance is the credential-store backend CONFORMANCE SUITE (ADR 0032,
// spec credential-store-spi): the checklist every backend — in-tree or third-party — must
// pass to be trustworthy. It is the CSI-sanity / cert-manager-conformance model applied to
// credential backends. Run it against any credresolve.CredentialResolver via a Subject.
package credconformance

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// Subject describes a backend under test: the resolver, a way to seed a resolvable grant,
// and optional capabilities (crypto-shred). ns→tenant mapping lets the shred check target a
// tenant's key.
type Subject struct {
	// Name labels the subject in subtest output.
	Name string
	// Resolver is the backend under test.
	Resolver credresolve.CredentialResolver
	// Seed installs a grant so Resolve(ns, server, userHash) returns token.
	Seed func(ctx context.Context, ns, server, userHash, token string) error
	// TenantForNS maps a namespace to the tenant key (for the crypto-shred check). nil ⇒ ns.
	TenantForNS func(ns string) string
	// Shred destroys a tenant's KEK (crypto-shred). nil ⇒ the backend does not claim it, and
	// the crypto-shred check is skipped.
	Shred func(ctx context.Context, tenant string) error
}

func (s Subject) tenant(ns string) string {
	if s.TenantForNS != nil {
		return s.TenantForNS(ns)
	}
	return ns
}

// Run executes every conformance check that applies to the Subject.
func Run(t *testing.T, s Subject) {
	t.Run(s.Name+"/isolation", func(t *testing.T) { s.checkIsolation(t) })
	t.Run(s.Name+"/revoke", func(t *testing.T) { s.checkRevoke(t) })
	if s.Shred != nil {
		t.Run(s.Name+"/crypto-shred", func(t *testing.T) { s.checkCryptoShred(t) })
	}
}

// checkIsolation: N distinct users resolved CONCURRENTLY each get their OWN token — zero
// cross-attribution (the ADR 0030 bleed invariant, the core of a shared credential plane).
func (s Subject) checkIsolation(t *testing.T) {
	ctx := context.Background()
	const n = 24
	const server = "srv"
	const ns = "iso-ns"
	want := map[string]string{}
	for i := range n {
		user := fmt.Sprintf("user-%02d", i)
		tok := fmt.Sprintf("token-for-%02d", i)
		want[user] = tok
		if err := s.Seed(ctx, ns, server, user, tok); err != nil {
			t.Fatalf("seed %s: %v", user, err)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]string{}
	errs := map[string]error{}
	for user := range want {
		wg.Add(1)
		go func(user string) {
			defer wg.Done()
			cred, err := s.Resolver.Resolve(ctx, ns, server, user)
			mu.Lock()
			got[user], errs[user] = cred.Value, err
			mu.Unlock()
		}(user)
	}
	wg.Wait()

	for user, wantTok := range want {
		if errs[user] != nil {
			t.Errorf("user %s resolve error: %v", user, errs[user])
			continue
		}
		if got[user] != wantTok {
			t.Errorf("CROSS-ATTRIBUTION: user %s resolved %q, want %q", user, got[user], wantTok)
		}
	}
}

// checkRevoke: after Revoke a resolve no longer returns the token (fails closed).
func (s Subject) checkRevoke(t *testing.T) {
	ctx := context.Background()
	const ns, server, user = "rev-ns", "srv", "user-1"
	if err := s.Seed(ctx, ns, server, user, "revoke-me"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if cred, err := s.Resolver.Resolve(ctx, ns, server, user); err != nil || cred.Value != "revoke-me" {
		t.Fatalf("pre-revoke resolve = (%+v, %v), want the seeded token", cred, err)
	}
	if err := s.Resolver.Revoke(ctx, ns, server, user); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	cred, err := s.Resolver.Resolve(ctx, ns, server, user)
	if err == nil && cred.Value == "revoke-me" {
		t.Fatal("token still resolves AFTER revoke — revoke did not take effect")
	}
}

// checkCryptoShred: destroying one tenant's KEK makes that tenant's grants unrecoverable,
// while another tenant is unaffected (per-tenant isolation of the shred).
func (s Subject) checkCryptoShred(t *testing.T) {
	ctx := context.Background()
	const server, user = "srv", "user-1"
	nsA, nsB := "shred-tenant-a", "shred-tenant-b"
	if err := s.Seed(ctx, nsA, server, user, "a-token"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := s.Seed(ctx, nsB, server, user, "b-token"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	if err := s.Shred(ctx, s.tenant(nsA)); err != nil {
		t.Fatalf("Shred(A): %v", err)
	}
	// A is unrecoverable...
	if cred, err := s.Resolver.Resolve(ctx, nsA, server, user); err == nil && cred.Value == "a-token" {
		t.Fatal("tenant A's token still resolves after crypto-shred")
	}
	// ...B is unaffected.
	cred, err := s.Resolver.Resolve(ctx, nsB, server, user)
	if err != nil || cred.Value != "b-token" {
		t.Fatalf("tenant B resolve = (%+v, %v) after shredding A, want b-token (isolation)", cred, err)
	}
}
