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

package asyncbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/asyncbus"
)

// startEmbeddedJetStream runs a real nats-server with JetStream on a FILE store under t.TempDir().
//
// It is the real broker, not a double: the whole claim under test is DURABILITY, and a fake would prove
// only that the fake remembers things. Embedding it keeps the proof hermetic — no Docker, no cluster, so
// this runs in tier0 on any machine.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // an ephemeral port, so parallel tests never collide
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(15*time.Second), "the embedded NATS server must come up")
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

func newBus(t *testing.T, url string) *asyncbus.JetStreamBus {
	t.Helper()
	bus, err := asyncbus.NewJetStream(context.Background(), asyncbus.JetStreamOptions{URL: url})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func hop(id, subject, body string) asyncbus.Message {
	return asyncbus.Message{
		ID: id, Subject: subject, Data: []byte(body),
		Headers: map[string]string{"ce-type": "ai.ctxmesh.a2a.async", "ce-id": id},
	}
}

// collector records delivered messages and signals when it has enough.
type collector struct {
	mu    sync.Mutex
	got   []asyncbus.Message
	want  int
	done  chan struct{}
	once  sync.Once
	fail  error // when set, the handler nacks
	tries int   // handler invocations, refused ones included — how much delivery budget was spent
}

func newCollector(want int) *collector {
	return &collector{want: want, done: make(chan struct{})}
}

func (c *collector) handle(_ context.Context, msg asyncbus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tries++
	if c.fail != nil {
		return c.fail
	}
	c.got = append(c.got, msg)
	if len(c.got) >= c.want {
		c.once.Do(func() { close(c.done) })
	}
	return nil
}

func (c *collector) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tries
}

func (c *collector) messages() []asyncbus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]asyncbus.Message(nil), c.got...)
}

// consume runs a subscriber until the collector is satisfied or the deadline passes, then stops it —
// modelling one consumer LIFETIME so a test can end one and start another.
func consume(t *testing.T, bus *asyncbus.JetStreamBus, subject, durable string, c *collector, wait time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- bus.Subscribe(ctx, subject, durable, c.handle) }()

	var satisfied bool
	select {
	case <-c.done:
		satisfied = true
	case <-time.After(wait):
	}
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Subscribe did not return after its context was cancelled")
	}
	return satisfied
}

// A published message is durably accepted and then consumed, verbatim.
func TestJetStream_PublishAndConsume(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	subject := asyncbus.Subject("reg-a")

	require.NoError(t, bus.Publish(context.Background(), hop("msg-1", subject, `{"payload":"hello"}`)))

	c := newCollector(1)
	require.True(t, consume(t, bus, subject, "dispatcher-reg-a", c, 20*time.Second), "the message must arrive")

	got := c.messages()
	require.Len(t, got, 1)
	assert.Equal(t, "msg-1", got[0].ID, "the messageId survives the hop — it is the idempotency key")
	assert.Equal(t, `{"payload":"hello"}`, string(got[0].Data), "the body is moved verbatim")
	assert.Equal(t, "ai.ctxmesh.a2a.async", got[0].Headers["ce-type"],
		"the CloudEvent binding headers are moved verbatim — the seam never rewrites the wire format")
}

// THE BAR (M141 🧪): a message published while NOBODY is listening survives, and a consumer that starts
// later — a restart, under the same durable name — still receives it. This is what separates a durable
// backend from a fan-out bus, and it is the reason the async hop can be trusted at all.
func TestJetStream_SurvivesAConsumerRestart(t *testing.T) {
	url := startEmbeddedJetStream(t)
	bus := newBus(t, url)
	subject := asyncbus.Subject("reg-restart")
	const durable = "dispatcher-reg-restart"

	// (1) A consumer exists, then goes away with nothing outstanding.
	warmup := newCollector(1)
	require.NoError(t, bus.Publish(context.Background(), hop("before-1", subject, `{"n":1}`)))
	require.True(t, consume(t, bus, subject, durable, warmup, 20*time.Second))

	// (2) Publish while NO consumer is running at all.
	for _, id := range []string{"offline-1", "offline-2"} {
		require.NoError(t, bus.Publish(context.Background(), hop(id, subject, `{"n":2}`)))
	}

	// (3) A NEW consumer process — a fresh connection, the same durable name — picks up what it missed.
	restarted := newBus(t, url)
	c := newCollector(2)
	require.True(t, consume(t, restarted, subject, durable, c, 30*time.Second),
		"messages published while the consumer was down must be delivered when it returns")

	delivered := c.messages()
	ids := make([]string, 0, len(delivered))
	for _, m := range delivered {
		ids = append(ids, m.ID)
	}
	assert.ElementsMatch(t, []string{"offline-1", "offline-2"}, ids)
	assert.NotContains(t, ids, "before-1", "an already-acked message is not redelivered to the same durable")
}

