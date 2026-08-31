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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// fakeConfigReader is a canned onlineConfigReader for the cpDB-backed resolver tests: it returns a fixed
// (config, found) or an error, driving each ResolveOnline branch with no live cpDB.
type fakeConfigReader struct {
	cfg   onlinescore.OnlineConfig
	found bool
	err   error
}

func (f fakeConfigReader) GetOnlineConfig(context.Context, string, string) (onlinescore.OnlineConfig, bool, error) {
	return f.cfg, f.found, f.err
}

// Test: an ENABLED cpDB config row resolves to the parsed policy (the controller published it).
func TestDBResolveOnline_EnabledRow(t *testing.T) {
	t.Parallel()

	r := NewDBOnlineConfigResolver(fakeConfigReader{
		cfg: onlinescore.OnlineConfig{
			Namespace:       "default",
			AgentName:       "foo",
			Enabled:         true,
			SampleRate:      0.05,
			MaxScoredPerDay: 25,
			Window:          24 * time.Hour,
			MinSamples:      10,
		},
		found: true,
	})

	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	require.NotNil(t, got, "an enabled config row resolves to a policy")
	assert.InDelta(t, 0.05, got.SampleRate, 1e-9)
	assert.Equal(t, 25, got.MaxScoredPerDay)
	assert.Equal(t, 24*time.Hour, got.Window)
	assert.Equal(t, 10, got.MinSamples)
}

// Test: NO row (never published) ⇒ (nil, nil) — the worker uses process defaults (judge OFF).
func TestDBResolveOnline_NoRow(t *testing.T) {
	t.Parallel()

	r := NewDBOnlineConfigResolver(fakeConfigReader{found: false})
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got, "no row ⇒ (nil, nil): process defaults (judge OFF)")
}

// Test: a DISABLED row (enabled=false — the controller cleared the policy) ⇒ (nil, nil), judge OFF.
func TestDBResolveOnline_DisabledRow(t *testing.T) {
	t.Parallel()

	r := NewDBOnlineConfigResolver(fakeConfigReader{
		cfg:   onlinescore.OnlineConfig{Namespace: "default", AgentName: "foo", Enabled: false, SampleRate: 1.0, MaxScoredPerDay: 5},
		found: true,
	})
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got, "an explicitly disabled row ⇒ (nil, nil): judge OFF (the fail-safe)")
}

// Test: a store read error ⇒ (nil, err) — the worker logs it and falls back to defaults (never fabricates).
func TestDBResolveOnline_StoreError(t *testing.T) {
	t.Parallel()

	r := NewDBOnlineConfigResolver(fakeConfigReader{err: errors.New("cpDB down")})
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.Error(t, err, "a store read error surfaces so the worker falls back to defaults for this agent")
	assert.Nil(t, got)
}

// Test: a nil store ⇒ (nil, nil) — defensive (the resolver never nil-derefs).
func TestDBResolveOnline_NilStore(t *testing.T) {
	t.Parallel()

	r := NewDBOnlineConfigResolver(nil)
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// Test: the resolver reads what the store round-trips (mem-store integration — the worker's real read path).
func TestDBResolveOnline_MemStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := onlinescore.NewMemStore()
	ctx := context.Background()
	require.NoError(t, store.UpsertOnlineConfig(ctx, onlinescore.OnlineConfig{
		Namespace:       "default",
		AgentName:       "foo",
		Enabled:         true,
		SampleRate:      0.5,
		MaxScoredPerDay: 3,
		Window:          2 * time.Hour,
		MinSamples:      4,
	}))

	r := NewDBOnlineConfigResolver(store)
	got, err := r.ResolveOnline(ctx, "default", "foo")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.InDelta(t, 0.5, got.SampleRate, 1e-9)
	assert.Equal(t, 3, got.MaxScoredPerDay)
	assert.Equal(t, 2*time.Hour, got.Window)
	assert.Equal(t, 4, got.MinSamples)

	// A cleared row ⇒ (nil, nil): the worker defaults to judge OFF.
	require.NoError(t, store.DeleteOnlineConfig(ctx, "default", "foo"))
	got, err = r.ResolveOnline(ctx, "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got, "after the controller clears the row, the resolver returns no policy (judge OFF)")
}
