package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// fakeTRStore is a toolregistry.Store whose Get is scriptable + counted; the other
// methods are unused by the runtime read path.
type fakeTRStore struct {
	getFn func(ns, name string) (*toolregistry.ToolRegistry, error)
	calls int
}

func (f *fakeTRStore) Get(_ context.Context, ns, name string) (*toolregistry.ToolRegistry, error) {
	f.calls++
	return f.getFn(ns, name)
}

func (f *fakeTRStore) List(context.Context, controlplane.ListOptions) (
	controlplane.Page[toolregistry.ToolRegistry], error,
) {
	panic("unused")
}

func (f *fakeTRStore) Create(context.Context, toolregistry.ToolRegistry) (*toolregistry.ToolRegistry, error) {
	panic("unused")
}

func (f *fakeTRStore) Upsert(context.Context, toolregistry.ToolRegistry) (*toolregistry.ToolRegistry, error) {
	panic("unused")
}
func (f *fakeTRStore) Delete(context.Context, string, string) error { panic("unused") }
func (f *fakeTRStore) ListCatalog(context.Context, string, []string) ([]toolregistry.ToolRegistry, error) {
	panic("unused")
}

func TestToolRegistrySource_ProjectsAndCaches(t *testing.T) {
	ctx := context.Background()
	store := &fakeTRStore{getFn: func(ns, name string) (*toolregistry.ToolRegistry, error) {
		return &toolregistry.ToolRegistry{
			Namespace: ns, Name: name,
			Annotations: map[string]string{"agents.ctxmesh.ai/mcp-auth-type": "oauth"},
			Labels:      map[string]string{"mcp.ctxmesh.ai/scope": "org"},
		}, nil
	}}
	s := newToolRegistrySource(store, time.Minute, logr.Discard())

	tr, err := s.getTR(ctx, "prod", "scalekit")
	require.NoError(t, err)
	require.NotNil(t, tr)
	assert.Equal(t, "oauth", tr.Annotations["agents.ctxmesh.ai/mcp-auth-type"])
	assert.Equal(t, "org", tr.Labels["mcp.ctxmesh.ai/scope"])

	// Second call within TTL is served from cache — the store is not hit again.
	_, err = s.getTR(ctx, "prod", "scalekit")
	require.NoError(t, err)
	assert.Equal(t, 1, store.calls, "a fresh cache hit must not touch the store")
}

func TestToolRegistrySource_NotFoundIsNilAndNegativeCached(t *testing.T) {
	ctx := context.Background()
	store := &fakeTRStore{getFn: func(string, string) (*toolregistry.ToolRegistry, error) {
		return nil, controlplane.ErrNotFound
	}}
	s := newToolRegistrySource(store, time.Minute, logr.Discard())

	tr, err := s.getTR(ctx, "prod", "nope")
	require.NoError(t, err)
	assert.Nil(t, tr, "a missing server is (nil, nil) — the callers treat it conservatively")

	_, err = s.getTR(ctx, "prod", "nope")
	require.NoError(t, err)
	assert.Equal(t, 1, store.calls, "a not-found is negative-cached")
}

// The blast-radius mitigation: a store error AFTER a value was cached serves the
// stale value (no error), so a Postgres blip is invisible for a recently-read server.
func TestToolRegistrySource_ServesStaleOnError(t *testing.T) {
	ctx := context.Background()
	fail := false
	store := &fakeTRStore{getFn: func(ns, name string) (*toolregistry.ToolRegistry, error) {
		if fail {
			return nil, errors.New("postgres down")
		}
		return &toolregistry.ToolRegistry{
			Namespace: ns, Name: name,
			Annotations: map[string]string{"agents.ctxmesh.ai/mcp-auth-type": "oauth"},
		}, nil
	}}
	s := newToolRegistrySource(store, time.Nanosecond, logr.Discard()) // TTL ~0 ⇒ always re-fetch

	// Prime the cache.
	_, err := s.getTR(ctx, "prod", "scalekit")
	require.NoError(t, err)

	// Now Postgres is down — but the entry was cached, so the read still succeeds
	// with the last-known value (fail-safe).
	fail = true
	tr, err := s.getTR(ctx, "prod", "scalekit")
	require.NoError(t, err, "a store error must serve the last-known value, not fail")
	require.NotNil(t, tr)
	assert.Equal(t, "oauth", tr.Annotations["agents.ctxmesh.ai/mcp-auth-type"])
}

// A store error with NO prior cache entry propagates — the callers then degrade to
// "not OAuth" (fail-safe, no credential leak), and the error is diagnosable.
func TestToolRegistrySource_ErrorPropagatesWithoutCache(t *testing.T) {
	ctx := context.Background()
	store := &fakeTRStore{getFn: func(string, string) (*toolregistry.ToolRegistry, error) {
		return nil, errors.New("postgres down")
	}}
	s := newToolRegistrySource(store, time.Minute, logr.Discard())

	_, err := s.getTR(ctx, "prod", "never-seen")
	require.Error(t, err, "a store error with no cached fallback must propagate (diagnosable)")
}
