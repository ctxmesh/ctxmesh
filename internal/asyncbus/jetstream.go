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

package asyncbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
)

// The NATS JetStream backend (M141, ADR 0121) — the platform's self-hosted durable async broker.
//
// JetStream is a persisted STREAM plus named DURABLE CONSUMERS, which maps onto the seam's contract with
// nothing left over: a publish is acknowledged only after the stream has persisted it, and a durable
// consumer keeps its position and its un-acked messages across a consumer restart. It is one binary with
// no external dependency (no ZooKeeper/KRaft quorum to operate), which is what makes the M141 async bar
// runnable offline — the same de-risk the M140 model services gave the retrieval bar.

const (
	// streamName is the single stream carrying every registry's async AMP traffic. One stream with
	// per-registry SUBJECTS (rather than a stream per registry) keeps provisioning static: adding a
	// registry adds a subject, not a broker object, so nothing has to reconcile stream lifecycle against
	// AgentRegistry lifecycle. Consumers bind with a subject filter.
	streamName = jsname.Stream
	// subjectPrefix namespaces our subjects inside a NATS server an operator may share with other
	// workloads. The wildcard ctxmesh.a2a.> is what the stream captures.
	subjectPrefix = jsname.SubjectPrefix

	// publishTimeout bounds a publish. It must be a real bound: a publish that hangs would stall the AMP
	// hop that triggered it, and the caller has its own deadline to honour.
	publishTimeout = 10 * time.Second

	// AckWait is how long JetStream waits for an ack, or a progress report, before redelivering a
	// message. While a handler runs, Subscribe reports progress every quarter of it, so a message is
	// redelivered only once its handler has stopped: returned without acking, or died with the process.
	// It is also above the BFF dispatcher's 105s bound on a delivery, so even a lost progress report
	// cannot redeliver a hop still being delivered. The consumer sets no BackOff, because JetStream takes
	// a BackOff's first step as the ack wait of every first delivery, which redelivered any hop taking
	// longer than that step (5s) while it was still running.
	AckWait = 2 * time.Minute
	// defaultMaxAge bounds an undelivered message's life. A day is long enough to cover a dispatcher
	// outage, a node roll or a night, and short enough that a permanently-dead consumer cannot grow the
	// store forever. An operator with a different tolerance sets JetStreamOptions.MaxAge.
	defaultMaxAge = 24 * time.Hour

	// maxDeliver caps redelivery so a permanently-failing message cannot loop forever; past it the
	// message is terminated rather than retried, the same posture the run-worker's poison cap takes. It is
	// len(redeliveryBackoff)+1 — the first attempt plus one per backoff step.
	maxDeliver = 5

	// DefaultMaxInFlight is how many messages one durable hands to its handler at once when
	// JetStreamOptions.MaxInFlight is unset. Delivery is concurrent so that an agent with several replicas
	// gets work for all of them: the handler waits for the agent's run, so one at a time would leave every
	// replica but one idle.
	DefaultMaxInFlight = 8

	// maxSubjectBytes bounds a subject this bus publishes on. A subject is sent on the NATS protocol's
	// control line, which the server caps (4096 bytes by default) and answers past the cap with an error
	// that closes the client's connection for good. Platform subjects are five DNS-label tokens, far below
	// this.
	maxSubjectBytes = 512
)

// ackWait is AckWait, as the consumer is configured with it; a test shortens it to watch progress reports
// keep a slow handler's message from being redelivered.
var ackWait = AckWait

// ErrConnectionClosed reports a NATS connection that is closed for good: the client gave up on it, or
// the server ended it. Nothing published or consumed through the bus works again, so the process holding
// it has to be restarted.
var ErrConnectionClosed = errors.New("asyncbus: the NATS connection is closed")

// redeliveryBackoff is the delay before each REDELIVERY of a nacked message.
//
// It exists because a bare, immediate nack is a trap: a downstream agent that is merely cold-starting
// would nack, be redelivered within milliseconds, nack again, and burn the whole MaxDeliver budget in
// well under a second — turning a transient unavailability into a PERMANENTLY dropped hop. Backing off
// gives the callee time to actually become available, which is the entire point of retrying.
//
// The schedule spans ~6 minutes across four retries: long enough to ride out a cold start, a pod roll or
// a brief outage; short enough that a recovered agent is not left waiting.
var redeliveryBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	60 * time.Second,
	300 * time.Second,
}

