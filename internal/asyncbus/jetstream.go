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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The NATS JetStream backend (M141, ADR 0121) — the platform's self-hosted durable async broker.
//
// JetStream is a persisted STREAM plus named DURABLE CONSUMERS, which maps onto the seam's contract with
// nothing left over: a publish is acknowledged only after the stream has persisted it, and a durable
// consumer keeps its position and its un-acked messages across a consumer restart. It is one binary with
// no external dependency (no ZooKeeper/KRaft quorum to operate), which is what makes the M141 async bar
// runnable offline — the same de-risk the M140 model services gave the retrieval bar.

const (
	// streamName is the single stream carrying every registry's async A2A traffic. One stream with
	// per-registry SUBJECTS (rather than a stream per registry) keeps provisioning static: adding a
	// registry adds a subject, not a broker object, so nothing has to reconcile stream lifecycle against
	// AgentRegistry lifecycle. Consumers still bind per-registry via a subject filter.
	streamName = "AGENTRY_A2A"
	// subjectPrefix namespaces our subjects inside a NATS server an operator may share with other
	// workloads. The wildcard AGENTRY_A2A.> is what the stream captures.
	subjectPrefix = "agentry.a2a"

	// publishTimeout bounds a publish. It must be a real bound: a publish that hangs would stall the A2A
	// hop that triggered it, and the caller has its own deadline to honour.
	publishTimeout = 10 * time.Second

	// ackWait is how long JetStream waits for an ack before redelivering. It has to exceed a realistic
	// handler duration (the handler POSTs to an agent, which may be cold-starting) or the broker will
	// redeliver work that is still in flight and turn at-least-once into a stampede.
	ackWait = 2 * time.Minute
	// defaultMaxAge bounds an undelivered message's life. A day is long enough to cover a dispatcher
	// outage, a node roll or a night, and short enough that a permanently-dead consumer cannot grow the
	// store forever. An operator with a different tolerance sets JetStreamOptions.MaxAge.
	defaultMaxAge = 24 * time.Hour

	// maxDeliver caps redelivery so a permanently-failing message cannot loop forever; past it the
	// message is terminated rather than retried, the same posture the run-worker's poison cap takes. It is
	// len(redeliveryBackoff)+1 — the first attempt plus one per backoff step.
	maxDeliver = 5
)

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

// Subject builds the routing subject for a registry. Exported so the publisher and the dispatcher agree
// on one spelling instead of formatting it independently at two call sites.
func Subject(registryID string) string {
	return subjectPrefix + "." + sanitizeSubjectToken(registryID)
}

// sanitizeSubjectToken keeps a registry id from breaking out of its subject token. A registry id is a DNS
// label by CRD validation, so this is defence in depth rather than a live threat — but a subject token is
// a routing decision, and a '.' or '>' smuggled into one would silently widen what a consumer receives.
func sanitizeSubjectToken(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// JetStreamBus is both Publisher and Subscriber over one NATS connection.
type JetStreamBus struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	closed sync.Once
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
}

// NewJetStream connects, ensures the stream exists, and returns the bus. It is safe to call from every
// replica: stream creation is CreateOrUpdate, so concurrent callers converge on one stream rather than
// racing to fail.
func NewJetStream(ctx context.Context, opts JetStreamOptions) (*JetStreamBus, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("asyncbus: a NATS URL is required")
	}
	connOpts := []nats.Option{
		nats.Name("agentry-asyncbus"),
		// Reconnect indefinitely: a broker restart must not permanently detach a control-plane consumer.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
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
	return &JetStreamBus{nc: nc, js: js}, nil
}

// Publish durably enqueues the message: it returns only after JetStream has ACKED it, which it does after
// persisting to the file store.
func (b *JetStreamBus) Publish(ctx context.Context, msg Message) error {
	if msg.Subject == "" {
		return fmt.Errorf("asyncbus: a subject is required to publish")
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

// Subscribe consumes subject under a durable consumer and blocks until ctx is done.
//
// The consumer is created with CreateOrUpdateConsumer, so a restart REJOINS the existing durable rather
// than starting a fresh one — that is what makes an un-acked or a while-you-were-away message arrive
// after the consumer comes back, which is the property the whole seam exists for.
func (b *JetStreamBus) Subscribe(ctx context.Context, subject, durable string, handler Handler) error {
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(durable) == "" {
		return fmt.Errorf("asyncbus: subscribe needs both a subject and a durable name")
	}
	stream, err := b.js.Stream(ctx, streamName)
	if err != nil {
		return fmt.Errorf("asyncbus: opening stream %q: %w", streamName, err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: subject,
		// Explicit acks are the whole point: an implicit ack would mark work done before the handler ran.
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    ackWait,
		MaxDeliver: maxDeliver,
		// BackOff governs redelivery after an ACK TIMEOUT (a handler that died mid-flight); explicit naks
		// carry their own delay below. Both follow the same schedule so a failure behaves the same way
		// whether the handler said "no" or simply never came back.
		BackOff: redeliveryBackoff,
		// Deliver everything the durable has not yet acked, oldest first.
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("asyncbus: creating durable consumer %q: %w", durable, err)
	}

	consumeCtx, err := cons.Consume(func(m jetstream.Msg) {
		// A panicking handler must NOT ack — leaving the message un-acked lets AckWait redeliver it,
		// which is the correct outcome for a bug we cannot reason about here.
		defer func() { _ = recover() }()

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
	})
	if err != nil {
		return fmt.Errorf("asyncbus: consuming %q: %w", subject, err)
	}
	defer consumeCtx.Stop()

	<-ctx.Done()
	return nil
}

// Close drains and closes the connection. Idempotent.
func (b *JetStreamBus) Close() error {
	b.closed.Do(func() { b.nc.Close() })
	return nil
}
