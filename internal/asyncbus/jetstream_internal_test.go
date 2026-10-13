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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pool never runs more than its size at once, and runs that many when there is work for them.
func TestWorkerPool_RunsAtMostItsSize(t *testing.T) {
	const size = 3
	pool := newWorkerPool(size)
	var running, most atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 10)

	go func() {
		for range 10 {
			pool.run(func() {
				now := running.Add(1)
				for {
					seen := most.Load()
					if now <= seen || most.CompareAndSwap(seen, now) {
						break
					}
				}
				started <- struct{}{}
				<-release
				running.Add(-1)
			})
		}
	}()

	for range size {
		<-started
	}
	select {
	case <-started:
		t.Fatal("a fourth function started while three were running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for range 10 - size {
		<-started
	}
	pool.wait()
	assert.Equal(t, int32(size), most.Load(), "the pool ran as many at once as it may, and no more")
}

// A connection the server closes for good is reported, and a running Subscribe returns instead of
// staying attached to nothing. The close is provoked the way an oversized subject did before Publish
// refused one: the server answers a control line over its limit with an error that ends the connection.
func TestSubscribe_ReturnsWhenTheConnectionIsClosed(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(15*time.Second))
	t.Cleanup(srv.Shutdown)

	bus, err := NewJetStream(context.Background(), JetStreamOptions{URL: srv.ClientURL()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	require.NoError(t, bus.Err())

	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(t.Context(), Subject("reg-a"), "closing", func(context.Context, Message) error { return nil })
	}()
	require.Eventually(t, func() bool {
		_, cErr := bus.js.Consumer(context.Background(), streamName, "closing")
		return cErr == nil
	}, 10*time.Second, 20*time.Millisecond)

	_ = bus.nc.Publish(strings.Repeat("a", 5000), nil) // past the server's 4096-byte control line

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrConnectionClosed, "Subscribe returns once the connection is gone")
	case <-time.After(15 * time.Second):
		t.Fatal("Subscribe stayed attached to a closed connection")
	}
	require.ErrorIs(t, bus.Err(), ErrConnectionClosed)
	assert.Contains(t, strings.ToLower(bus.Err().Error()), "control line", "the server's reason is kept")
	assert.Error(t, bus.Publish(context.Background(), Message{Subject: Subject("reg-a"), Data: []byte("{}")}),
		"nothing can be published on it")
}

// Only a server known to be older than 2.10 earns the upgrade hint.
func TestServerAtLeast(t *testing.T) {
	for version, want := range map[string]bool{
		"2.9.25": false, "v2.9.1": false, "1.4.0": false,
		"2.10.0": true, "2.11.4": true, "2.14.6": true, "3.0.0": true,
		"": true, "unknown": true,
	} {
		assert.Equal(t, want, serverAtLeast(version, 2, 10), version)
	}
}

// A handler that runs past the ack wait still has its message delivered once: Subscribe reports progress
// while the handler works, so the broker never hands the message to another worker meanwhile.
func TestSubscribe_ProgressKeepsALongHandlersMessage(t *testing.T) {
	saved := ackWait
	ackWait = 2 * time.Second
	t.Cleanup(func() { ackWait = saved })

	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(15*time.Second))
	t.Cleanup(srv.Shutdown)
	bus, err := NewJetStream(context.Background(), JetStreamOptions{URL: srv.ClientURL()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	require.NoError(t, bus.Publish(context.Background(), Message{ID: "long-1", Subject: Subject("reg-a"), Data: []byte("{}")}))
	var calls atomic.Int32
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(ctx, Subject("reg-a"), "long", func(context.Context, Message) error {
			if calls.Add(1) == 1 {
				time.Sleep(3 * ackWait)
				close(finished)
			}
			return nil
		})
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the long handler never finished")
	}
	time.Sleep(ackWait) // a redelivery made during the long run would be handled by now
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int32(1), calls.Load(), "a message whose handler is still working is not redelivered")
}