// nakDelay returns how long to wait before redelivering, given how many times the message has already
// been delivered (1 on the first attempt). Past the schedule it holds at the final step.
func nakDelay(numDelivered uint64) time.Duration {
	i := min(max(int(numDelivered)-1, 0), len(redeliveryBackoff)-1)
	return redeliveryBackoff[i]
}

// Subject builds the registry-only routing subject, ctxmesh.a2a.<registryId>. Hops are published on
// jsname.AgentSubject, which addresses the receiver too; this form is still consumed so that a hop published
// on it before an upgrade is delivered.
func Subject(registryID string) string {
	return jsname.RegistrySubject(registryID)
}

// JetStreamBus is both Publisher and Subscriber over one NATS connection.
type JetStreamBus struct {
	nc          *nats.Conn
	js          jetstream.JetStream
	maxAge      time.Duration
	maxInFlight int
	// connClosed is closed when the connection is closed for good, by Close or by the server.
	connClosed chan struct{}
	closed     sync.Once
}

// JetStreamOptions configures the backend.
type JetStreamOptions struct {
	// URL is the NATS server (nats://host:4222). Required.
	URL string
	// CredentialsFile is an optional NATS credentials file — the operator's chosen auth. It is read from
	// the CONTROL PLANE's filesystem; an agent pod never holds it.
	CredentialsFile string
	// MaxAge bounds how long an undelivered message is kept. Zero ⇒ defaultMaxAge. It exists because
	// retention is by LIMITS (see the stream config): messages are kept regardless of who is listening,
	// so something has to stop the store growing without bound.
	MaxAge time.Duration
	// Replicas is the stream replica count. 1 for a single-node dev broker; 3 for a real cluster. The
	// default (0 ⇒ 1) is honest about a single-node install rather than pretending to be replicated.
	Replicas int
	// MaxInFlight is how many messages one Subscribe hands to its handler at once, and the most
	// unacknowledged messages the broker gives its durable. Zero ⇒ DefaultMaxInFlight.
	MaxInFlight int
}

// NewJetStream connects, ensures the stream exists, and returns the bus. It is safe to call from every
// replica: stream creation is CreateOrUpdate, so concurrent callers converge on one stream rather than
// racing to fail.
func NewJetStream(ctx context.Context, opts JetStreamOptions) (*JetStreamBus, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("asyncbus: a NATS URL is required")
	}
	connClosed := make(chan struct{})
	connOpts := []nats.Option{
		nats.Name("ctxmesh-asyncbus"),
		// Reconnect indefinitely: a broker restart must not permanently detach a control-plane consumer.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		// Some server errors close a connection with no reconnect at all. Err and Subscribe report it, so
		// the process can be restarted instead of staying connected to nothing.
		nats.ClosedHandler(func(*nats.Conn) { close(connClosed) }),
	}
	if f := strings.TrimSpace(opts.CredentialsFile); f != "" {
		connOpts = append(connOpts, nats.UserCredentials(f))
	}
	nc, err := nats.Connect(opts.URL, connOpts...)
	if err != nil {
		return nil, fmt.Errorf("asyncbus: connecting to NATS at %q: %w", opts.URL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("asyncbus: opening JetStream: %w", err)
	}

	replicas := opts.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	cfg := jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subjectPrefix + ".>"},
		// FILE, not memory: the seam promises a publish survives a broker restart, and a memory stream
		// would quietly break that promise on the very failure it exists to cover.
		Storage:  jetstream.FileStorage,
		Replicas: replicas,
		// LIMITS retention, NOT interest/work-queue. Interest retention drops a message published to a
		// subject that has no registered consumer yet — so an agent that published an async hop a moment
		// before its dispatcher first came up would have the message SILENTLY DISCARDED, which is exactly
		// the loss the seam promises cannot happen. Work-queue retention keeps it, but forbids overlapping
		// consumer filters, turning a second durable on the same subject into a create-time failure.
		// Limits retention keeps every message until MaxAge regardless of who is listening; acks still
		// govern per-consumer delivery. Bounded storage is the price, and it is the right one to pay.
		Retention: jetstream.LimitsPolicy,
		MaxAge:    maxAge,
		// Native dedupe over the publish window: a producer retry that re-sends the same messageId is
		// collapsed by the broker. The launcher still dedupes (the contract is at-least-once and the
		// window is finite) — this only narrows how often it has to.
		Duplicates: 2 * time.Minute,
	}
	if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
		nc.Close()
		return nil, fmt.Errorf("asyncbus: ensuring stream %q: %w", streamName, err)
	}
	maxInFlight := opts.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = DefaultMaxInFlight
	}
	return &JetStreamBus{nc: nc, js: js, maxAge: maxAge, maxInFlight: maxInFlight, connClosed: connClosed}, nil
}

