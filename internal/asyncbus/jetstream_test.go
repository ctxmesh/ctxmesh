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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
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
		// A bare "Should be true" from the caller says nothing about WHY, which is how
		// a delivery failure in CI reads as "slow infrastructure". Say what arrived.
		got := c.messages()
		ids := make([]string, 0, len(got))
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		t.Logf("consume(%s/%s) timed out after %s: wanted %d, got %d %v (handler called %d times)",
			subject, durable, wait, c.want, len(got), ids, c.attempts())
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
	assert.Equal(t, "ctxmesh.a2a.reg-a", asyncbus.Subject("reg-a"))
	assert.NotContains(t, asyncbus.Subject("reg.a"), "reg.a", "a dot cannot split the token")
	assert.NotContains(t, asyncbus.Subject("*"), "*", "a wildcard cannot widen the subscription")
	assert.NotContains(t, asyncbus.Subject(">"), ">", "a full wildcard cannot widen the subscription")
}

// A publish with no subject is refused rather than landing somewhere unrouted.
func TestJetStream_PublishRequiresASubject(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	require.Error(t, bus.Publish(context.Background(), asyncbus.Message{ID: "x", Data: []byte("{}")}))
}

// TestSubscribe_LeavesNothingUnacked pins the precondition the consumer-restart test
// states in prose and did not actually establish.
//
// consume() returns when the HANDLER has what it wants, and the handler runs BEFORE
// m.Ack(). Teardown was `defer consumeCtx.Stop()`, and Stop is asynchronous — so
// Subscribe could return with the ack still in flight, leaving the message
// outstanding for the full AckWait (two minutes). The restart test then began its
// "publish while no consumer is running" phase against a durable that still had an
// un-acked message, which is not the state it says it is testing.
//
// This asserts the state directly from the server rather than by timing.
func TestSubscribe_LeavesNothingUnacked(t *testing.T) {
	url := startEmbeddedJetStream(t)
	bus := newBus(t, url)
	subject := asyncbus.Subject("reg-acked")
	const durable = "dispatcher-reg-acked"

	c := newCollector(1)
	require.NoError(t, bus.Publish(context.Background(), hop("only-1", subject, `{"n":1}`)))
	require.True(t, consume(t, bus, subject, durable, c, 20*time.Second))

	// Ask the server, not the clock.
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	cons, err := js.Consumer(context.Background(), "CTXMESH_A2A", durable)
	require.NoError(t, err)
	// Poll rather than read once. The contract is AT-LEAST-ONCE (asyncbus.go), so the consume loop
	// acks fire-and-forget: `m.Ack()` hands the ack to the client and returns, and a redelivery
	// caused by an ack still in flight is explicitly permitted — consumers dedupe. Reading
	// NumAckPending the instant Subscribe returns therefore asserted a SYNCHRONOUS ack, which the
	// bus has never promised and does not implement. It held on a fast machine and failed on a
	// loaded CI runner, which is the signature of a test asserting a race rather than a rule.
	//
	// This is not a weaker bar, it is the right one: the defect worth catching is a message left
	// UNACKED — redelivered forever, the consumer stuck — not an ack that lands a millisecond
	// after the call returns. A genuinely unacked message never drains, so the poll still fails.
	require.Eventually(t, func() bool {
		info, iErr := cons.Info(context.Background())
		return iErr == nil && info.NumAckPending == 0
	}, 10*time.Second, 50*time.Millisecond,
		"the message was never acked — Subscribe left it outstanding, so it will redeliver forever")
}

// consumerInfo reads a durable's state from the server, as KEDA's nats-jetstream scaler does.
func consumerInfo(t *testing.T, url, durable string) (*jetstream.ConsumerInfo, error) {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	cons, err := js.Consumer(context.Background(), "CTXMESH_A2A", durable)
	if err != nil {
		return nil, err
	}
	return cons.Info(context.Background())
}

// lag is what KEDA's nats-jetstream scaler scales on: the hops a durable has not had acknowledged,
// delivered or not.
func lag(t *testing.T, url, durable string) int {
	t.Helper()
	info, err := consumerInfo(t, url, durable)
	require.NoError(t, err)
	return int(info.NumPending) + info.NumAckPending
}

// ensureDurable starts a consumer and stops it once the server holds its durable, as the dispatcher does
// for every registry member before any hop is sent to it.
func ensureDurable(t *testing.T, bus *asyncbus.JetStreamBus, url, subject, durable string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(ctx, subject, durable, func(context.Context, asyncbus.Message) error { return nil })
	}()
	require.Eventually(t, func() bool { _, err := consumerInfo(t, url, durable); return err == nil },
		10*time.Second, 20*time.Millisecond, "the durable %s was never created", durable)
	cancel()
	require.NoError(t, <-done)
}

