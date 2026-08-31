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
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real publisher SETs `run:{ns}:{id}:control = cancel` with a bounded TTL — the exact key the
// state-layer proxy's /control endpoint reads back (statelayer.controlKey). If either side's key format
// drifts, this test (and the statelayer control test) fail loudly. The NAMESPACE is in the key so the
// proxy can scope the read to the caller's own namespace (M142.3, C15).
func TestRedisRunControlPublisher_SetsCancelMarkerWithTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	pub := NewRedisRunControlPublisher(mr.Addr())

	require.NoError(t, pub.Publish(context.Background(), "team-ns", "run-abc", controlVerbCancel))

	got, err := mr.Get("run:team-ns:run-abc:control")
	require.NoError(t, err)
	assert.Equal(t, "cancel", got, "the marker verb is exactly \"cancel\"")

	ttl := mr.TTL("run:team-ns:run-abc:control")
	assert.Positive(t, ttl, "the marker is set with a TTL so it self-expires (never lingers forever)")
	assert.LessOrEqual(t, ttl, controlMarkerTTL, "the TTL is bounded by controlMarkerTTL")
}

// publishCancelMarker via the Server: with a wired publisher the cancel marker lands in Valkey.
func TestPublishCancelMarker_WithPublisher_SetsMarker(t *testing.T) {
	mr := miniredis.RunT(t)
	s := &Server{runControl: NewRedisRunControlPublisher(mr.Addr()), log: logr.Discard()}

	s.publishCancelMarker(context.Background(), "team-ns", "run-xyz")

	got, err := mr.Get("run:team-ns:run-xyz:control")
	require.NoError(t, err)
	assert.Equal(t, "cancel", got)
}

// A NIL publisher (STATELAYER_ADDR unset) degrades to today's soft cancel: publishCancelMarker is a no-op
// and NEVER panics — the durable status flip (done by the caller) remains the authoritative cancel.
func TestPublishCancelMarker_NilPublisher_NoOp(t *testing.T) {
	s := &Server{runControl: nil, log: logr.Discard()}
	assert.NotPanics(t, func() {
		s.publishCancelMarker(context.Background(), "team-ns", "run-none")
	}, "a nil publisher must degrade to a soft cancel, never panic")
}
