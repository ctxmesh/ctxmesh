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

// Package statelayer is the control-plane state-layer proxy (M51, ADR 0050). Agent
// workloads hold NO Valkey credential and cannot name another tenant's key: this
// proxy holds the credential and enforces tenant/agent/conversation scope
// SERVER-SIDE from the verified OBO run-capability token (internal/runcap).
//
// The memory API shape mirrors the launcher's :2998 endpoint (state-layer.md), but
// the key prefix + attribution come from the VERIFIED TOKEN, never from caller
// input. A compromised launcher can only ever touch keys inside its own
// token-derived prefix. (The launcher's own direct-Valkey memory handlers are
// retired when the credential injection is removed — ADR 0050 §8 phase 3.)
package statelayer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// memoryTTL is the conversation-context lifetime (mirrors the launcher default).
	memoryTTL = 24 * time.Hour
	// memoryOpTimeout bounds every Valkey round-trip — a dead backend fails fast.
	memoryOpTimeout = 2 * time.Second
	// maxConversationEntries caps a conversation's stored entries (ADR 0036, m33.6).
	maxConversationEntries = 500
	// maxConversationID bounds the caller-supplied conversation id.
	maxConversationID = 256
	// messageIDHeader carries a per-hop message id (ADR 0035).
	messageIDHeader = "X-Message-Id"
)

// MemoryStore is the Valkey-backed conversation store (ADR 0035/0036). Same
// contract as the launcher's store; the proxy owns the credentialed client.
type MemoryStore interface {
	Get(ctx context.Context, key string) ([]json.RawMessage, error)
	Replace(ctx context.Context, key string, entries []json.RawMessage, ttl time.Duration) error
	ReplaceIfVersion(
		ctx context.Context, key string, entries []json.RawMessage, expectedVersion int, ttl time.Duration,
	) (newVersion int, conflict bool, err error)
	Append(ctx context.Context, key string, entry json.RawMessage, ttl time.Duration) (int, error)
}

// redisStore is the go-redis MemoryStore. The proxy constructs it with the Valkey
// credential (Password), which agent pods never hold.
type redisStore struct{ rdb *redis.Client }

// NewRedisStore connects (lazily) to Valkey at addr with an optional username/password
// (ADR 0050 §6 — a command-restricted ACL user). Empty password ⇒ unauthenticated
// (dev / a Valkey with no requirepass).
func NewRedisStore(addr, username, password string) MemoryStore {
	return &redisStore{rdb: redis.NewClient(&redis.Options{
		Addr:         addr,
		Username:     username,
		Password:     password,
		DialTimeout:  memoryOpTimeout,
		ReadTimeout:  memoryOpTimeout,
		WriteTimeout: memoryOpTimeout,
	})}
}

func (s *redisStore) Replace(ctx context.Context, key string, entries []json.RawMessage, ttl time.Duration) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	if len(entries) > 0 {
		vals := make([]any, 0, len(entries))
		for _, e := range entries {
			vals = append(vals, string(e))
		}
		pipe.RPush(ctx, key, vals...)
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) Get(ctx context.Context, key string) ([]json.RawMessage, error) {
	vals, err := s.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(vals))
	for _, v := range vals {
		out = append(out, json.RawMessage(v))
	}
	return out, nil
}

// ReplaceIfVersion — optimistic-concurrency replace (ADR 0036). WATCH/EXEC in ONE
// atomic call (ADR 0050 §7 — never split across HTTP requests).
func (s *redisStore) ReplaceIfVersion(
	ctx context.Context, key string, entries []json.RawMessage, expectedVersion int, ttl time.Duration,
) (int, bool, error) {
	var (
		newVersion int
		conflict   bool
	)
	txf := func(tx *redis.Tx) error {
		n, err := tx.LLen(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		if int(n) != expectedVersion {
			conflict = true
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, key)
			if len(entries) > 0 {
				vals := make([]any, 0, len(entries))
				for _, e := range entries {
					vals = append(vals, string(e))
				}
				pipe.RPush(ctx, key, vals...)
				pipe.Expire(ctx, key, ttl)
			}
			return nil
		})
		if err != nil {
			return err
		}
		newVersion = len(entries)
		return nil
	}
	err := s.rdb.Watch(ctx, txf, key)
	if errors.Is(err, redis.TxFailedErr) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return newVersion, conflict, nil
}

