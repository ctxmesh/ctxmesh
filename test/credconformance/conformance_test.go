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
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ctxmesh/agent-engine/internal/credpostgres"
	"github.com/ctxmesh/agent-engine/internal/credprovider"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/test/credconformance"
)

var farFuture = time.Now().Add(48 * time.Hour)

func oauthAlways(context.Context, string, string) (bool, error) { return true, nil }

func newLocalBackend(t *testing.T, storage credpostgres.Storage) *credpostgres.Backend {
	t.Helper()
	sealer, err := credpostgres.NewLocalSealer(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	b, err := credpostgres.NewBackend(credpostgres.BackendConfig{
		Storage: storage, Sealer: sealer, AuthTypeIsOAuth: oauthAlways,
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return b
}

func seedViaBackend(b *credpostgres.Backend) func(context.Context, string, string, string, string) error {
	return func(ctx context.Context, ns, server, userHash, token string) error {
		return b.Store(ctx, ns, "", server, userHash, credpostgres.Grant{AccessToken: token, ExpiresAt: farFuture})
	}
}

// TestConformance_PostgresInMemory: the Postgres backend (in-memory store + local sealer)
// passes the suite hermetically — the in-tree scale backend conforms.
func TestConformance_PostgresInMemory(t *testing.T) {
	b := newLocalBackend(t, credpostgres.NewMemStore())
	credconformance.Run(t, credconformance.Subject{
		Name:     "postgres-mem",
		Resolver: b,
		Seed:     seedViaBackend(b),
	})
}

// pgAdapter adapts a credpostgres.Backend to the credprovider.Backend contract so the SAME
// backend can be served over the out-of-tree JSON-over-mTLS path and re-run through the suite.
type pgAdapter struct{ b *credpostgres.Backend }

func (a pgAdapter) Capabilities(context.Context) (credprovider.Capabilities, error) {
	return credprovider.Capabilities{}, nil
}

func (a pgAdapter) Resolve(
	ctx context.Context, ns, boundary, server, userHash, _ string,
) (credresolve.Credential, error) {
	return a.b.Resolve(ctx, ns, boundary, server, userHash)
}

func (a pgAdapter) Store(
	ctx context.Context, ns, boundary, server, userHash, _ string, g credprovider.GrantMaterial,
) error {
	exp := farFuture
	if g.ExpiresAtUnix > 0 {
		exp = time.Unix(g.ExpiresAtUnix, 0)
	}
	return a.b.Store(ctx, ns, boundary, server, userHash, credpostgres.Grant{AccessToken: g.AccessToken, ExpiresAt: exp})
}

func (a pgAdapter) Revoke(ctx context.Context, ns, boundary, server, userHash, _ string) error {
	return a.b.Revoke(ctx, ns, boundary, server, userHash)
}

// TestConformance_OutOfTreeRemote: the same backend served over the credprovider contract
// (Client → Handler) ALSO passes the suite — the BYO-vault path conforms end-to-end.
func TestConformance_OutOfTreeRemote(t *testing.T) {
	b := newLocalBackend(t, credpostgres.NewMemStore())
	srv := httptest.NewServer(credprovider.NewHandler(pgAdapter{b}))
	defer srv.Close()
	client := credprovider.NewClient(srv.URL, srv.Client())

	credconformance.Run(t, credconformance.Subject{
		Name:     "remote-credprovider",
		Resolver: client,
		Seed: func(ctx context.Context, ns, server, userHash, token string) error {
			g := credprovider.GrantMaterial{AccessToken: token, ExpiresAtUnix: farFuture.Unix()}
			return client.Store(ctx, ns, "", server, userHash, "", g)
		},
	})
}

// TestConformance_PostgresOpenBao: the FULL scale profile — real Postgres + OpenBao transit
// — passes the suite INCLUDING crypto-shred. Gated on CREDPOSTGRES_TEST_DSN + OPENBAO_TEST_*.
func TestConformance_PostgresOpenBao(t *testing.T) {
	dsn := os.Getenv("CREDPOSTGRES_TEST_DSN")
	addr, token := os.Getenv("OPENBAO_TEST_ADDR"), os.Getenv("OPENBAO_TEST_TOKEN")
	if dsn == "" || addr == "" || token == "" {
		t.Skip("set CREDPOSTGRES_TEST_DSN + OPENBAO_TEST_ADDR + OPENBAO_TEST_TOKEN to run the full-profile conformance")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "TRUNCATE credential_grants"); err != nil {
		// table may not exist yet — NewStore creates it below; ignore.
		_ = err
	}
	store, err := credpostgres.NewStore(ctx, db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_, _ = db.ExecContext(ctx, "TRUNCATE credential_grants")

	sealer, err := credpostgres.NewTransitSealer(credpostgres.TransitSealerConfig{
		Address: addr, Token: token, KeyPrefix: "conf-",
	})
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	b, err := credpostgres.NewBackend(credpostgres.BackendConfig{
		Storage: store, Sealer: sealer, AuthTypeIsOAuth: oauthAlways,
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	credconformance.Run(t, credconformance.Subject{
		Name:     "postgres-openbao",
		Resolver: b,
		Seed:     seedViaBackend(b),
		Shred:    func(ctx context.Context, tenant string) error { return sealer.CryptoShred(ctx, tenant) },
	})
}
