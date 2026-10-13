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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The async AMP DISPATCHER (M141.4, ADR 0121) — the half that turns a durable message back into a call.
//
// Knative Eventing PUSHES: its Broker delivers over HTTP, so a ksvc can scale to zero and be woken by
// traffic. JetStream is PULLED: something must hold a connection and consume. A ksvc cannot, and putting
// a broker connection in an agent pod would mean broker credentials in agent pods — the thing the
// token-service and the state-layer proxy exist to prevent. So the control plane consumes and delivers
// over HTTP, which is deliberately the same shape Knative's Broker+Trigger has: durable transport plus
// HTTP delivery. That is why the backend swap is invisible to an agent — the launcher still receives a
// CloudEvent POST and still dedupes on the envelope's messageId, exactly as it does today.
//
// It runs as a background worker inside the BFF, alongside the run-worker, the online scorer and the
// cost-rollup worker, rather than as a new Deployment: it needs the same control-plane reach they have
// and nothing else.
//
// Each registry member has a durable consumer of its own (jsname.AgentDurable), so the consumer's pending
// count is the number of hops waiting for that agent: the metric a queue-depth AgentScalingPolicy scales
// the agent on. The set follows the capability registry. A hop to an agent that is not a member yet waits
// in the stream and is delivered once the agent joins, within the stream's retention.

const (
	// asyncDispatchTimeout bounds one delivery attempt. Generous, because the callee may be a ksvc cold
	// starting from zero — and a timeout here is a NACK, which costs a backoff cycle, not a lost message.
	asyncDispatchTimeout = 60 * time.Second

	// asyncDurableName is the durable that drains hops published on the registry-only subject
	// (jsname.RegistrySubjects) before hops were addressed to their receiver. It is a fixed name on
	// purpose: a generated one would create a NEW consumer on every boot, so the messages waiting for the
	// old one would never be delivered — the exact failure durability is meant to prevent.
	asyncDurableName = "ctxmesh-async-dispatcher"

	// asyncDeliveryAttestationTTL bounds the attestation minted for one delivery attempt: it is used at once,
	// by a receiver that may be cold-starting.
	asyncDeliveryAttestationTTL = asyncDispatchTimeout + time.Minute

	// defaultAsyncMemberResync is how often the dispatcher re-reads the registry members it keeps a
	// consumer for, and how long it waits before retrying a consumer the backend refused.
	defaultAsyncMemberResync = 15 * time.Second
)

// AsyncDispatcherConfig configures the dispatcher.
type AsyncDispatcherConfig struct {
	// Subscriber is the durable consumer side of the async backend.
	Subscriber asyncbus.Subscriber
	// Client delivers to agents. Nil ⇒ a bounded default.
	Client *http.Client
	// MemberResync is how often the registry members are re-read. Zero ⇒ defaultAsyncMemberResync.
	MemberResync time.Duration
}

// StartAsyncDispatcher consumes async AMP hops and delivers them to their target agents until ctx is
// done. It returns immediately; the consumers run in goroutines, like the other BFF workers.
func (s *Server) StartAsyncDispatcher(ctx context.Context, cfg AsyncDispatcherConfig) {
	if cfg.Subscriber == nil {
		return
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: asyncDispatchTimeout}
	}
	resync := cfg.MemberResync
	if resync <= 0 {
		resync = defaultAsyncMemberResync
	}
	deliver := func(hctx context.Context, msg asyncbus.Message) error {
		// The backend records nothing about a nack, so every failed attempt is logged here with its
		// reason; past the delivery budget the backend stops redelivering the message.
		err := s.deliverAsyncHop(hctx, client, msg)
		if err != nil {
			s.log.Error(err, "async dispatch: delivery failed; redelivering after a backoff",
				"messageId", msg.ID, "subject", msg.Subject)
		}
		return err
	}
	go s.consumeAsyncQueue(ctx, cfg.Subscriber, jsname.RegistrySubjects, asyncDurableName, resync, deliver)
	if s.agentCapabilities != nil {
		go s.syncAgentQueues(ctx, cfg.Subscriber, resync, deliver)
	}
}

