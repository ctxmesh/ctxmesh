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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

const embModel = "text-embedding-3-small"

func mem(scope, subject, content string, vec ...float32) AgentMemory {
	return AgentMemory{
		Namespace: "prod", AgentName: "assistant", Scope: scope, Subject: subject,
		Content: content, EmbeddingModel: embModel, EmbeddingDim: len(vec), Embedding: vec,
	}
}

// Remember stores a memory; Search returns the nearest by cosine, honoring TopK.
func TestRemember_AndSearchByCosine(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	_, err := s.Remember(ctx, mem(ScopeAgent, "", "likes metric units", 1, 0, 0))
	require.NoError(t, err)
	_, err = s.Remember(ctx, mem(ScopeAgent, "", "based in Berlin", 0, 1, 0))
	require.NoError(t, err)

	got, err := s.Search(ctx, SearchQuery{
		Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
		Vector: []float32{1, 0, 0}, TopK: 1,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "likes metric units", got[0].Memory.Content)
	assert.InDelta(t, 1.0, got[0].Score, 0.001)
}

// Re-remembering the same content in the same partition updates in place — never a duplicate.
func TestRemember_IdempotentByContent(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	first, err := s.Remember(ctx, mem(ScopeAgent, "", "prefers dark mode", 1, 0, 0))
	require.NoError(t, err)
	second, err := s.Remember(ctx, mem(ScopeAgent, "", "prefers dark mode", 0, 1, 0)) // same content, new vector
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "same content re-remembered keeps the same row")
	n, err := s.Count(ctx, "prod", "assistant", ScopeAgent, "")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "no duplicate row")
}

// Per-user isolation (ADR 0045): a user's memory is never returned for another user.
func TestSearch_PerUserIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	_, err := s.Remember(ctx, mem(ScopeAgentUser, "alice", "alice secret", 1, 0, 0))
	require.NoError(t, err)

	// Bob searches the same agent with an identical query vector — must see nothing of Alice's.
	got, err := s.Search(ctx, SearchQuery{
		Namespace: "prod", AgentName: "assistant", Scope: ScopeAgentUser, Subject: "bob",
		EmbeddingModel: embModel, Vector: []float32{1, 0, 0}, TopK: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, got, "a user must never retrieve another user's memory")
}

// Cross-model vectors are excluded (comparing them is silently wrong — ADR 0045).
func TestSearch_EmbeddingModelFilter(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	other := mem(ScopeAgent, "", "stored with a different model", 1, 0, 0)
	other.EmbeddingModel = "some-other-model"
	_, err := s.Remember(ctx, other)
	require.NoError(t, err)

	got, err := s.Search(ctx, SearchQuery{
		Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
		Vector: []float32{1, 0, 0}, TopK: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, got, "a row embedded with a different model must not be compared")
}

// The similarity threshold drops weak matches.
func TestSearch_Threshold(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	_, err := s.Remember(ctx, mem(ScopeAgent, "", "orthogonal", 0, 1, 0)) // cosine 0 vs the query
	require.NoError(t, err)

	got, err := s.Search(ctx, SearchQuery{
		Namespace: "prod", AgentName: "assistant", Scope: ScopeAgent, EmbeddingModel: embModel,
		Vector: []float32{1, 0, 0}, TopK: 5, Threshold: 0.5,
	})
	require.NoError(t, err)
	assert.Empty(t, got, "a cosine-0 match is below the 0.5 threshold")
}

func TestDelete_AndNotFound(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	m, err := s.Remember(ctx, mem(ScopeAgent, "", "deletable", 1, 0, 0))
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, "prod", "assistant", m.ID))
	assert.ErrorIs(t, s.Delete(ctx, "prod", "assistant", m.ID), controlplane.ErrNotFound)
	// Wrong agent → not found (no cross-agent delete).
	m2, _ := s.Remember(ctx, mem(ScopeAgent, "", "other", 1, 0, 0))
	assert.ErrorIs(t, s.Delete(ctx, "prod", "different-agent", m2.ID), controlplane.ErrNotFound)
}

func TestValidate_Rejections(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	cases := map[string]AgentMemory{
		"agent-wide with a subject":  mem(ScopeAgent, "alice", "x", 1, 0, 0),
		"per-user without a subject": mem(ScopeAgentUser, "", "x", 1, 0, 0),
		"unknown scope":              mem("bogus", "", "x", 1, 0, 0),
	}
	for name, m := range cases {
		_, err := s.Remember(ctx, m)
		assert.ErrorIs(t, err, controlplane.ErrInvalid, name)
	}
	// Embedding length must match the declared dimension.
	bad := mem(ScopeAgent, "", "x", 1, 0, 0)
	bad.EmbeddingDim = 5
	_, err := s.Remember(ctx, bad)
	assert.ErrorIs(t, err, controlplane.ErrInvalid, "embedding length != dim")
}
