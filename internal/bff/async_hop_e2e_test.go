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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/asyncbus"
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
	var received []string
	delivered := make(chan struct{}, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get("Ce-Id"))
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
		cap0("team-ns", "reviewer", "reg-a", "Reviews things."),
	)
	s.asyncPublisher = bus
	s.agentURL = func(string, string) string { return agent.URL }

	// (1) PUBLISH with NO dispatcher running — the hop has nowhere to go yet and must not be lost.
	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// (2) NOW start the dispatcher — the restart case: a hop published while the control plane's
	// consumer was down is delivered once it comes back.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartAsyncDispatcher(ctx, AsyncDispatcherConfig{Subscriber: bus, Client: agent.Client()})

	select {
	case <-delivered:
	case <-time.After(30 * time.Second):
		t.Fatal("the hop published before the dispatcher started was never delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"msg-1"}, received,
		"the target agent receives the CloudEvent, identified by the same messageId it was published with")
}
