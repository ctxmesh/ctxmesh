package audit

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
)

func mutation(verb Verb, name, ns, rv string) AuditEntry {
	return AuditEntry{
		Timestamp: time.Now().UTC(), Verb: verb, Kind: "AgentDeployment",
		Name: name, Namespace: ns, Subject: "kubectl", ResourceVersion: rv,
	}
}

func TestPostgresSink_RecordDrainsToStore(t *testing.T) {
	store := auditlog.NewMemStore()
	sink := NewPostgresSink(store, logr.Discard())

	sink.Record(mutation(VerbCreate, "a", "ns1", "100"))
	sink.Record(mutation(VerbDelete, "a", "ns1", "105"))
	sink.drain() // synchronous flush (same-package)

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	// Newest-first + the controller shape mapped correctly.
	got := page.Items[0]
	assert.Equal(t, "controller", got.Source)
	assert.Equal(t, "delete", got.Action)
	assert.Equal(t, "AgentDeployment", got.ResourceKind)
	assert.Equal(t, "kubectl", got.Actor)
	assert.Equal(t, "105", got.ResourceVersion)
}

func TestPostgresSink_IdempotentAcrossReplicas(t *testing.T) {
	store := auditlog.NewMemStore()
	sink := NewPostgresSink(store, logr.Discard())

	// The SAME mutation (same resourceVersion) observed on 3 replicas → 3 Records → one row.
	e := mutation(VerbUpdate, "a", "ns1", "200")
	sink.Record(e)
	sink.Record(e)
	sink.Record(e)
	sink.drain()

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	assert.Len(t, page.Items, 1, "cross-replica duplicate observations collapse to one row")
}

func TestPostgresSink_RecordNeverBlocksAndCountsDrops(t *testing.T) {
	store := auditlog.NewMemStore()
	sink := NewPostgresSink(store, logr.Discard())

	// Fill the buffer + overflow WITHOUT a drainer running — Record must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < pgSinkBuffer+50; i++ {
			sink.Record(mutation(VerbCreate, "a", "ns1", time.Now().Format(time.RFC3339Nano)+string(rune(i))))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked when the queue was full — it must drop, never block the informer loop")
	}
	assert.Positive(t, sink.Dropped(), "overflow entries are dropped + counted")
}

func TestPostgresSink_StartStopsOnContextCancel(t *testing.T) {
	sink := NewPostgresSink(auditlog.NewMemStore(), logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sink.Start(ctx) }()
	sink.Record(mutation(VerbCreate, "a", "ns1", "1"))
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return on context cancel")
	}
	assert.False(t, sink.NeedLeaderElection(), "the sink runs on every replica, never leader-elected")
}

func TestMultiSink_FansToEverySink(t *testing.T) {
	var a, b int
	sink := MultiSink{
		SinkFunc(func(AuditEntry) { a++ }),
		SinkFunc(func(AuditEntry) { b++ }),
	}
	sink.Record(mutation(VerbCreate, "a", "ns1", "1"))
	sink.Record(mutation(VerbDelete, "a", "ns1", "2"))
	assert.Equal(t, 2, a)
	assert.Equal(t, 2, b)
}