// An agent's durable counts the hops waiting for that agent and for no other: the number a queue-depth
// AgentScalingPolicy scales the agent on. A hop that was refused stays counted until it is delivered, so
// an agent scaled to zero, whose hops fail to deliver, is still seen to have work waiting.
func TestJetStream_AnAgentsQueueCountsOnlyItsOwnHops(t *testing.T) {
	url := startEmbeddedJetStream(t)
	bus := newBus(t, url)
	worker, other := jsname.AgentDurable("team-ns", "worker"), jsname.AgentDurable("team-ns", "other")
	ensureDurable(t, bus, url, jsname.AgentFilter("team-ns", "worker"), worker)
	ensureDurable(t, bus, url, jsname.AgentFilter("team-ns", "other"), other)
	require.Zero(t, lag(t, url, worker), "nothing is waiting yet")

	for _, id := range []string{"w-1", "w-2", "w-3"} {
		require.NoError(t, bus.Publish(context.Background(), hop(id, jsname.AgentSubject("reg-a", "team-ns", "worker"), `{}`)))
	}
	for _, id := range []string{"o-1", "o-2"} {
		require.NoError(t, bus.Publish(context.Background(), hop(id, jsname.AgentSubject("reg-a", "team-ns", "other"), `{}`)))
	}
	// The same agent name in another namespace is another agent.
	require.NoError(t, bus.Publish(context.Background(), hop("x-1", jsname.AgentSubject("reg-a", "elsewhere", "worker"), `{}`)))

	require.Eventually(t, func() bool { return lag(t, url, worker) == 3 }, 10*time.Second, 50*time.Millisecond,
		"the worker's queue holds the worker's hops")
	assert.Equal(t, 2, lag(t, url, other), "and the other agent's queue only its own")

	refusing := newCollector(3)
	refusing.fail = errors.New("no replica is running")
	consume(t, bus, jsname.AgentFilter("team-ns", "worker"), worker, refusing, 2*time.Second)
	require.Positive(t, refusing.attempts(), "the hops were delivered and refused")
	assert.Equal(t, 3, lag(t, url, worker), "refused hops are still waiting")

	accepting := newCollector(3)
	require.True(t, consume(t, bus, jsname.AgentFilter("team-ns", "worker"), worker, accepting, 30*time.Second))
	require.Eventually(t, func() bool { return lag(t, url, worker) == 0 }, 10*time.Second, 50*time.Millisecond,
		"once delivered, the worker's queue drains")
	assert.Equal(t, 2, lag(t, url, other), "draining one agent's queue leaves another's alone")
}

// The durable that consumed every registry's hops before they were addressed to an agent is narrowed to
// the registry-only subjects: it still drains a hop published that way, and never sees an agent-addressed
// one, which the agent's own durable delivers. Without the narrowing every hop would be delivered twice.
func TestJetStream_TheRegistryDurableDrainsOnlyRegistryOnlySubjects(t *testing.T) {
	url := startEmbeddedJetStream(t)
	bus := newBus(t, url)
	const durable = "dispatcher-legacy"

	// The durable as an earlier dispatcher created it: filtered on everything, with a BackOff.
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateOrUpdateConsumer(context.Background(), "CTXMESH_A2A", jetstream.ConsumerConfig{
		Durable: durable, FilterSubject: "ctxmesh.a2a.>", AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 5 * time.Second, MaxDeliver: 5, DeliverPolicy: jetstream.DeliverAllPolicy,
		BackOff: []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second, 300 * time.Second},
	})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(context.Background(), hop("old-1", asyncbus.Subject("reg-a"), `{}`)))
	require.NoError(t, bus.Publish(context.Background(), hop("new-1", jsname.AgentSubject("reg-a", "team-ns", "worker"), `{}`)))

	c := newCollector(1)
	require.True(t, consume(t, bus, jsname.RegistrySubjects, durable, c, 20*time.Second))
	require.Len(t, c.messages(), 1)
	assert.Equal(t, "old-1", c.messages()[0].ID, "a hop published on the registry-only subject is still delivered")

	info, err := consumerInfo(t, url, durable)
	require.NoError(t, err)
	assert.Equal(t, jsname.RegistrySubjects, info.Config.FilterSubject, "the durable's filter moved")
	assert.Empty(t, info.Config.BackOff, "its BackOff is gone, so its ack wait is the full AckWait")
	assert.Equal(t, asyncbus.AckWait, info.Config.AckWait)
	require.Eventually(t, func() bool { return lag(t, url, durable) == 0 }, 10*time.Second, 50*time.Millisecond,
		"the agent-addressed hop is not waiting for the registry durable")

	agent := newCollector(1)
	require.True(t, consume(t, bus, jsname.AgentFilter("team-ns", "worker"), jsname.AgentDurable("team-ns", "worker"),
		agent, 20*time.Second))
	assert.Equal(t, "new-1", agent.messages()[0].ID, "the agent's own durable delivers it")
}

