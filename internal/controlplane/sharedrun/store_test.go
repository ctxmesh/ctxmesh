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

package sharedrun_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/sharedrun"
)

// eachStore runs one behavioural contract against the in-memory twin AND the Postgres store (the
// publishedartifact / namespacetenant conformance pattern). The twin always runs; the Postgres store runs
// only when CONTROLPLANE_TEST_DSN points at a throwaway DB (migrated by OpenDB + truncated first) — CI
// without a DB still exercises the contract via the twin. This satisfies m75.1's real-Postgres DoD: point
// CONTROLPLANE_TEST_DSN at a live pg16 (pgvector/pgvector:pg16 — plain postgres:16 fails migration 0003)
// and the same asserts run, including the hash-only-at-rest column check below.
func eachStore(t *testing.T, fn func(t *testing.T, s sharedrun.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, sharedrun.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE shared_runs`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, sharedrun.NewPostgresStore(db)) })
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// TestStore_Create_GetByTokenHash is the core round-trip: Create stores a record; GetByTokenHash finds it
// by the hash; the stored hash matches SHA-256(token) and is NOT the token itself.
func TestStore_Create_GetByTokenHash(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		const token = "s3cr3t-token-never-stored"
		h := hashToken(token)

		rec := sharedrun.SharedRun{
			ID:             "share-1",
			TokenHash:      h,
			RunID:          "run-abc",
			Namespace:      "team-a",
			Agent:          "support-bot",
			CreatedBy:      "alice",
			ExpiresAt:      time.Now().Add(time.Hour),
			IncludeContent: true,
		}
		require.NoError(t, s.Create(ctx, rec))

		got, ok, err := s.GetByTokenHash(ctx, h)
		require.NoError(t, err)
		require.True(t, ok, "the share must be found by its token hash")
		assert.Equal(t, "share-1", got.ID)
		assert.Equal(t, "run-abc", got.RunID)
		assert.Equal(t, "team-a", got.Namespace)
		assert.Equal(t, "support-bot", got.Agent, "V16: the agent is snapshotted at mint and round-trips")
		assert.Equal(t, "alice", got.CreatedBy)
		assert.True(t, got.IncludeContent)
		assert.False(t, got.Revoked)

		// The hash-at-rest guarantee: the stored hash is SHA-256(token), never the token.
		assert.Equal(t, h, got.TokenHash)
		assert.NotEqual(t, token, got.TokenHash, "the token itself must never be stored")

		// A missing hash returns not-found (never an error).
		_, ok, err = s.GetByTokenHash(ctx, hashToken("some-other-token"))
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

// TestStore_HashOnlyAtRest_PostgresColumn asserts DIRECTLY against the Postgres row that the token_hash
// column holds the hash and the raw token appears NOWHERE in the row (the m75.1 DoD: query the row, assert
// token_hash != token). Skipped without a DSN (there is no column to query in the twin).
func TestStore_HashOnlyAtRest_PostgresColumn(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — the hash-at-rest column check needs a real Postgres row")
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE shared_runs`)
	require.NoError(t, err)

	ctx := context.Background()
	s := sharedrun.NewPostgresStore(db)
	const token = "raw-token-that-must-not-leak-into-the-row"
	h := hashToken(token)
	require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
		ID: "share-hash", TokenHash: h, RunID: "run-1", Namespace: "ns", CreatedBy: "bob",
		ExpiresAt: time.Now().Add(time.Hour),
	}))

	var storedHash string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT token_hash FROM shared_runs WHERE id = $1`, "share-hash").Scan(&storedHash))
	assert.Equal(t, h, storedHash, "token_hash column holds the SHA-256")
	assert.NotEqual(t, token, storedHash, "the raw token must never be in the row")

	// Belt-and-braces: the raw token must not appear as a substring of ANY text column.
	var hits int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM shared_runs
		WHERE id LIKE '%' || $1 || '%'
		   OR token_hash LIKE '%' || $1 || '%'
		   OR run_id LIKE '%' || $1 || '%'
		   OR namespace LIKE '%' || $1 || '%'
		   OR created_by LIKE '%' || $1 || '%'`, token).Scan(&hits))
	assert.Equal(t, 0, hits, "the raw token must not appear in any column")
}