// Publish durably enqueues the message: it returns only after JetStream has ACKED it, which it does after
// persisting to the file store.
func (b *JetStreamBus) Publish(ctx context.Context, msg Message) error {
	if msg.Subject == "" {
		return fmt.Errorf("asyncbus: a subject is required to publish")
	}
	if len(msg.Subject) > maxSubjectBytes {
		return fmt.Errorf("asyncbus: a %d-byte subject is over the %d-byte limit", len(msg.Subject), maxSubjectBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	m := &nats.Msg{Subject: msg.Subject, Data: msg.Data, Header: nats.Header{}}
	for k, v := range msg.Headers {
		m.Header.Set(k, v)
	}
	if msg.ID != "" {
		// Nats-Msg-Id drives JetStream's dedupe window.
		m.Header.Set(jetstream.MsgIDHeader, msg.ID)
	}
	if _, err := b.js.PublishMsg(ctx, m); err != nil {
		return fmt.Errorf("asyncbus: publishing %q to %q: %w", msg.ID, msg.Subject, err)
	}
	return nil
}

// Subscribe consumes subject under a durable consumer and blocks until ctx is done, or until the
// connection is closed for good, which it reports as ErrConnectionClosed.
//
// The consumer is created with CreateOrUpdateConsumer, so a restart REJOINS the existing durable rather
// than starting a fresh one — that is what makes an un-acked or a while-you-were-away message arrive
// after the consumer comes back, which is the property the whole seam exists for. Subscribing an existing
// durable with a different subject moves its filter.
//
// Up to MaxInFlight messages are handled at once, each acked or nacked on its own, and the broker is held
// to the same bound (MaxAckPending), so it never hands the durable more unacknowledged messages than
// there are handlers to run them. A handler must return within AckWait, or the broker redelivers its
// message while it is still running.
func (b *JetStreamBus) Subscribe(ctx context.Context, subject, durable string, handler Handler) error {
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(durable) == "" {
		return fmt.Errorf("asyncbus: subscribe needs both a subject and a durable name")
	}
	stream, err := b.js.Stream(ctx, streamName)
	if err != nil {
		return fmt.Errorf("asyncbus: opening stream %q: %w", streamName, b.closedOr(err))
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: subject,
		// Explicit acks are the whole point: an implicit ack would mark work done before the handler ran.
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    ackWait,
		MaxDeliver: maxDeliver,
		// Deliver everything the durable has not yet acked, oldest first.
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MaxAckPending: b.maxInFlight,
		// A durable nothing has pulled from for twice the stream's retention is deleted by the broker, so
		// the durable of an agent that is gone does not outlive it forever. Twice is what keeps a re-created
		// durable exact: every message still stored was published after the old one went quiet, so
		// delivering them all delivers none twice.
		InactiveThreshold: 2 * b.maxAge,
	})
	if err != nil {
		if !serverAtLeast(b.nc.ConnectedServerVersion(), 2, 10) {
			return fmt.Errorf("asyncbus: creating durable consumer %q: %w (NATS 2.10 or newer is required "+
				"to move a durable's filter; this server is %s)", durable, err, b.nc.ConnectedServerVersion())
		}
		return fmt.Errorf("asyncbus: creating durable consumer %q: %w", durable, b.closedOr(err))
	}

	workers := newWorkerPool(b.maxInFlight)
	consumeCtx, err := cons.Consume(func(m jetstream.Msg) {
		// MaxAckPending keeps the broker from delivering more than there are workers, so this rarely
		// waits: a worker frees as its message is acked.
		workers.run(func() { handleMessage(ctx, m, handler) })
	})
	if err != nil {
		return fmt.Errorf("asyncbus: consuming %q: %w", subject, b.closedOr(err))
	}
	select {
	case <-ctx.Done():
	case <-b.connClosed:
		// Nothing can be received or acked on a closed connection, so there is nothing to wait for.
		consumeCtx.Stop()
		return b.Err()
	}

	// Stop() is ASYNCHRONOUS: it signals the consume loop and returns immediately, so
	// returning here would break this method's contract. A caller that treats the
	// return as "I am no longer consuming" would be wrong — the old loop can still be
	// attached, still receiving, and still ACKING. Two subscribers on one durable then
	// split the stream between them, and messages meant for the new one are consumed
	// and acked by the old one, silently.
	//
	// Closed() is the client's own "fully stopped/drained, processing complete" signal; the handlers it
	// started are waited for after it, and see ctx done, so they end promptly. Bounded, because a wedged
	// handler must not hold shutdown open forever.
	consumeCtx.Stop()
	deadline := time.NewTimer(consumerStopTimeout)
	defer deadline.Stop()
	select {
	case <-consumeCtx.Closed():
	case <-deadline.C:
		return nil
	}
	handled := make(chan struct{})
	go func() { workers.wait(); close(handled) }()
	select {
	case <-handled:
	case <-deadline.C:
	}
	return nil
}

// workerPool runs at most a fixed number of functions at once.
type workerPool struct {
	slots chan struct{}
	wg    sync.WaitGroup
}

func newWorkerPool(n int) *workerPool {
	return &workerPool{slots: make(chan struct{}, n)}
}

// run waits for a free worker, then runs fn on it.
func (p *workerPool) run(fn func()) {
	p.slots <- struct{}{}
	p.wg.Go(func() {
		defer func() { <-p.slots }()
		fn()
	})
}

// wait blocks until every function started by run has returned.
func (p *workerPool) wait() { p.wg.Wait() }

// handleMessage runs the handler for one message and acks or naks it, reporting progress while the
// handler runs so the broker does not redeliver the message to another worker meanwhile.
func handleMessage(ctx context.Context, m jetstream.Msg, handler Handler) {
	// A panicking handler must NOT ack — leaving the message un-acked lets AckWait redeliver it,
	// which is the correct outcome for a bug we cannot reason about here.
	defer func() { _ = recover() }()

	working := make(chan struct{})
	defer close(working)
	go func() {
		tick := time.NewTicker(ackWait / 4)
		defer tick.Stop()
		for {
			select {
			case <-working:
				return
			case <-tick.C:
				_ = m.InProgress()
			}
		}
	}()

	headers := make(map[string]string, len(m.Headers()))
	for k := range m.Headers() {
		headers[k] = m.Headers().Get(k)
	}
	out := Message{
		ID:      m.Headers().Get(jetstream.MsgIDHeader),
		Subject: m.Subject(),
		Data:    m.Data(),
		Headers: headers,
	}
	if err := handler(ctx, out); err != nil {
		// Redeliver AFTER a backoff, never immediately — see redeliveryBackoff. MaxDeliver bounds the
		// loop; past it JetStream stops redelivering rather than retrying forever.
		delay := redeliveryBackoff[0]
		if meta, mErr := m.Metadata(); mErr == nil {
			delay = nakDelay(meta.NumDelivered)
		}
		_ = m.NakWithDelay(delay)
		return
	}
	_ = m.Ack()
}

// Err is nil while the connection is usable, and wraps ErrConnectionClosed, with the server's reason when
// there is one, once it is closed for good.
func (b *JetStreamBus) Err() error {
	select {
	case <-b.connClosed:
	default:
		return nil
	}
	if last := b.nc.LastError(); last != nil {
		return fmt.Errorf("%w: %w", ErrConnectionClosed, last)
	}
	return ErrConnectionClosed
}

// closedOr is Err when the connection is closed for good, which explains err better, else err.
func (b *JetStreamBus) closedOr(err error) error {
	if cErr := b.Err(); cErr != nil {
		return cErr
	}
	return err
}

// serverAtLeast reports whether a NATS server version "major.minor.patch" is at least major.minor. A
// version it cannot read counts as new enough, so only a known-old server earns the upgrade hint.
func serverAtLeast(version string, major, minor int) bool {
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(version, "v"), "%d.%d", &gotMajor, &gotMinor); err != nil {
		return true
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

// consumerStopTimeout bounds how long Subscribe waits for the consume loop to finish
// after its context is cancelled. Generous: the wait exists so a rebind of the same
// durable cannot race a loop that is still attached, and the only cost of waiting is
// a slower shutdown.
const consumerStopTimeout = 30 * time.Second

// Close drains and closes the connection. Idempotent.
func (b *JetStreamBus) Close() error {
	b.closed.Do(func() { b.nc.Close() })
	return nil
}
