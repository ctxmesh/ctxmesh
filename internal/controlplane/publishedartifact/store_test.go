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

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/publishedartifact"
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

// TestStore_ListTemplates_VisibilityPredicate: the leak-safe WHERE — org/public/team visible per the
// membership set, private never returned, tombstoned never returned, latest-version-per-origin.
func TestStore_ListTemplates_VisibilityPredicate(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()

		// org artifact in ns-b (a member) — visible to ns-caller.
		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-b", OriginName: "org-agent",
			SpecJSON: json.RawMessage(`{"name":"org-agent"}`), Visibility: "org", ContentHash: "h1",
		})
		require.NoError(t, err)

		// org artifact in ns-outside (NOT a member) — must not leak.
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-outside", OriginName: "leaked-agent",
			SpecJSON: json.RawMessage(`{"name":"leaked-agent"}`), Visibility: "org", ContentHash: "h2",
		})
		require.NoError(t, err)

		// public artifact in an unrelated namespace — always visible.
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-unrelated", OriginName: "public-agent",
			SpecJSON: json.RawMessage(`{"name":"public-agent"}`), Visibility: "public", ContentHash: "h3",
		})
		require.NoError(t, err)

		// team artifact in callerNS — visible (own-ns team).
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-caller", OriginName: "team-agent",
			SpecJSON: json.RawMessage(`{"name":"team-agent"}`), Visibility: "team", ContentHash: "h4",
		})
		require.NoError(t, err)

		// team artifact in ns-b (a member, but NOT callerNS) — must NOT appear (team is own-ns only).
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-b", OriginName: "sibling-team-agent",
			SpecJSON: json.RawMessage(`{"name":"sibling-team-agent"}`), Visibility: "team", ContentHash: "h5",
		})
		require.NoError(t, err)

		// private artifact in callerNS — must NEVER appear.
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-caller", OriginName: "private-agent",
			SpecJSON: json.RawMessage(`{"name":"private-agent"}`), Visibility: "private", ContentHash: "h6",
		})
		require.NoError(t, err)

		rows, err := s.ListTemplates(ctx, "ns-caller", []string{"ns-caller", "ns-b"})
		require.NoError(t, err)

		names := make(map[string]bool)
		for _, r := range rows {
			names[r.OriginName] = true
		}

		assert.True(t, names["org-agent"], "org artifact from member ns-b must appear")
		assert.True(t, names["public-agent"], "public artifact must appear regardless of membership")
		assert.True(t, names["team-agent"], "team artifact in callerNS must appear")
		assert.False(t, names["leaked-agent"], "org artifact in non-member ns must NOT appear (leak-safe)")
		assert.False(t, names["sibling-team-agent"], "team artifact in sibling member ns must NOT appear")
		assert.False(t, names["private-agent"], "private artifact must NEVER appear")
	})
}

// TestStore_ListTemplates_LatestVersionWins: DISTINCT ON (kind, origin_namespace, origin_name)
// ORDER BY version DESC returns only the highest non-tombstoned version per origin.
func TestStore_ListTemplates_LatestVersionWins(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()

		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "evolving",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "org", ContentHash: "h-v1",
		})
		require.NoError(t, err)
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "evolving",
			SpecJSON: json.RawMessage(`{"v":2}`), Visibility: "org", ContentHash: "h-v2",
		})
		require.NoError(t, err)

		rows, err := s.ListTemplates(ctx, "ns-caller", []string{"ns-caller", "ns-a"})
		require.NoError(t, err)
		require.Len(t, rows, 1, "exactly one row per origin (latest version)")
		assert.Equal(t, 2, rows[0].Version, "version must be 2 (latest)")
		assert.Equal(t, "h-v2", rows[0].ContentHash)
		assert.JSONEq(t, `{"v":2}`, string(rows[0].SpecJSON))
	})
}

// TestStore_ListTemplates_TombstonedExcluded: a tombstoned artifact must never appear.
func TestStore_ListTemplates_TombstonedExcluded(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()

		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-caller", OriginName: "dead-agent",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "public", ContentHash: "h1",
		})
		require.NoError(t, err)
		require.NoError(t, s.Tombstone(ctx, kindAgent, "ns-caller", "dead-agent"))

		rows, err := s.ListTemplates(ctx, "ns-caller", []string{"ns-caller"})
		require.NoError(t, err)
		assert.Empty(t, rows, "tombstoned artifact must not appear")
	})
}

// TestStore_ListTemplates_EmptyMembersPublicOnly: when members is nil/empty, only public rows appear
// (the COALESCE guard — parity with the m73.3 fix that guarded the org clause against SQL NULL).
func TestStore_ListTemplates_EmptyMembersPublicOnly(t *testing.T) {
	eachStore(t, func(t *testing.T, s publishedartifact.Store) {
		ctx := context.Background()

		_, err := s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "org-agent",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "org", ContentHash: "h1",
		})
		require.NoError(t, err)
		_, err = s.Publish(ctx, publishedartifact.PublishedArtifact{
			Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "public-agent",
			SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "public", ContentHash: "h2",
		})
		require.NoError(t, err)

		// nil members → COALESCE makes it an empty array → no org matches, only public.
		rows, err := s.ListTemplates(ctx, "ns-caller", nil)
		require.NoError(t, err)

		names := make(map[string]bool)
		for _, r := range rows {
			names[r.OriginName] = true
		}
		assert.True(t, names["public-agent"], "public artifact must appear even with nil members")
		assert.False(t, names["org-agent"], "org artifact must NOT appear when members is nil (COALESCE guard)")
	})
}
