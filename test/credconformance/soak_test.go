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

package credconformance_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/credpostgres"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// auditSink records every credential-plane ActionUse, thread-safe, so the soak can prove
// no grant was ever attributed to the wrong user.
type auditSink struct {
	mu       sync.Mutex
	perUser  map[string]int
	badClass int
}

func newAuditSink() *auditSink { return &auditSink{perUser: map[string]int{}} }

func (a *auditSink) record(e credresolve.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Action == credresolve.ActionUse {
		a.perUser[e.UserHash]++
		if e.Class != credresolve.ClassPersonalGrant {
			a.badClass++
		}
	}
}

// TestSoak_NoBleed_Plane drives the FULL central credential plane — credplane.Client →
// Server → the shared backend (cache DISABLED so every resolve hits the shared cache /
// singleflight / storage) → audit — under sustained concurrent DISTINCT-user load, and
// asserts ZERO cross-attribution (ADR 0030's bleed invariant) in both the returned tokens
// and the plane audit. This is the m27.8 no-bleed soak; it uses synthetic per-user grants
// (no real accounts needed).
func TestSoak_NoBleed_Plane(t *testing.T) {
	const users, rounds = 32, 30 // 960 concurrent distinct-user resolves through the plane
	const ns, server = "soak-ns", "srv"
	ctx := context.Background()

	sink := newAuditSink()
	sealer, err := credpostgres.NewLocalSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	backend, err := credpostgres.NewBackend(credpostgres.BackendConfig{
		Storage:         credpostgres.NewMemStore(),
		Sealer:          sealer,
		AuthTypeIsOAuth: oauthAlways,
		Audit:           sink.record,
		CacheTTL:        -1, // disable the cache: every resolve exercises the shared path
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	// Seed a DISTINCT token per user.
	want := make([]string, users)
	for i := range users {
		want[i] = fmt.Sprintf("token-%03d", i)
		g := credpostgres.Grant{AccessToken: want[i], ExpiresAt: farFuture}
		if err := backend.Store(ctx, ns, "", server, userHash(i), g); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Drive the plane over the real client→server delegation path.
	srv := httptest.NewServer(credplane.NewServer(backend, logr.Discard()).Handler())
	defer srv.Close()
	client := credplane.NewClient(srv.URL, nil)

	var bleed, resolves int64
	var mu sync.Mutex
	for range rounds {
		var wg sync.WaitGroup
		for i := range users {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				cred, err := client.Resolve(ctx, ns, "", server, userHash(i))
				mu.Lock()
				resolves++
				if err != nil || cred.Value != want[i] {
					bleed++
					t.Errorf("CROSS-ATTRIBUTION: user %d resolved %q (err %v), want %q", i, cred.Value, err, want[i])
				}
				mu.Unlock()
			}(i)
		}
		wg.Wait()
	}

	if bleed != 0 {
		t.Fatalf("no-bleed soak FAILED: %d/%d resolves cross-attributed", bleed, resolves)
	}
	// Audit: exactly one ActionUse per resolve, every one attributed to a seeded user with
	// the personal-grant class — no phantom user, no mis-classed event.
	total := 0
	for uh, c := range sink.perUser {
		if !isSeededUser(uh, users) {
			t.Errorf("audit recorded a phantom user %q", uh)
		}
		if c != rounds {
			t.Errorf("user %q audited %d times, want %d (per-user attribution drift)", uh, c, rounds)
		}
		total += c
	}
	if total != users*rounds || sink.badClass != 0 {
		t.Fatalf("audit total=%d (want %d), mis-classed=%d", total, users*rounds, sink.badClass)
	}
	t.Logf("no-bleed soak OK: %d users x %d rounds = %d plane resolves, zero cross-attribution", users, rounds, resolves)
}

func userHash(i int) string { return fmt.Sprintf("userhash-%03d", i) }

func isSeededUser(uh string, users int) bool {
	for i := range users {
		if uh == userHash(i) {
			return true
		}
	}
	return false
}