// TestStore_Revoke_Idempotent_ShownInList: Revoke flips the flag; GetByTokenHash still returns the row
// (revoked=true — the handler decides via IsLive), ListForRun INCLUDES revoked rows (V11: honest
// "what did I expose?" — the UI badges them as Revoked), and a double-revoke is a no-op.
func TestStore_Revoke_Idempotent_ShownInList(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		h := hashToken("revoke-me")
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "share-r", TokenHash: h, RunID: "run-r", Namespace: "ns", CreatedBy: "carol",
			ExpiresAt: time.Now().Add(time.Hour),
		}))

		require.NoError(t, s.Revoke(ctx, "share-r"))
		// The row is still fetchable by hash (raw row), but marked revoked — IsLive is false.
		got, ok, err := s.GetByTokenHash(ctx, h)
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, got.Revoked)
		assert.False(t, got.IsLive(time.Now()), "a revoked share is not live")

		// V11: the manage list INCLUDES revoked rows (UI badges them; "what did I expose?").
		list, err := s.ListForRun(ctx, "run-r")
		require.NoError(t, err)
		require.Len(t, list, 1, "revoked shares are now included in the list (V11)")
		assert.True(t, list[0].Revoked, "the revoked row is present and marked revoked")

		// Idempotent: a second revoke + revoking an absent id are both no-op successes.
		require.NoError(t, s.Revoke(ctx, "share-r"))
		require.NoError(t, s.Revoke(ctx, "does-not-exist"))
	})
}

// TestStore_ExpiryHonored: an expired share fetches by hash but IsLive(now) is false (the public read 404s
// it). A future-expiry share is live. Expiry is a value check, not a store filter — the handler decides.
func TestStore_ExpiryHonored(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		now := time.Now()

		expiredHash := hashToken("expired")
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "share-expired", TokenHash: expiredHash, RunID: "run-e", Namespace: "ns", CreatedBy: "dan",
			ExpiresAt: now.Add(-time.Minute), // already expired
		}))
		liveHash := hashToken("live")
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "share-live", TokenHash: liveHash, RunID: "run-e", Namespace: "ns", CreatedBy: "dan",
			ExpiresAt: now.Add(time.Hour),
		}))

		expired, ok, err := s.GetByTokenHash(ctx, expiredHash)
		require.NoError(t, err)
		require.True(t, ok, "the store returns the raw row even when expired (handler decides)")
		assert.False(t, expired.IsLive(now), "an expired share is not live")

		live, ok, err := s.GetByTokenHash(ctx, liveHash)
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, live.IsLive(now), "a future-expiry share is live")
	})
}

// TestStore_ListForRun: returns ALL shares for a run (including revoked, V11), newest-first, scoped to
// that run only.
func TestStore_ListForRun(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		base := time.Now()

		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "s-old", TokenHash: hashToken("t-old"), RunID: "run-list", Namespace: "ns", CreatedBy: "e",
			CreatedAt: base.Add(-time.Hour), ExpiresAt: base.Add(time.Hour),
		}))
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "s-new", TokenHash: hashToken("t-new"), RunID: "run-list", Namespace: "ns", CreatedBy: "e",
			CreatedAt: base, ExpiresAt: base.Add(time.Hour),
		}))
		// A revoked share must still appear in the list (V11: honest "what did I expose?").
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "s-revoked", TokenHash: hashToken("t-revoked"), RunID: "run-list", Namespace: "ns", CreatedBy: "e",
			CreatedAt: base.Add(-2 * time.Hour), ExpiresAt: base.Add(time.Hour),
		}))
		require.NoError(t, s.Revoke(ctx, "s-revoked"))
		// A share on a DIFFERENT run must not appear.
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "s-other", TokenHash: hashToken("t-other"), RunID: "run-other", Namespace: "ns", CreatedBy: "e",
			ExpiresAt: base.Add(time.Hour),
		}))

		list, err := s.ListForRun(ctx, "run-list")
		require.NoError(t, err)
		require.Len(t, list, 3, "all three shares on run-list (including revoked, V11)")
		assert.Equal(t, "s-new", list[0].ID, "newest-first ordering")
		assert.Equal(t, "s-old", list[1].ID)
		assert.Equal(t, "s-revoked", list[2].ID)
		// Revoked row is present and marked revoked.
		assert.True(t, list[2].Revoked, "the revoked row is included and flagged")
	})
}

