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

package bff

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
)

// queueRecorder is a Subscriber that records which durables are being consumed, under which filter.
type queueRecorder struct {
	mu     sync.Mutex
	active map[string]string // durable → filter
	starts map[string]int
	fail   map[string]int // durable → how many more Subscribe calls fail at once
}

func newQueueRecorder() *queueRecorder {
	return &queueRecorder{active: map[string]string{}, starts: map[string]int{}, fail: map[string]int{}}
}

func (q *queueRecorder) Subscribe(ctx context.Context, subject, durable string, _ asyncbus.Handler) error {
	q.mu.Lock()
	q.starts[durable]++
	if q.fail[durable] > 0 {
		q.fail[durable]--
		q.mu.Unlock()
		return errors.New("broker unreachable")
	}
	q.active[durable] = subject
	q.mu.Unlock()
	<-ctx.Done()
	q.mu.Lock()
	delete(q.active, durable)
	q.mu.Unlock()
	return nil
}

func (q *queueRecorder) Close() error { return nil }

func (q *queueRecorder) consuming() map[string]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return maps.Clone(q.active)
}

func (q *queueRecorder) startsOf(durable string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.starts[durable]
}

// The dispatcher keeps one queue per registry member, so each queue's pending count is the hops waiting
// for one agent, and follows the capability registry as agents join and leave. The registry-only durable
// keeps draining hops published before they were addressed to an agent.
func TestAsyncDispatcher_KeepsAQueuePerRegistryMember(t *testing.T) {
	s, _, _ := newAsyncServer(t, nil,
		cap0("team-ns", "planner", "reg-a", ""),
		reviewerIn("reg-a"),
		cap0("team-ns", "loner", "", "Belongs to no registry."),
	)
	sub := newQueueRecorder()
	s.StartAsyncDispatcher(t.Context(), AsyncDispatcherConfig{Subscriber: sub, MemberResync: 20 * time.Millisecond})

	require.Eventually(t, func() bool { return len(sub.consuming()) == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, map[string]string{
		asyncDurableName: jsname.RegistrySubjects,
		jsname.AgentDurable("team-ns", "planner"):  jsname.AgentFilter("team-ns", "planner"),
		jsname.AgentDurable("team-ns", "reviewer"): jsname.AgentFilter("team-ns", "reviewer"),
	}, sub.consuming(), "a queue per member; an agent in no registry is sent no hops")

	require.NoError(t, s.agentCapabilities.Delete(t.Context(), "team-ns", "planner"))
	require.NoError(t, s.agentCapabilities.Set(t.Context(), cap0("other-ns", "writer", "reg-b", "")))
	require.Eventually(t, func() bool {
		got := sub.consuming()
		_, planner := got[jsname.AgentDurable("team-ns", "planner")]
		_, writer := got[jsname.AgentDurable("other-ns", "writer")]
		return !planner && writer
	}, 5*time.Second, 10*time.Millisecond, "a member that left stops being consumed, and a new one starts")
	assert.Equal(t, 1, sub.startsOf(jsname.AgentDurable("team-ns", "reviewer")),
		"a queue that is still wanted is not restarted on every resync")
}

// A queue the backend refuses is retried rather than left down: while it is down its hops wait undelivered.
func TestAsyncDispatcher_RetriesAQueueTheBackendRefused(t *testing.T) {
	s, _, _ := newAsyncServer(t, nil, reviewerIn("reg-a"))
	var logged []string
	var mu sync.Mutex
	s.log = funcr.New(func(_, args string) { mu.Lock(); logged = append(logged, args); mu.Unlock() }, funcr.Options{})
	sub := newQueueRecorder()
	durable := jsname.AgentDurable("team-ns", "reviewer")
	sub.fail[durable] = 2
	s.StartAsyncDispatcher(t.Context(), AsyncDispatcherConfig{Subscriber: sub, MemberResync: 20 * time.Millisecond})

	require.Eventually(t, func() bool { _, ok := sub.consuming()[durable]; return ok }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 3, sub.startsOf(durable))
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, strings.Join(logged, "\n"), "queuing undelivered", "each refusal is logged")
}

// A hop to an agent that belongs to no registry is accepted, since the agent may be about to join one,
// and logged, because no queue delivers it until then.
func TestAsyncPublish_LogsAReceiverInNoRegistry(t *testing.T) {
	caller := mkParentRun("run-async-orphan")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates work."))
	var logged []string
	s.log = funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{})

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Len(t, bus.published(), 1)
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0], "belongs to no registry yet")
	assert.Contains(t, logged[0], "team-ns/reviewer")

	logged = nil
	require.NoError(t, s.agentCapabilities.Set(t.Context(), reviewerIn("reg-a")))
	rec = postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-2"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, logged, "a hop to a member is not remarked on")
}
