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

package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// conversation.go — the conversation → active-agent pointer (M67, ADR 0060 §5). Handoff is a
// CONVERSATION primitive: when agent A hands the conversation off to roster member B (`handoff_to`),
// A's run TERMINATES and B becomes the conversation's ACTIVE AGENT — so the user's NEXT turn on the
// same conversationId routes to B (a NEW ROOT run for B), not back to A. The run's `agent` field is
// IMMUTABLE (it is the audit record); the mutable "who is the conversation talking to now" lives HERE,
// deliberately separate from the runs table (colocating a mutable agent column next to the never-mutate
// run.Agent invites exactly the bug that invariant forbids — Fable, 2026-08-09).
//
// This is the minimal correct shape: one row per conversation. It is a proto-conversation object; a
// full conversation store (owner, title, created_at) can fold into it later without a rename.

// ActiveAgent is the conversation's current active-agent pointer (ADR 0060 §5). Namespace + Agent name
// the roster member the conversation is now talking to; SourceRunID is A's run — the handoff that set
// this pointer — so B's next run can record the backlink (B has no ParentRunID by design; this closes
// the handoff lineage loop). UpdatedAt bounds when the pointer last moved.
type ActiveAgent struct {
	Namespace   string
	Agent       string
	SourceRunID string
	UpdatedAt   time.Time
}

// ErrNoActiveAgent is returned by ConversationStore.GetActiveAgent when a conversation has no
// active-agent pointer (it never handed off) — the run-create routing then uses the caller's explicit
// agent, unchanged. It is not an error condition; the caller checks for it and falls back.
var ErrNoActiveAgent = errors.New("run: conversation has no active agent")

// ConversationStore persists the conversation → active-agent pointer (ADR 0060 §5). It slots behind a
// seam exactly like the run Store: a hot in-memory twin for dev/single-pod and a durable Postgres
// backing when a run-store DSN is configured, so a handoff survives a BFF restart and the next user
// turn (which may land on any pod) routes to the active agent.
type ConversationStore interface {
	// SetActiveAgent upserts the conversation's active-agent pointer to (namespace, agent), recording
	// sourceRunID (the handing-off run A) + the update time. Idempotent under retry — a re-issued
	// handoff sets the same pointer. conversationID must be non-empty (a single-shot run with no
	// conversation cannot hand off — there is no thread to transfer).
	SetActiveAgent(conversationID, namespace, agent, sourceRunID string) error
	// GetActiveAgent returns the conversation's active-agent pointer, or ErrNoActiveAgent when none
	// is set. Consumed at run-create: a conversation with a pointer AND no explicit agent override
	// routes to the active agent (B).
	GetActiveAgent(conversationID string) (ActiveAgent, error)
}

// convSchemaDDL creates the active-agent pointer table (applied idempotently at open, matching the run
// store). One row per conversation; the run store and this share one Postgres so an operator runs one
// datastore. No FK to runs(id): source_run_id is an audit backlink, not a lifecycle dependency (the
// pointer outlives A's run row retention).
const convSchemaDDL = `
CREATE TABLE IF NOT EXISTS conversation_active_agent (
    conversation_id text PRIMARY KEY,
    namespace       text NOT NULL DEFAULT '',
    agent           text NOT NULL DEFAULT '',
    source_run_id   text NOT NULL DEFAULT '',
    updated_at      timestamptz NOT NULL
);`

// pgConversationStore is the durable Postgres-backed active-agent pointer store.
type pgConversationStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewPostgresConversationStore opens the durable conversation active-agent store over an open *sql.DB
// (the SAME handle the run store uses — one Postgres), applying its schema idempotently.
func NewPostgresConversationStore(ctx context.Context, db *sql.DB) (ConversationStore, error) {
	if _, err := db.ExecContext(ctx, convSchemaDDL); err != nil {
		return nil, fmt.Errorf("conversation: apply schema: %w", err)
	}
	return &pgConversationStore{db: db, now: time.Now}, nil
}

func (p *pgConversationStore) SetActiveAgent(conversationID, namespace, agent, sourceRunID string) error {
	if conversationID == "" {
		return errors.New("conversation: SetActiveAgent requires a conversation id")
	}
	const q = `INSERT INTO conversation_active_agent (conversation_id, namespace, agent, source_run_id, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (conversation_id) DO UPDATE SET
			namespace = EXCLUDED.namespace,
			agent = EXCLUDED.agent,
			source_run_id = EXCLUDED.source_run_id,
			updated_at = EXCLUDED.updated_at`
	if _, err := p.db.ExecContext(context.Background(), q,
		conversationID, namespace, agent, sourceRunID, p.now().UTC()); err != nil {
		return fmt.Errorf("conversation: set active agent: %w", err)
	}
	return nil
}

func (p *pgConversationStore) GetActiveAgent(conversationID string) (ActiveAgent, error) {
	const q = `SELECT namespace, agent, source_run_id, updated_at
		FROM conversation_active_agent WHERE conversation_id = $1`
	var (
		a       ActiveAgent
		updated time.Time
	)
	err := p.db.QueryRowContext(context.Background(), q, conversationID).
		Scan(&a.Namespace, &a.Agent, &a.SourceRunID, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ActiveAgent{}, ErrNoActiveAgent
	case err != nil:
		return ActiveAgent{}, fmt.Errorf("conversation: get active agent: %w", err)
	}
	a.UpdatedAt = updated.UTC()
	return a, nil
}

// memConversationStore is the hot in-memory active-agent pointer store (dev/single-pod). Safe for
// concurrent use.
type memConversationStore struct {
	mu       sync.Mutex
	pointers map[string]ActiveAgent
}

// NewMemConversationStore returns a hot in-memory conversation active-agent store.
func NewMemConversationStore() ConversationStore {
	return &memConversationStore{pointers: map[string]ActiveAgent{}}
}

func (m *memConversationStore) SetActiveAgent(conversationID, namespace, agent, sourceRunID string) error {
	if conversationID == "" {
		return errors.New("conversation: SetActiveAgent requires a conversation id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pointers[conversationID] = ActiveAgent{
		Namespace:   namespace,
		Agent:       agent,
		SourceRunID: sourceRunID,
		UpdatedAt:   time.Now().UTC(),
	}
	return nil
}

func (m *memConversationStore) GetActiveAgent(conversationID string) (ActiveAgent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.pointers[conversationID]
	if !ok {
		return ActiveAgent{}, ErrNoActiveAgent
	}
	return a, nil
}