// A REGRESSION GUARD for the retention choice (ADR 0121): a message published to a subject whose
// consumer does not exist YET must still be delivered when that consumer first appears.
//
// This is not hypothetical. The first cut of this backend used INTEREST retention, which drops a message
// that lands with no registered consumer — so an agent publishing an async hop moments before its
// dispatcher first came up would have had the hop silently discarded, which is precisely the loss the
// seam promises cannot happen. Every consuming test above failed against that config, and this test
// states the property directly so the choice cannot quietly regress.
func TestJetStream_MessagePublishedBeforeAnyConsumerExistsSurvives(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	subject := asyncbus.Subject("reg-cold")

	// Nobody has ever subscribed to this subject — no consumer exists on the stream at all.
	require.NoError(t, bus.Publish(context.Background(), hop("cold-start", subject, `{"n":1}`)))

	c := newCollector(1)
	require.True(t, consume(t, bus, subject, "dispatcher-reg-cold", c, 30*time.Second),
		"a hop published before its dispatcher first started must NOT be discarded")
	require.Len(t, c.messages(), 1)
	assert.Equal(t, "cold-start", c.messages()[0].ID)
}

// A handler that fails NACKs, and the message comes back — at-least-once, not at-most-once.
//
// This also pins the REDELIVERY BACKOFF (ADR 0121). The first cut nacked immediately, which looks
// harmless until you count: a downstream agent that is merely cold-starting nacks, is redelivered within
// milliseconds, nacks again — and burns the entire MaxDeliver budget in well under a second, turning a
// transient unavailability into a permanently dropped hop. The refusing consumer below runs for three
// seconds; with immediate redelivery that is enough to exhaust every attempt, and the message is gone by
// the time an accepting consumer arrives. With a backoff it is not.
func TestJetStream_NackRedeliversAfterABackoff(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	subject := asyncbus.Subject("reg-nack")
	const durable = "dispatcher-reg-nack"

	require.NoError(t, bus.Publish(context.Background(), hop("retry-me", subject, `{"n":1}`)))

	// First lifetime: the handler refuses for long enough that an un-backed-off retry loop would have
	// spent all five delivery attempts.
	refusing := newCollector(1)
	refusing.fail = errors.New("downstream agent unreachable")
	consume(t, bus, subject, durable, refusing, 3*time.Second)
	assert.Empty(t, refusing.messages(), "a nacked message is not recorded as handled")
	assert.LessOrEqual(t, refusing.attempts(), 2,
		"three seconds of refusal must not burn the delivery budget — redelivery is backed off, not immediate")

	// Second lifetime: it accepts, and the message is still there to accept.
	accepting := newCollector(1)
	require.True(t, consume(t, bus, subject, durable, accepting, 60*time.Second),
		"a nacked message must be redelivered, not dropped")
	require.Len(t, accepting.messages(), 1)
	assert.Equal(t, "retry-me", accepting.messages()[0].ID)
}

// Subjects isolate registries: a consumer bound to one registry never sees another's traffic. This is the
// same boundary the per-registry Knative Broker draws, preserved across the backend swap.
func TestJetStream_SubjectsIsolateRegistries(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	mine, theirs := asyncbus.Subject("reg-mine"), asyncbus.Subject("reg-theirs")

	require.NoError(t, bus.Publish(context.Background(), hop("theirs-1", theirs, `{}`)))
	require.NoError(t, bus.Publish(context.Background(), hop("mine-1", mine, `{}`)))

	c := newCollector(1)
	require.True(t, consume(t, bus, mine, "dispatcher-reg-mine", c, 20*time.Second))
	require.Len(t, c.messages(), 1)
	assert.Equal(t, "mine-1", c.messages()[0].ID, "only this registry's traffic reaches this consumer")
}

// A registry id can never widen a subscription: anything outside a subject token is neutralised, so a
// crafted id cannot smuggle a wildcard into the routing key.
func TestSubject_CannotEscapeItsToken(t *testing.T) {
	assert.Equal(t, "agentry.a2a.reg-a", asyncbus.Subject("reg-a"))
	assert.NotContains(t, asyncbus.Subject("reg.a"), "reg.a", "a dot cannot split the token")
	assert.NotContains(t, asyncbus.Subject("*"), "*", "a wildcard cannot widen the subscription")
	assert.NotContains(t, asyncbus.Subject(">"), ">", "a full wildcard cannot widen the subscription")
}

// A publish with no subject is refused rather than landing somewhere unrouted.
func TestJetStream_PublishRequiresASubject(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	require.Error(t, bus.Publish(context.Background(), asyncbus.Message{ID: "x", Data: []byte("{}")}))
}
