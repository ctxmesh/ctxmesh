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

package agentmemory

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

const embModel = "text-embedding-3-small"

// testDim matches the schema's vector(1536) column. pad grows a short test vector to it, zero-filling the
// tail — the leading values carry the direction, so cosine similarity between two padded prefixes is
// unchanged (this lets the tests reason in 2-3 dims while the pg column enforces the real dimension).
const testDim = 1536

func pad(vec ...float32) []float32 {
	out := make([]float32, testDim)
	copy(out, vec)
	return out
}

// eachStore holds the in-memory twin and the Postgres (pgvector) store to ONE behavioural contract (the M40
// pattern). The twin always runs; Postgres runs only when CONTROLPLANE_TEST_DSN points at a throwaway DB whose
// Postgres has the `vector` extension available (the 0003 migration creates it).
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE agent_memories`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

func mem(scope, subject, content string, vec ...float32) AgentMemory {
	return AgentMemory{
		Namespace: "prod", AgentName: "assistant", Scope: scope, Subject: subject,
		Content: content, EmbeddingModel: embModel, EmbeddingDim: testDim, Embedding: pad(vec...),
	}
}

// Remember stores a memory; Search returns the nearest by cosine, honoring TopK.
func TestStore_RememberAndSearchByCosine(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Remember(ctx, mem(ScopeAgent, "", "likes metric units", 1, 0, 0))
		require.NoError(t, err)
		_, err = s.Remember(ctx, mem(ScopeAgent, "", "based in Berlin", 0, 1, 0))
		require.NoError(t, err)

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
			Vector: pad(1, 0, 0), TopK: 1,
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "likes metric units", got[0].Memory.Content)
		assert.InDelta(t, 1.0, got[0].Score, 0.001)
	})
}

// Re-remembering the same content in the same partition updates in place — never a duplicate.
func TestStore_RememberIdempotentByContent(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		first, err := s.Remember(ctx, mem(ScopeAgent, "", "prefers dark mode", 1, 0, 0))
		require.NoError(t, err)
		second, err := s.Remember(ctx, mem(ScopeAgent, "", "prefers dark mode", 0, 1, 0)) // same content, new vector
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID, "same content re-remembered keeps the same row")
		n, err := s.Count(ctx, "prod", "assistant", ScopeAgent, "")
		require.NoError(t, err)
		assert.Equal(t, 1, n, "no duplicate row")
	})
}

// Per-user isolation (ADR 0045): a user's memory is never returned for another user.
func TestStore_SearchPerUserIsolation(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Remember(ctx, mem(ScopeAgentUser, "alice", "alice secret", 1, 0, 0))
		require.NoError(t, err)

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", AgentName: "assistant", Scope: ScopeAgentUser, Subject: "bob",
			EmbeddingModel: embModel, Vector: pad(1, 0, 0), TopK: 5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "a user must never retrieve another user's memory")
	})
}

// Cross-model vectors are excluded (comparing them is silently wrong — ADR 0045).
func TestStore_SearchEmbeddingModelFilter(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		other := mem(ScopeAgent, "", "stored with a different model", 1, 0, 0)
		other.EmbeddingModel = "some-other-model"
		_, err := s.Remember(ctx, other)
		require.NoError(t, err)

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
			Vector: pad(1, 0, 0), TopK: 5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "a row embedded with a different model must not be compared")
	})
}

// The similarity threshold drops weak matches.
func TestStore_SearchThreshold(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Remember(ctx, mem(ScopeAgent, "", "orthogonal", 0, 1, 0)) // cosine 0 vs the query
		require.NoError(t, err)

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
			Vector: pad(1, 0, 0), TopK: 5, Threshold: 0.5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "a cosine-0 match is below the 0.5 threshold")
	})
}

// List returns an agent's memories newest-first; Delete removes by id and refuses cross-agent deletes.
func TestStore_ListAndDelete(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		m, err := s.Remember(ctx, mem(ScopeAgent, "", "deletable", 1, 0, 0))
		require.NoError(t, err)

		list, err := s.List(ctx, ListOptions{Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent})
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, "deletable", list[0].Content)
		assert.Nil(t, list[0].Embedding, "reads do not echo the raw vector")

		assert.ErrorIs(t, s.Delete(ctx, "prod", "different-agent", m.ID), controlplane.ErrNotFound)
		require.NoError(t, s.Delete(ctx, "prod", "assistant", m.ID))
		assert.ErrorIs(t, s.Delete(ctx, "prod", "assistant", m.ID), controlplane.ErrNotFound)
	})
}

func TestStore_ValidateRejections(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		bad := map[string]AgentMemory{
			"agent-wide with a subject":  mem(ScopeAgent, "alice", "x", 1, 0, 0),
			"per-user without a subject": mem(ScopeAgentUser, "", "x", 1, 0, 0),
			"unknown scope":              mem("bogus", "", "x", 1, 0, 0),
		}
		for name, m := range bad {
			_, err := s.Remember(ctx, m)
			assert.ErrorIs(t, err, controlplane.ErrInvalid, name)
		}
		dimMismatch := mem(ScopeAgent, "", "x", 1, 0, 0)
		dimMismatch.EmbeddingDim = 5
		_, err := s.Remember(ctx, dimMismatch)
		assert.ErrorIs(t, err, controlplane.ErrInvalid, "embedding length != dim")
	})
}
