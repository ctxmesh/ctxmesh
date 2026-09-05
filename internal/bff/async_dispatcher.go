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
	"time"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
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

const (
	// asyncDispatchTimeout bounds one delivery attempt. Generous, because the callee may be a ksvc cold
	// starting from zero — and a timeout here is a NACK, which costs a backoff cycle, not a lost message.
	asyncDispatchTimeout = 60 * time.Second

	// asyncDurableName is the JetStream durable consumer this dispatcher rejoins across restarts. It is a
	// fixed name on purpose: a generated one would create a NEW consumer on every boot, so the messages
	// waiting for the old one would never be delivered — the exact failure durability is meant to prevent.
	asyncDurableName = "ctxmesh-async-dispatcher"

	// asyncAllRegistries is the subject filter covering every registry's traffic. One dispatcher serves
	// them all; isolation is not weakened by that, because the callee still hard-denies a cross-registry
	// envelope at AMP layer 1, and this dispatcher independently refuses to cross a registry boundary
	// (see deliver).
	asyncAllRegistries = "ctxmesh.a2a.>"
)

// AsyncDispatcherConfig configures the dispatcher.
type AsyncDispatcherConfig struct {
	// Subscriber is the durable consumer side of the async backend.
	Subscriber asyncbus.Subscriber
	// Client delivers to agents. Nil ⇒ a bounded default.
	Client *http.Client
}

// StartAsyncDispatcher consumes async AMP hops and delivers them to their target agents until ctx is
// done. It returns immediately; the loop runs in a goroutine, like the other BFF workers.
func (s *Server) StartAsyncDispatcher(ctx context.Context, cfg AsyncDispatcherConfig) {
	if cfg.Subscriber == nil {
		return
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: asyncDispatchTimeout}
	}
	go func() {
		// Subscribe blocks until ctx is done. A backend error is worth surfacing loudly: with the
		// dispatcher down, hops accumulate in the stream (they are durable, so nothing is lost) but
		// nothing is delivered, and that is not a state to discover from a user report.
		if err := cfg.Subscriber.Subscribe(ctx, asyncAllRegistries, asyncDurableName,
			func(hctx context.Context, msg asyncbus.Message) error {
				return s.deliverAsyncHop(hctx, client, msg)
			},
		); err != nil && ctx.Err() == nil {
			s.log.Error(err, "async dispatcher stopped; async AMP hops are queuing undelivered")
		}
	}()
}

// deliverAsyncHop POSTs one message to its target agent as a CloudEvent.
//
// Routing needs no envelope parsing: the CloudEvent's `type` is the receiver agent and its `id` is the
// messageId, both set by the producer (cmd/launcher/cloudevent.go). The namespace and registry come from
// the PLATFORM headers the publish edge stamped from the verified run, never from the producer.
//
// Returning an error nacks, so the message is redelivered after the backend's backoff. That is the right
// answer for a cold-starting or briefly-unavailable callee; it is the wrong answer for a message that can
// never be delivered, which is why the un-routable cases below are ACKED with a logged reason instead of
// being retried forever.
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
	if !ok || target.RegistryID != registryID {
		s.log.Info("async dispatch: refusing delivery outside the sender's registry",
			"messageId", msg.ID, "receiver", namespace+"/"+receiver, "senderRegistry", registryID)
		return nil // ack: this will never become deliverable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.agentEndpoint(receiver, namespace), bytes.NewReader(msg.Data))
	if err != nil {
		return fmt.Errorf("async dispatch: building the delivery request: %w", err)
	}
	for k, v := range msg.Headers {
		switch k {
		case headerAsyncNamespace, headerAsyncRegistry:
			continue // platform routing context; not part of the CloudEvent the agent sees
		}
		req.Header.Set(k, v)
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

// agentEndpoint is the in-cluster address of an agent's service — the same shape the launcher's delegate
// path resolves, so an async hop lands exactly where a synchronous one would. Injectable (agentURL) so a
// unit test can point delivery at an httptest server instead of cluster DNS.
func (s *Server) agentEndpoint(agent, namespace string) string {
	if s.agentURL != nil {
		return s.agentURL(agent, namespace)
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", agent, namespace)
}