// consumeAsyncQueue runs one durable consumer until ctx is done. A backend error is logged loudly and the
// consumer is retried: while it is down its hops accumulate in the stream (they are durable, so nothing
// is lost) but none is delivered, and that is not a state to discover from a user report.
func (s *Server) consumeAsyncQueue(
	ctx context.Context, sub asyncbus.Subscriber, subject, durable string, retry time.Duration, h asyncbus.Handler,
) {
	for {
		err := sub.Subscribe(ctx, subject, durable, h)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Error(err, "async dispatcher: a consumer stopped; its hops are queuing undelivered until it "+
				"is retried", "durable", durable, "retryIn", retry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// syncAgentQueues keeps one consumer running for every registry member, re-reading the members every
// resync: a new member's consumer starts, and the consumer of an agent that left every registry stops.
// A failed read changes nothing, so a capability-registry outage never stops delivery.
func (s *Server) syncAgentQueues(ctx context.Context, sub asyncbus.Subscriber, resync time.Duration, h asyncbus.Handler) {
	running := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, stop := range running {
			stop()
		}
		wg.Wait()
	}()
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		members, err := s.agentCapabilities.ListMembers(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error(err, "async dispatcher: reading the registry members failed; keeping the current consumers")
		} else {
			want := make(map[string]bool, len(members))
			for _, m := range members {
				durable := jsname.AgentDurable(m.Namespace, m.Agent)
				want[durable] = true
				if running[durable] != nil {
					continue
				}
				qctx, stop := context.WithCancel(ctx)
				running[durable] = stop
				wg.Add(1)
				go func(filter string) {
					defer wg.Done()
					s.consumeAsyncQueue(qctx, sub, filter, durable, resync, h)
				}(jsname.AgentFilter(m.Namespace, m.Agent))
			}
			for durable, stop := range running {
				if !want[durable] {
					stop()
					delete(running, durable)
					s.log.Info("async dispatcher: an agent left its registry; hops waiting for it are kept "+
						"until it joins one again or they expire", "durable", durable)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverAsyncHop POSTs one message to its target agent as a CloudEvent.
//
// Routing needs no envelope parsing: the CloudEvent's `type` is the receiver agent and its `id` is the
// messageId, both set by the producer (cmd/launcher/cloudevent.go). The namespace and registry come from
// the PLATFORM headers the publish edge stamped from the verified run, never from the producer.
//
// Returning an error nacks, so the message is redelivered after the backend's backoff until its delivery
// budget is spent. That is the answer for a cold-starting callee, and for a receiver the capability registry
// has no row or no address for: the row may not be written yet, and a receiver that never gets one (a job
// agent receives nothing) fails visibly rather than being acked away. Only a message that can never be
// delivered for a settled reason (no routing context, a registry boundary, the callee's own refusal) is
// ACKED, with that reason logged.
func (s *Server) deliverAsyncHop(ctx context.Context, client *http.Client, msg asyncbus.Message) error {
	namespace := strings.TrimSpace(msg.Headers[headerAsyncNamespace])
	registryID := strings.TrimSpace(msg.Headers[headerAsyncRegistry])
	receiver := strings.TrimSpace(msg.Headers[ceTypeHeader])

	if namespace == "" || receiver == "" {
		s.log.Info("async dispatch: dropping an unroutable message (no namespace or receiver)",
			"messageId", msg.ID, "subject", msg.Subject)
		return nil // ack: retrying cannot add the missing routing context
	}

	// Registry isolation, enforced here as well as at the callee. The callee's launcher hard-denies a
	// cross-registry envelope at AMP layer 1, but that check reads the envelope's OWN registryId — which
	// the producer wrote. Checking the TARGET's real registry against the registry the message was
	// published into (stamped by the edge, not the pod) closes the gap where a crafted envelope claims a
	// registry its sender does not belong to.
	target, ok, err := s.agentCapabilities.Get(ctx, namespace, receiver)
	if err != nil {
		return fmt.Errorf("async dispatch: resolving %s/%s: %w", namespace, receiver, err)
	}
	if !ok {
		return fmt.Errorf("async dispatch: %s/%s has no capability-registry row", namespace, receiver)
	}
	if target.RegistryID != registryID {
		s.log.Info("async dispatch: refusing delivery outside the sender's registry",
			"messageId", msg.ID, "receiver", namespace+"/"+receiver, "senderRegistry", registryID)
		return nil // ack: this will never become deliverable
	}
	if target.Endpoint == "" {
		return fmt.Errorf("async dispatch: %s/%s has no delivery address (a job agent receives no AMP deliveries)",
			namespace, receiver)
	}

	attestation, deliverable, err := s.attestAsyncSender(ctx, msg, namespace, registryID, receiver)
	if err != nil || !deliverable {
		return err // an error nacks; undeliverable is acked, and logged there
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.Endpoint, bytes.NewReader(msg.Data))
	if err != nil {
		return fmt.Errorf("async dispatch: building the delivery request: %w", err)
	}
	for k, v := range msg.Headers {
		switch http.CanonicalHeaderKey(k) {
		case headerAsyncNamespace, headerAsyncRegistry, headerAsyncSender, runcap.CloudEventAttestationHeader:
			continue // platform context; not part of the CloudEvent the agent sees
		}
		req.Header.Set(k, v)
	}
	if attestation != "" {
		req.Header.Set(runcap.CloudEventAttestationHeader, attestation)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("async dispatch: delivering %q to %s/%s: %w", msg.ID, namespace, receiver, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// A 4xx from the callee is its own refusal (a cross-registry deny, a caller not on its allowlist).
	// Retrying will not change its mind, so ack and record it rather than burning the delivery budget.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		s.log.Info("async dispatch: the callee refused the hop; not retrying",
			"messageId", msg.ID, "receiver", namespace+"/"+receiver, "status", resp.StatusCode)
		return nil
	}
	return fmt.Errorf("async dispatch: %s/%s answered %d", namespace, receiver, resp.StatusCode)
}

// attestAsyncSender mints the platform's attestation of a hop's sender for its receiver, which a registry
// member requires before it admits a delivery (ADR 0153). The sender is the agent whose verified run
// published the hop; its registry and role are read now, from the capability registry, so a sender that
// left the registry the hop was published into since is not attested into it. A message published before
// senders were recorded is delivered without one, and the receiver's posture decides.
//
// deliverable is false, with the reason logged, for a hop that can never be attested; the caller acks it.
func (s *Server) attestAsyncSender(
	ctx context.Context, msg asyncbus.Message, namespace, registryID, receiver string,
) (attestation string, deliverable bool, err error) {
	senderName := strings.TrimSpace(msg.Headers[headerAsyncSender])
	if senderName == "" || s.capabilitySigner == nil {
		return "", true, nil
	}
	sender, ok, err := s.agentCapabilities.Get(ctx, namespace, senderName)
	if err != nil {
		return "", false, fmt.Errorf("async dispatch: resolving the sender %s/%s: %w", namespace, senderName, err)
	}
	if !ok || sender.RegistryID != registryID {
		s.log.Info("async dispatch: the sender is no longer in the registry it published into; not delivering",
			"messageId", msg.ID, "sender", namespace+"/"+senderName, "registry", registryID)
		return "", false, nil
	}
	attestation, err = s.capabilitySigner.MintAttestation(runcap.AttestationRequest{
		Namespace:  namespace,
		Agent:      senderName,
		RegistryID: sender.RegistryID,
		Role:       sender.Role,
		Receiver:   receiver,
		TTL:        asyncDeliveryAttestationTTL,
	})
	if err != nil {
		return "", false, fmt.Errorf("async dispatch: attesting the sender of %q: %w", msg.ID, err)
	}
	return attestation, true, nil
}

// agentAddress is where the platform calls an agent: the address its capability-registry row records for
// its execution model (an eventing agent serves at <agent>-eventing, not at a Knative route), else its
// Knative route. An agent with no row, or one written before addresses were recorded, has a route.
func (s *Server) agentAddress(ctx context.Context, namespace, agent string) (string, error) {
	if s.agentCapabilities != nil {
		row, ok, err := s.agentCapabilities.Get(ctx, namespace, agent)
		if err != nil {
			return "", fmt.Errorf("resolving the address of %s/%s: %w", namespace, agent, err)
		}
		if ok && row.Endpoint != "" {
			return row.Endpoint, nil
		}
	}
	return s.agentEndpoint(agent, namespace), nil
}

// agentEndpoint is the in-cluster address of a serving agent's Knative route. Injectable (agentURL) for
// tests.
func (s *Server) agentEndpoint(agent, namespace string) string {
	if s.agentURL != nil {
		return s.agentURL(agent, namespace)
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", agent, namespace)
}
