//go:build integration

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

package enduseragent_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/enduseragent"
)

// eachStore runs a contract against the in-memory twin AND the Postgres store (when
// CONTROLPLANE_TEST_DSN points at a throwaway migrated DB).
func eachStore(t *testing.T, fn func(t *testing.T, s enduseragent.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, enduseragent.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE end_user_agents`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, enduseragent.NewPostgresStore(db)) })
}

func TestStore_ExposureGate(t *testing.T) {
	eachStore(t, func(t *testing.T, s enduseragent.Store) {
		ctx := context.Background()

		// Not exposed → fail-closed (zero, false, nil).
		_, ok, err := s.Get(ctx, "ns1", "chatbot")
		require.NoError(t, err)
		assert.False(t, ok, "an un-mirrored agent is not exposed (BFF 404s)")

		// Expose it (reconciler Set) → resolves with the endpoint + spec.
		want := enduseragent.ExposedAgent{
			Namespace: "ns1", Agent: "chatbot", Endpoint: "https://chatbot.ns1.example.com",
			RecordCapable: true, OutputSchema: `{"type":"object"}`,
		}
		require.NoError(t, s.Set(ctx, want))
		got, ok, err := s.Get(ctx, "ns1", "chatbot")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, want, got)

		// Not-Ready (empty endpoint) still resolves ok — the BFF distinguishes 409 from 404.
		require.NoError(t, s.Set(ctx, enduseragent.ExposedAgent{Namespace: "ns1", Agent: "chatbot"}))
		got, ok, err = s.Get(ctx, "ns1", "chatbot")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Empty(t, got.Endpoint, "empty endpoint ⇒ not Ready (409, not 404)")

		// Delete (reconciler prune when endUserAccess unset) → back to fail-closed.
		require.NoError(t, s.Delete(ctx, "ns1", "chatbot"))
		_, ok, err = s.Get(ctx, "ns1", "chatbot")
		require.NoError(t, err)
		assert.False(t, ok)

		// Same agent name in a DIFFERENT namespace is independent.
		require.NoError(t, s.Set(ctx, enduseragent.ExposedAgent{Namespace: "ns2", Agent: "chatbot", Endpoint: "u"}))
		_, ok, _ = s.Get(ctx, "ns1", "chatbot")
		assert.False(t, ok)
		_, ok, _ = s.Get(ctx, "ns2", "chatbot")
		assert.True(t, ok)
	})
}
