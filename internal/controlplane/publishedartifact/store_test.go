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

package publishedartifact_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/publishedartifact"
)

// eachStore runs one behavioural contract against the in-memory twin AND the Postgres store (the
// namespacetenant / onlinescore conformance pattern). The twin always runs; the Postgres store runs only
// when CONTROLPLANE_TEST_DSN points at a throwaway DB (migrated by OpenDB + truncated first) — CI without a
// DB still exercises the contract via the twin. This satisfies m74.1's real-Postgres snapshot-publish DoD:
// point CONTROLPLANE_TEST_DSN at a live pg16 (pgvector/pgvector:pg16 — plain postgres:16 fails migration
// 0003) and the same asserts run.
func eachStore(t *testing.T, fn func(t *testing.T, s publishedartifact.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, publishedartifact.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE published_artifacts`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, publishedartifact.NewPostgresStore(db)) })
}

const kindAgent = "agent"

// TestStore_Publish_MonotonicPerOrigin is the core release contract: publish cuts v1 then v2 for one origin,
// independent per origin; the stored content_hash + spec round-trip; GetLatest returns the newest version.
func TestStore_Publish_MonotonicPerOrigin(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()

		v1, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind:            kindAgent,
			OriginNamespace: "team-a",
			OriginName:      "assistant",
			SpecJSON:        json.RawMessage(`{"name":"assistant","v":1}`),
			Visibility:      "team",
			ContentHash:     "hash-v1",
		})
		require.NoError(t, err)
		assert.Equal(t, 1, v1, "first publish is v1")

		v2, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind:            kindAgent,
			OriginNamespace: "team-a",
			OriginName:      "assistant",
			SpecJSON:        json.RawMessage(`{"name":"assistant","v":2}`),
			Visibility:      "org",
			ContentHash:     "hash-v2",
		})
		require.NoError(t, err)
		assert.Equal(t, 2, v2, "re-publish cuts v2")

		// A DIFFERENT origin starts its own version sequence at 1 (per-origin monotonic, independent).
		vOther, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind:            kindAgent,
			OriginNamespace: "team-b",
			OriginName:      "assistant",
			SpecJSON:        json.RawMessage(`{"name":"assistant","other":true}`),
			Visibility:      "public",
			ContentHash:     "hash-other",
		})
		require.NoError(t, err)
		assert.Equal(t, 1, vOther, "an independent origin's first publish is v1, not 3")

		// GetLatest returns v2 for team-a with its spec + content_hash intact.
		latest, ok, err := s.GetLatest(ctx, kindAgent, "team-a", "assistant")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 2, latest.Version)
		assert.Equal(t, "org", latest.Visibility)
		assert.Equal(t, "hash-v2", latest.ContentHash)
		assert.JSONEq(t, `{"name":"assistant","v":2}`, string(latest.SpecJSON))
		assert.False(t, latest.PublishedAt.IsZero(), "published_at is stamped")
	})
}

// TestStore_Tombstone_HidesArtifact: Tombstone flags every version so GetLatest reports not-found (the
// unpublish path — the fork-time gate then 404s).
func TestStore_Tombstone_HidesArtifact(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()
		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "team-c", OriginName: "bot",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "team", ContentHash: "h1",
		})
		require.NoError(t, err)
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "team-c", OriginName: "bot",
			SpecJSON: json.RawMessage(`{"v":2}`), Visibility: "team", ContentHash: "h2",
		})
		require.NoError(t, err)

		require.NoError(t, s.Tombstone(ctx, kindAgent, "team-c", "bot"))

		_, ok, err := s.GetLatest(ctx, kindAgent, "team-c", "bot")
		require.NoError(t, err)
		assert.False(t, ok, "a tombstoned artifact must be hidden from GetLatest")
	})
}

// TestStore_Tombstone_Idempotent: tombstoning an absent or already-tombstoned artifact is a no-op success.
func TestStore_Tombstone_Idempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()
		// Absent artifact → no error.
		require.NoError(t, s.Tombstone(ctx, kindAgent, "nope", "nope"))

		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "team-d", OriginName: "svc",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "team", ContentHash: "h1",
		})
		require.NoError(t, err)
		require.NoError(t, s.Tombstone(ctx, kindAgent, "team-d", "svc"))
		// Double tombstone → still a no-op success.
		require.NoError(t, s.Tombstone(ctx, kindAgent, "team-d", "svc"))
	})
}

// TestStore_GetLatest_Missing: an unpublished artifact returns (nil, false, nil) — not an error.
func TestStore_GetLatest_Missing(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()
		rec, ok, err := s.GetLatest(ctx, kindAgent, "team-x", "never-published")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Nil(t, rec)
	})
}

// TestStore_Publish_Validates: missing required fields are rejected before any write.
func TestStore_Publish_Validates(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()
		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "", OriginName: "x",
			SpecJSON: json.RawMessage(`{}`), Visibility: "team", ContentHash: "h",
		})
		assert.Error(t, err, "empty origin_namespace is rejected")

		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns", OriginName: "x",
			SpecJSON: nil, Visibility: "team", ContentHash: "h",
		})
		assert.Error(t, err, "empty spec_json is rejected")
	})
}
