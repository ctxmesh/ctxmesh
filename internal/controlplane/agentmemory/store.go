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

// Package agentmemory is the control-plane store for `agent`/long-term agent memory (ADR 0045, M46). Unlike
// session/shared memory (Valkey LISTs keyed by conversation, ADR 0036), agent memory PERSISTS across
// conversations and is retrieved BY MEANING, so it lives in Postgres + pgvector.
//
// Two load-bearing invariants are enforced by the store shape, not by callers:
//   - Per-user isolation: a memory is keyed (Namespace, AgentName, Scope, Subject). Subject "" is agent-wide;
//     a user_id is per-user. A user's remembered facts must never surface in another user's retrieval.
//   - Embedding-model provenance: every row carries EmbeddingModel + EmbeddingDim, and every Search filters by
//     EmbeddingModel — comparing vectors across models yields SILENTLY WRONG similarity (plausible scores,
//     garbage results). A model swap is a background re-embed, never a live DDL.
//
// Two implementations — Postgres (pgstore, pgvector) + an in-memory twin (memstore, cosine in Go) — pass one
// conformance suite (the internal/controlplane/toolregistry pattern).
package agentmemory

import (
	"context"
	"time"
)

// Scope values for agent-scope memory. Session/shared live in Valkey (ADR 0036); these are the Postgres scopes.
const (
	// ScopeAgent is agent-wide long-term memory (Subject == ""), shared across every user of the agent.
	ScopeAgent = "agent"
	// ScopeAgentUser is per-user long-term memory (Subject == user id), isolated to one user of the agent.
	ScopeAgentUser = "agent_user"
)

// TopK bounds (hardening, M46 close). An unset/non-positive TopK defaults to defaultTopK; any request is capped
// at maxTopK so a caller can never demand an unbounded LIMIT (an oversized response + a degraded HNSW scan).
// Both stores route TopK through resolveTopK, so the twin behaviour agrees and the cap holds at the store
// boundary — the right layer, since the Store owns the "TopK caps the results" contract.
const (
	defaultTopK = 10
	maxTopK     = 100
)

// resolveTopK normalizes a requested TopK: ≤0 → defaultTopK, > maxTopK → maxTopK, else the request.
func resolveTopK(k int) int {
	switch {
	case k <= 0:
		return defaultTopK
	case k > maxTopK:
		return maxTopK
	default:
		return k
	}
}

// AgentMemory is one stored long-term memory. ID is assigned by the store on Remember. Embedding is the raw
// vector (its length MUST equal EmbeddingDim, which MUST match the store's configured vector dimension).
type AgentMemory struct {
	ID             string
	Namespace      string
	AgentName      string
	Scope          string // ScopeAgent | ScopeAgentUser
	Subject        string // "" = agent-wide; user id = per-user
	Content        string
	Tags           map[string]string
	EmbeddingModel string
	EmbeddingDim   int
	Embedding      []float32
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SearchQuery is a semantic retrieval over one partition. Namespace/AgentName/Scope/Subject/EmbeddingModel
// scope the search (never a bare vector scan); Vector is the query embedding; TopK caps the results; Threshold
// is the minimum cosine similarity in [0,1] (0 = no floor).
type SearchQuery struct {
	Namespace      string
	AgentName      string
	Scope          string
	Subject        string
	EmbeddingModel string
	Vector         []float32
	TopK           int
	Threshold      float32
}

// ScoredMemory pairs a retrieved memory with its cosine similarity in [0,1] (1 = identical direction).
type ScoredMemory struct {
	Memory AgentMemory
	Score  float32
}

// ListOptions filters the console list of an agent's memories. Empty Subject lists agent-wide rows only;
// AllSubjects lists every subject (the admin/agent-owner view). Newest-first.
type ListOptions struct {
	Namespace   string
	AgentName   string
	Scope       string
	Subject     string
	AllSubjects bool
	Limit       int
}

// Store is the control-plane repository for agent-scope memory. Entity-specific (the per-entity store pattern),
// but unlike the catalog stores it is a vector store: Remember writes, Search retrieves by similarity.
type Store interface {
	// Remember inserts a memory, or updates in place when the same content already exists in the partition
	// (idempotent re-remember, keyed by content hash). Returns the stored record with its ID + timestamps.
	Remember(ctx context.Context, m AgentMemory) (*AgentMemory, error)
	// Search returns up to TopK memories in the query's partition ordered by descending cosine similarity,
	// dropping any below Threshold. Only rows whose EmbeddingModel matches the query are considered.
	Search(ctx context.Context, q SearchQuery) ([]ScoredMemory, error)
	// List returns an agent's memories for the console (newest-first, no vector), scoped per ListOptions.
	List(ctx context.Context, opts ListOptions) ([]AgentMemory, error)
	// Delete removes one memory by ID within its (namespace, agentName) agent. Absent ID → ErrNotFound.
	Delete(ctx context.Context, namespace, agentName, id string) error
	// Count returns the number of memories in a partition — for the optional max-memories LRU cap.
	Count(ctx context.Context, namespace, agentName, scope, subject string) (int, error)
}
