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

// Package asyncbus is the pluggable ASYNC-BACKEND seam for AMP hops (M141, ADR 0121).
//
// Before it, the only asynchronous path was Knative Eventing: an agent published a CloudEvent to its
// per-registry Broker and a Trigger pushed it to the callee's ksvc. That works, but it makes durable
// agent-to-agent messaging conditional on running Knative Eventing, and it gives an operator no way to
// put the platform's async traffic on the broker they already run.
//
// The seam is deliberately narrow — publish, and consume durably — because that is the whole of what an
// AMP hop needs and the whole of what every candidate backend agrees on. Everything richer (partitions,
// consumer groups, replay semantics) differs enough between brokers that putting it in the interface
// would mean picking a winner, which is the opposite of the point.
//
// # The delivery contract every backend must honour
//
//   - Publish returns nil only after the message is DURABLY accepted — persisted, not merely buffered in
//     the client. A publish that returns nil and then loses the message would break the whole point.
//   - Delivery is AT-LEAST-ONCE. Consumers must be idempotent; the launcher already is (it dedupes on the
//     envelope's messageId against a short-TTL Valkey seen-set, fail-closed — specs/eventing-scaling.md).
//     Every Message therefore carries the messageId as its ID, so a backend can also dedupe natively.
//   - A handler returning nil ACKs; returning an error NACKs, and the backend redelivers per its own
//     retry policy. A panic must not ack — the message is redelivered.
//   - Consumption survives a CONSUMER RESTART: a message published while nobody was listening, or
//     delivered but not acked, is delivered after the consumer comes back. This is the property that
//     separates a durable backend from a fan-out bus, and it is what the seam's conformance suite proves.
//
// # What is deliberately NOT here
//
// Credentials and connections live in the CONTROL PLANE, never in an agent pod — the same rule that put
// provider keys behind the token-service and Valkey behind the state-layer proxy. An agent publishes and
// receives over HTTP; the process holding a broker connection is ours.
package asyncbus

import (
	"context"
	"errors"
)

// ErrClosed is returned by a bus whose Close has already run.
var ErrClosed = errors.New("asyncbus: closed")

// Message is one async AMP hop in transit.
//
// Data is the encoded CloudEvent carrying the platform envelope, and Headers are its binding headers.
// The seam moves them OPAQUELY: it never parses the envelope, so the wire format stays owned by the AMP
// layer and a backend swap can never change what an agent receives.
type Message struct {
	// ID is the envelope's messageId — the idempotency key. Backends that support native deduplication
	// use it; consumers dedupe on it regardless, because at-least-once is the contract.
	ID string
	// Subject routes the message. It is the registry id (optionally suffixed per target agent), so a
	// consumer subscribes to exactly the traffic of the registry it serves — the same boundary the
	// per-registry Knative Broker draws today.
	Subject string
	// Data is the encoded CloudEvent body, moved verbatim.
	Data []byte
	// Headers are the CloudEvent binding headers, moved verbatim.
	Headers map[string]string
}

// Publisher durably enqueues async AMP hops.
type Publisher interface {
	// Publish returns nil only once the backend has DURABLY accepted the message. A backend that can only
	// confirm receipt-into-memory does not satisfy this and must return an error instead of lying.
	Publish(ctx context.Context, msg Message) error
	// Close releases the connection. Safe to call more than once.
	Close() error
}

// Handler processes one delivered message. nil ⇒ ack; an error ⇒ nack + redelivery.
type Handler func(ctx context.Context, msg Message) error

// Subscriber durably consumes async AMP hops.
//
// Not every backend implements it: with Knative Eventing the consumer is the Trigger, which pushes over
// HTTP from outside this process, so its Subscriber returns ErrPushDelivered rather than pretending. That
// asymmetry is real and worth surfacing rather than papering over — a caller that needs to pull must
// check for it.
type Subscriber interface {
	// Subscribe starts consuming subject under a DURABLE consumer name and blocks until ctx is done.
	// The durable name is what makes a restart resume rather than restart: the backend remembers that
	// consumer's position and its un-acked messages across process lifetimes.
	Subscribe(ctx context.Context, subject, durable string, handler Handler) error
	// Close releases the connection. Safe to call more than once.
	Close() error
}

// ErrPushDelivered reports a backend whose delivery is PUSH-based and therefore has no in-process
// consumer to run: the messages arrive over HTTP at the callee instead. Returned by Knative's Subscriber.
var ErrPushDelivered = errors.New(
	"asyncbus: this backend delivers by push (HTTP) — there is no in-process consumer to subscribe with")

// Compile-time proof that each backend satisfies the seam it claims — so a signature drift is a build
// failure here rather than a nil-interface surprise at wiring time.
var (
	_ Publisher  = (*JetStreamBus)(nil)
	_ Subscriber = (*JetStreamBus)(nil)
	_ Publisher  = (*KnativeBus)(nil)
	_ Subscriber = (*KnativeBus)(nil) // push-delivered: Subscribe returns ErrPushDelivered
)