// A durable nothing consumes is removed by the broker after twice the stream's retention, so the queue of
// an agent that is gone does not outlive it forever; at that age every message it could have held has
// expired, so a durable created again later delivers nothing twice.
func TestSubscribe_DurableExpiresAfterTwiceTheRetention(t *testing.T) {
	url := startEmbeddedJetStream(t)
	for maxAge, want := range map[time.Duration]time.Duration{0: 48 * time.Hour, time.Hour: 2 * time.Hour} {
		bus, err := asyncbus.NewJetStream(context.Background(), asyncbus.JetStreamOptions{URL: url, MaxAge: maxAge})
		require.NoError(t, err)
		durable := fmt.Sprintf("expiring-%s", want)
		ensureDurable(t, bus, url, jsname.AgentFilter("team-ns", "worker"), durable)
		info, err := consumerInfo(t, url, durable)
		require.NoError(t, err)
		assert.Equal(t, want, info.Config.InactiveThreshold)
		require.NoError(t, bus.Close())
	}
}

// A hop whose handler takes longer than the old first backoff step is delivered once. With a BackOff the
// broker took 5s as the ack wait of every first delivery and redelivered a slower hop while it was still
// being worked on; an agent's run usually takes longer than that.
func TestSubscribe_ASlowHandlerIsDeliveredOnce(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))
	subject := jsname.AgentSubject("reg-a", "team-ns", "slow")
	require.NoError(t, bus.Publish(context.Background(), hop("slow-1", subject, `{}`)))

	var calls atomic.Int32
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(ctx, jsname.AgentFilter("team-ns", "slow"), jsname.AgentDurable("team-ns", "slow"),
			func(context.Context, asyncbus.Message) error {
				if calls.Add(1) == 1 {
					time.Sleep(8 * time.Second)
					close(finished)
				}
				return nil
			})
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow hop was never handled")
	}
	time.Sleep(2 * time.Second) // a redelivery queued during the slow run would arrive now
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int32(1), calls.Load(), "a hop still being handled is not delivered again")
}

// One durable hands up to MaxInFlight hops to its handler at once, and never more: the broker is held to
// the same bound, so no hop waits unhandled while its ack wait runs.
func TestSubscribe_HandlesUpToMaxInFlightAtOnce(t *testing.T) {
	url := startEmbeddedJetStream(t)
	const limit = 3
	bus, err := asyncbus.NewJetStream(context.Background(), asyncbus.JetStreamOptions{URL: url, MaxInFlight: limit})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	subject, durable := jsname.AgentSubject("reg-a", "team-ns", "busy"), jsname.AgentDurable("team-ns", "busy")
	for i := range 7 {
		require.NoError(t, bus.Publish(context.Background(), hop(fmt.Sprintf("busy-%d", i), subject, `{}`)))
	}

	var running, most, handled atomic.Int32
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(ctx, jsname.AgentFilter("team-ns", "busy"), durable,
			func(context.Context, asyncbus.Message) error {
				now := running.Add(1)
				for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
				}
				<-release
				running.Add(-1)
				handled.Add(1)
				return nil
			})
	}()

	require.Eventually(t, func() bool { return running.Load() == limit }, 10*time.Second, 10*time.Millisecond,
		"hops to one agent are handled concurrently")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(limit), running.Load(), "and no more than the limit at once")
	info, err := consumerInfo(t, url, durable)
	require.NoError(t, err)
	assert.Equal(t, limit, info.Config.MaxAckPending)
	assert.LessOrEqual(t, info.NumAckPending, limit, "the broker has no more outstanding than there are handlers")

	close(release)
	require.Eventually(t, func() bool { return handled.Load() == 7 }, 10*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int32(limit), most.Load())
}

// A subject too long for the NATS protocol's control line is refused before it is sent. Sent, it would make
// the server close the connection for good, and every publish and consumer after it would fail until the
// process restarted.
func TestJetStream_AnOversizedSubjectIsRefusedAndTheConnectionSurvives(t *testing.T) {
	bus := newBus(t, startEmbeddedJetStream(t))

	long := jsname.AgentSubject("reg-a", "team-ns", strings.Repeat("a", 4100))
	require.Error(t, bus.Publish(context.Background(), hop("too-long", long, `{}`)))

	subject := jsname.AgentSubject("reg-a", "team-ns", "worker")
	require.NoError(t, bus.Publish(context.Background(), hop("after", subject, `{}`)),
		"the connection is still usable")
	require.NoError(t, bus.Err())
	c := newCollector(1)
	require.True(t, consume(t, bus, jsname.AgentFilter("team-ns", "worker"), jsname.AgentDurable("team-ns", "worker"), c,
		20*time.Second))
	assert.Equal(t, "after", c.messages()[0].ID)
}
