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

package budget

import (
	"sync"
	"time"
)

// spendTTL is how long a dimension's accumulated spend survives without a new
// call before it is evicted (specs/cost-governance.md: "TTL-evicted (24h; a
// conversation ages out)"). A conversation that goes quiet for a day starts
// fresh — matching the M5 memory TTL so a budget outlives the state it guards.
const spendTTL = 24 * time.Hour

// spendEntry is one accumulated-spend record: the exact total plus the last time
// it was touched (for TTL eviction). softFired latches the one-shot soft alert so
// it fires exactly once per dimension per lifetime.
type spendEntry struct {
	total     Money
	softFired bool
	lastSeen  time.Time
}

// Accountant holds the in-memory, thread-safe spend maps for both dimensions:
// per conversation id and per agent name (specs/cost-governance.md "Accounting").
// It is the v1 single-replica store — a gateway/launcher restart resets it (a
// mid-flight conversation gets a fresh budget); durable multi-replica spend
// (Valkey) is phase 2.
//
// Concurrency: every method locks the single mutex. Concurrent calls in one
// conversation may race the threshold by one call (accepted per the spec — the
// cap is a soft-real ceiling, not a transactional limit in v1), but there is no
// data race: the maps and every Money read/write happen under the lock.
type Accountant struct {
	mu    sync.Mutex
	conv  map[string]*spendEntry
	agent map[string]*spendEntry

	// now is injectable so TTL eviction and lastSeen are testable without wall
	// clock. Defaults to time.Now.
	now func() time.Time
}

// NewAccountant returns an empty Accountant ready for concurrent use.
func NewAccountant() *Accountant {
	return &Accountant{
		conv:  make(map[string]*spendEntry),
		agent: make(map[string]*spendEntry),
		now:   time.Now,
	}
}

// spentLocked returns the current spend for key in m, evicting a stale entry
// first. Caller holds a.mu. A missing/evicted key reads as $0.
func (a *Accountant) spentLocked(m map[string]*spendEntry, key string) Money {
	e := a.liveEntryLocked(m, key)
	if e == nil {
		return Zero()
	}
	return e.total
}

// liveEntryLocked returns the entry for key if present and not expired, else nil
// (evicting an expired one). Caller holds a.mu.
func (a *Accountant) liveEntryLocked(m map[string]*spendEntry, key string) *spendEntry {
	e, ok := m[key]
	if !ok {
		return nil
	}
	if a.now().Sub(e.lastSeen) > spendTTL {
		delete(m, key)
		return nil
	}
	return e
}

// ConvSpent returns the current accumulated spend for a conversation id.
func (a *Accountant) ConvSpent(conversationID string) Money {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spentLocked(a.conv, conversationID)
}

// AgentSpent returns the current accumulated spend for an agent name.
func (a *Accountant) AgentSpent(agentName string) Money {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spentLocked(a.agent, agentName)
}

// addLocked adds cost to key in m, creating the entry if absent, and returns the
// new total. Caller holds a.mu.
func (a *Accountant) addLocked(m map[string]*spendEntry, key string, cost Money) Money {
	e := a.liveEntryLocked(m, key)
	if e == nil {
		e = &spendEntry{total: Zero()}
		m[key] = e
	}
	e.total = e.total.Add(cost)
	e.lastSeen = a.now()
	return e.total
}

// Add records a completed call's ACTUAL cost against both dimensions. A dimension
// with an empty key is skipped (no conversation id ⇒ no per-conversation
// accounting). Returns the new per-conversation and per-agent totals.
func (a *Accountant) Add(conversationID, agentName string, cost Money) (convTotal, agentTotal Money) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if conversationID != "" {
		convTotal = a.addLocked(a.conv, conversationID, cost)
	} else {
		convTotal = Zero()
	}
	if agentName != "" {
		agentTotal = a.addLocked(a.agent, agentName, cost)
	} else {
		agentTotal = Zero()
	}
	return convTotal, agentTotal
}

// markSoftFiredLocked latches the one-shot soft alert for key in m and reports
// whether THIS call is the one that latched it (i.e. it was not already fired).
// Caller holds a.mu. A missing entry is created so the latch survives even if the
// soft crossing happens before any cost is added (defensive; in practice Add runs
// first).
func (a *Accountant) markSoftFiredLocked(m map[string]*spendEntry, key string) bool {
	e := a.liveEntryLocked(m, key)
	if e == nil {
		e = &spendEntry{total: Zero(), lastSeen: a.now()}
		m[key] = e
	}
	if e.softFired {
		return false
	}
	e.softFired = true
	return true
}

// MarkConvSoftFired latches the conversation-dimension soft alert once, returning
// true only on the transition. An empty key returns false (nothing to track).
func (a *Accountant) MarkConvSoftFired(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.markSoftFiredLocked(a.conv, conversationID)
}

// MarkAgentSoftFired latches the agent-dimension soft alert once, returning true
// only on the transition. An empty key returns false (nothing to track).
func (a *Accountant) MarkAgentSoftFired(agentName string) bool {
	if agentName == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.markSoftFiredLocked(a.agent, agentName)
}
