//go:build integration

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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// THE M141 ASYNC BAR, end to end and against the REAL broker: an agent publishes a hop, it is DURABLY
// enqueued, and the target agent receives it as a CloudEvent — including when the dispatcher was not
// running at the moment it was published.
//
// The two halves are unit-tested separately above with a recording bus; this joins them through an
// embedded nats-server on a file store, because the property that matters (the hop survives the gap
// between publish and delivery) only exists when a real durable backend is in the middle.
func TestAsyncHop_SurvivesADispatcherRestart(t *testing.T) {
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(15*time.Second))
	defer srv.Shutdown()

	bus, err := asyncbus.NewJetStream(context.Background(), asyncbus.JetStreamOptions{URL: srv.ClientURL()})
	require.NoError(t, err)
	defer func() { _ = bus.Close() }()

	// The receiving agent.
	var mu sync.Mutex
	var received, attestations []string
	delivered := make(chan struct{}, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get("Ce-Id"))
		attestations = append(attestations, r.Header.Get(runcap.CloudEventAttestationHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer agent.Close()

	caller := mkParentRun("run-async-e2e")
	s, signer, _ := newAsyncServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Coordinates work."),
		reviewerIn("reg-a"),
	)
	s.asyncPublisher = bus
	client, _ := redirectTo(agent)

	// (1) PUBLISH with NO dispatcher running — the hop has nowhere to go yet and must not be lost.
	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// (2) NOW start the dispatcher — the restart case: a hop published while the control plane's
	// consumer was down is delivered once it comes back.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartAsyncDispatcher(ctx, AsyncDispatcherConfig{Subscriber: bus, Client: client})

	select {
	case <-delivered:
	case <-time.After(30 * time.Second):
		t.Fatal("the hop published before the dispatcher started was never delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"msg-1"}, received,
		"the target agent receives the CloudEvent, identified by the same messageId it was published with")
	att, err := signer.Verifier().VerifyAttestation(attestations[0])
	require.NoError(t, err, "the delivery carries the platform's attestation of its sender")
	assert.Equal(t, "supervisor", att.Agent, "the sender is the agent whose run published the hop")
	assert.Equal(t, "reviewer", att.Receiver)
	assert.Equal(t, "reg-a", att.RegistryID)
}

// Across the upgrade that addressed hops to their receiver, against the real broker: a hop an earlier BFF
// published on the registry-only subject is still delivered, and a hop published now waits in the
// receiver's own queue, counted there and nowhere else, until it is delivered.
func TestAsyncHop_TheReceiversQueueHoldsItsHops(t *testing.T) {
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(15*time.Second))
	defer srv.Shutdown()

	bus, err := asyncbus.NewJetStream(context.Background(), asyncbus.JetStreamOptions{URL: srv.ClientURL()})
	require.NoError(t, err)
	defer func() { _ = bus.Close() }()

	var mu sync.Mutex
	var received []string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get("Ce-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer agent.Close()

	caller := mkParentRun("run-async-queue")
	s, signer, _ := newAsyncServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Coordinates work."),
		reviewerIn("reg-a"),
	)
	s.asyncPublisher = bus
	client, _ := redirectTo(agent)

	// A hop as an earlier BFF published it: registry-only subject, same platform headers.
	legacy := asyncbus.Message{
		ID: "msg-legacy", Subject: asyncbus.Subject("reg-a"), Data: []byte(`{}`),
		Headers: map[string]string{
			"Ce-Id": "msg-legacy", "Ce-Type": "reviewer", "Ce-Specversion": "1.0", "Ce-Source": "supervisor",
			headerAsyncNamespace: "team-ns", headerAsyncRegistry: "reg-a",
		},
	}
	require.NoError(t, bus.Publish(t.Context(), legacy))
	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Stopped before the broker connection closes (defers run last-in, first-out).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.StartAsyncDispatcher(ctx, AsyncDispatcherConfig{Subscriber: bus, Client: client})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 2
	}, 30*time.Second, 50*time.Millisecond, "both hops reach the receiver")
	mu.Lock()
	assert.ElementsMatch(t, []string{"msg-legacy", "msg-1"}, received, "each delivered once")
	mu.Unlock()

	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	waiting := func(durable string) int {
		cons, cErr := js.Consumer(context.Background(), jsname.Stream, durable)
		require.NoError(t, cErr)
		info, iErr := cons.Info(context.Background())
		require.NoError(t, iErr)
		return int(info.NumPending) + info.NumAckPending
	}
	require.Eventually(t, func() bool { return waiting(jsname.AgentDurable("team-ns", "reviewer")) == 0 },
		10*time.Second, 50*time.Millisecond, "the receiver's queue drains once its hops are delivered")
	assert.Zero(t, waiting(jsname.AgentDurable("team-ns", "supervisor")), "nothing waits for the sender")
}
