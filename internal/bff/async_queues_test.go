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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
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

// The receiver becomes a token of the NATS subject, so a CloudEvent type that cannot be an agent's name is
// refused before anything reaches the broker. An oversized one would make the broker close the BFF's
// only connection, after which no hop could be published or delivered until the pod restarted.
func TestAsyncPublish_RefusesAReceiverThatCannotBeAnAgent(t *testing.T) {
	for name, receiver := range map[string]string{
		"past the control line":     strings.Repeat("a", 4100),
		"longer than an agent name": strings.Repeat("a", 45),
		"not a DNS-1123 name":       "Reviewer_1",
		"a subject wildcard":        "reviewer.>",
		"whitespace inside":         "re viewer",
	} {
		t.Run(name, func(t *testing.T) {
			caller := mkParentRun("run-async-bad-" + strings.ReplaceAll(name, " ", "-"))
			s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates work."))
			headers := ceHeaders()
			headers["Ce-Type"] = receiver

			rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), headers, `{"messageId":"msg-1"}`)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Empty(t, bus.published(), "nothing reaches the broker")
		})
	}

	caller := mkParentRun("run-async-longest")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates work."))
	headers := ceHeaders()
	headers["Ce-Type"] = strings.Repeat("a", 44)
	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), headers, `{"messageId":"msg-1"}`)
	assert.Equal(t, http.StatusAccepted, rec.Code, "the longest agent name is accepted")
	assert.Len(t, bus.published(), 1)
}

// A hop to an agent in another registry is refused at the edge: it would never be delivered, and waiting
// in that agent's queue it would count as work, scaling the agent for nothing.
func TestAsyncPublish_RefusesAReceiverInAnotherRegistry(t *testing.T) {
	caller := mkParentRun("run-async-cross")
	s, signer, bus := newAsyncServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Coordinates work."),
		reviewerIn("reg-b"),
	)
	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, bus.published(), "nothing enters the other agent's queue")
}

// The receiver check fails closed: a capability registry that cannot answer refuses the hop rather than
// letting a possible crossing through.
func TestAsyncPublish_ReceiverLookupFailureRefuses(t *testing.T) {
	caller := mkParentRun("run-async-lookup")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates work."))
	s.agentCapabilities = failingFor{Store: s.agentCapabilities, agent: "reviewer"}

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Empty(t, bus.published())
}

// failingFor is a capability registry that cannot read one agent's row.
type failingFor struct {
	agentcapability.Store
	agent string
}

func (f failingFor) Get(ctx context.Context, namespace, agent string) (agentcapability.AgentCapability, bool, error) {
	if agent == f.agent {
		return agentcapability.AgentCapability{}, false, errors.New("connection refused")
	}
	return f.Store.Get(ctx, namespace, agent)
}

// closingBus is an async backend whose connection can be closed for good.
type closingBus struct {
	recordingBus
	err error
}

func (b *closingBus) Err() error { return b.err }

// The BFF's liveness probe fails once the async backend's connection is closed for good, so the kubelet
// restarts a pod that could otherwise neither publish nor deliver another hop.
func TestHealth_FailsWhenTheAsyncConnectionIsClosed(t *testing.T) {
	bus := &closingBus{}
	s := &Server{asyncPublisher: bus, version: "test", log: logr.Discard()}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "healthy while the connection is usable")

	bus.err = asyncbus.ErrConnectionClosed
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "async backend connection closed")
}

// The dispatcher outlasts the receiving launcher, which bounds processing a delivery at 90s plus a few
// seconds of admission and dedupe, and gives up before the broker's ack wait, so a hop is never
// redelivered while it is still being delivered.
func TestAsyncDispatchTimeout_SitsBetweenTheLauncherAndTheAckWait(t *testing.T) {
	const launcherProcessTimeout = 90 * time.Second // cmd/launcher asyncProcessTimeout
	assert.Greater(t, asyncDispatchTimeout, launcherProcessTimeout+5*time.Second)
	assert.Less(t, asyncDispatchTimeout, asyncbus.AckWait)
}