// TestStore_ListByCreator is the caller-scoped "my shares" contract (V13): a principal's shares across
// EVERY run, newest-first, revoked/expired included — and ONLY that principal's (another creator's are
// excluded, the caller-scoping boundary).
func TestStore_ListByCreator(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		base := time.Now()

		// alice minted three shares across two different runs (one later revoked). Distinct agents prove the
		// V16 snapshot survives the ListByCreator scan.
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "a-old", TokenHash: hashToken("ta-old"), RunID: "run-1", Namespace: "ns", Agent: "triage-bot", CreatedBy: "alice",
			CreatedAt: base.Add(-time.Hour), ExpiresAt: base.Add(time.Hour),
		}))
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "a-new", TokenHash: hashToken("ta-new"), RunID: "run-2", Namespace: "ns", Agent: "research-bot", CreatedBy: "alice",
			CreatedAt: base, ExpiresAt: base.Add(time.Hour),
		}))
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "a-revoked", TokenHash: hashToken("ta-rev"), RunID: "run-3", Namespace: "ns", CreatedBy: "alice",
			CreatedAt: base.Add(-2 * time.Hour), ExpiresAt: base.Add(time.Hour),
		}))
		require.NoError(t, s.Revoke(ctx, "a-revoked"))
		// bob minted one — it must NEVER appear in alice's list (the caller-scoping boundary).
		require.NoError(t, s.Create(ctx, sharedrun.SharedRun{
			ID: "b-1", TokenHash: hashToken("tb-1"), RunID: "run-1", Namespace: "ns", CreatedBy: "bob",
			CreatedAt: base, ExpiresAt: base.Add(time.Hour),
		}))

		list, err := s.ListByCreator(ctx, "alice")
		require.NoError(t, err)
		require.Len(t, list, 3, "all of alice's shares across every run (incl. revoked, V13)")
		assert.Equal(t, "a-new", list[0].ID, "newest-first ordering")
		assert.Equal(t, "research-bot", list[0].Agent, "V16: the agent snapshot survives ListByCreator")
		assert.Equal(t, "a-old", list[1].ID)
		assert.Equal(t, "triage-bot", list[1].Agent)
		assert.Equal(t, "a-revoked", list[2].ID)
		assert.True(t, list[2].Revoked, "the revoked row is included and flagged")
		for _, rec := range list {
			assert.Equal(t, "alice", rec.CreatedBy, "another principal's shares never appear in a caller's list")
		}

		// bob sees only his own; an unknown principal sees nothing.
		bob, err := s.ListByCreator(ctx, "bob")
		require.NoError(t, err)
		require.Len(t, bob, 1)
		assert.Equal(t, "b-1", bob[0].ID)
		none, err := s.ListByCreator(ctx, "nobody")
		require.NoError(t, err)
		assert.Empty(t, none, "a principal with no shares gets an empty list")
	})
}

// TestStore_Create_Validates: missing required fields are rejected before any write.
func TestStore_Create_Validates(t *testing.T) {
	eachStore(t, func(t *testing.T, s sharedrun.Store) {
		ctx := context.Background()
		exp := time.Now().Add(time.Hour)
		assert.Error(t, s.Create(ctx, sharedrun.SharedRun{ID: "", TokenHash: "h", RunID: "r", ExpiresAt: exp}))
		assert.Error(t, s.Create(ctx, sharedrun.SharedRun{ID: "i", TokenHash: "", RunID: "r", ExpiresAt: exp}))
		assert.Error(t, s.Create(ctx, sharedrun.SharedRun{ID: "i", TokenHash: "h", RunID: "", ExpiresAt: exp}))
		assert.Error(t, s.Create(ctx, sharedrun.SharedRun{ID: "i", TokenHash: "h", RunID: "r"}), "zero expiry rejected")
	})
}