func (s *redisStore) Append(ctx context.Context, key string, entry json.RawMessage, ttl time.Duration) (int, error) {
	pipe := s.rdb.TxPipeline()
	pipe.RPush(ctx, key, string(entry))
	pipe.LTrim(ctx, key, -maxConversationEntries, -1)
	pipe.Expire(ctx, key, ttl)
	llen := pipe.LLen(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(llen.Val()), nil
}

// --- token-derived scope (the isolation boundary, ADR 0050 §1) ----------------

// Scope is the server-derived, per-request access scope: the Valkey key PREFIX a
// caller may touch and the authoritative agent name for attribution. Everything
// here comes from the VERIFIED token — never from caller input.
type Scope struct {
	prefix string // e.g. "mem:team-alpha/support-agent:" or "mem:shared:reg-1:"
	agent  string // the agent name (private-scope), for server-authoritative attribution
}

// scopeFromAgent derives the private-memory scope from a capability's Agent claim
// ("<namespace>/<agent>", ADR 0033). requestedShared selects the shared team
// scratchpad, keyed under the token's registry boundary (bnd "r:<registry>"); a
// shared request without a registry boundary falls back to private (a visible
// misconfig, never a cross-tenant leak).
func scopeFromAgent(agent, boundary string, requestedShared bool) (Scope, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" || !strings.Contains(agent, "/") {
		return Scope{}, fmt.Errorf("token agent claim %q is not <namespace>/<agent>", agent)
	}
	i := strings.LastIndex(agent, "/")
	ns, name := agent[:i], agent[i+1:]
	if ns == "" || name == "" {
		return Scope{}, fmt.Errorf("token agent claim %q has an empty namespace or name", agent)
	}
	if requestedShared {
		if reg, ok := strings.CutPrefix(strings.TrimSpace(boundary), "r:"); ok && reg != "" {
			return Scope{prefix: "mem:shared:" + reg + ":", agent: name}, nil
		}
		// shared requested but no registry boundary → private fallback.
	}
	return Scope{prefix: "mem:" + ns + "/" + name + ":", agent: name}, nil
}

// key composes the full Valkey key for a validated conversation id, locked inside
// the server-derived prefix — a compromised caller can never escape it.
func (s Scope) key(convID string) string { return s.prefix + convID }

// --- entry attribution (server-authoritative, ADR 0050 §1 / ADR 0036) ---------

// attributeEntry stamps the agent (from the TOKEN — the caller's `agent` field is
// stripped + overwritten), messageId, and ts onto a message-shaped entry. A
// non-message entry is stored verbatim.
func attributeEntry(entry json.RawMessage, agent, messageID string, now time.Time) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
		return entry
	}
	role, hasRole := obj["role"]
	content, hasContent := obj["content"]
	if !hasRole || !hasContent || !isJSONString(role) || !isJSONString(content) {
		return entry
	}
	// SERVER-AUTHORITATIVE: always overwrite `agent` with the token's agent (ADR 0050
	// §1) — a compromised caller must not attribute entries as a different agent.
	obj["agent"] = mustJSON(agent)
	if _, ok := obj["messageId"]; !ok {
		obj["messageId"] = mustJSON(messageID)
	}
	if _, ok := obj["ts"]; !ok {
		obj["ts"] = mustJSON(now.UnixMilli())
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return entry
	}
	return out
}

func isJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func newMessageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("m-%d", time.Now().UnixNano())
	}
	return "m-" + hex.EncodeToString(b[:])
}

// validateConversationID rejects a conversation id that could break the key layout
// (":" / "/") or routing — the id is opaque but sandboxed inside the token prefix.
func validateConversationID(id string) error {
	if id == "" {
		return errors.New("conversationId is required")
	}
	if len(id) > maxConversationID {
		return fmt.Errorf("conversationId too long (max %d)", maxConversationID)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return errors.New("conversationId contains control characters")
		}
		switch r {
		case '/', ':', ' ', '\t', '\n', '\r':
			return fmt.Errorf("conversationId contains disallowed character %q", r)
		}
	}
	return nil
}
