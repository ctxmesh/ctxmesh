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

package deliveryclaim_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/deliveryclaim"
)

func newValkey(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// One delivery holds a message at a time; a failed one lets the next through; a succeeded one makes every
// later delivery a duplicate.
func TestClaim_Lifecycle(t *testing.T) {
	_, rdb := newValkey(t)
	ctx := context.Background()

	st, err := deliveryclaim.Claim(ctx, rdb, "k", "first", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Claimed, st, "the first delivery claims the message")

	st, err = deliveryclaim.Claim(ctx, rdb, "k", "second", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.InFlight, st, "a concurrent duplicate finds it in flight")

	require.NoError(t, deliveryclaim.Release(ctx, rdb, "k", "second"))
	st, err = deliveryclaim.Claim(ctx, rdb, "k", "third", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.InFlight, st, "only the holder can release a claim")

	require.NoError(t, deliveryclaim.Release(ctx, rdb, "k", "first"))
	st, err = deliveryclaim.Claim(ctx, rdb, "k", "retry", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Claimed, st, "a released message is processed again")

	require.NoError(t, deliveryclaim.Complete(ctx, rdb, "k", 10*time.Minute))
	st, err = deliveryclaim.Claim(ctx, rdb, "k", "redelivery", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Done, st, "a processed message is a duplicate")
	require.NoError(t, deliveryclaim.Release(ctx, rdb, "k", "retry"))
	st, err = deliveryclaim.Claim(ctx, rdb, "k", "redelivery", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Done, st, "a late release does not undo a completion")
}

// A claim whose holder died is not held forever: once it expires the message is processed again. The done
// marker carries its own, longer window.
func TestClaim_Expiry(t *testing.T) {
	mr, rdb := newValkey(t)
	ctx := context.Background()

	_, err := deliveryclaim.Claim(ctx, rdb, "k", "crashed", 2*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, mr.TTL("k"))
	mr.FastForward(2*time.Minute + time.Second)
	st, err := deliveryclaim.Claim(ctx, rdb, "k", "retry", 2*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Claimed, st, "an expired claim lets the retry through")

	require.NoError(t, deliveryclaim.Complete(ctx, rdb, "k", 10*time.Minute))
	assert.Equal(t, 10*time.Minute, mr.TTL("k"), "completion replaces the claim's window with the dedupe window")
}

// An id recorded by the seen-set that preceded claims reads as processed.
func TestClaim_EarlierSeenMarkerIsDone(t *testing.T) {
	mr, rdb := newValkey(t)
	require.NoError(t, mr.Set("k", "1"))
	st, err := deliveryclaim.Claim(context.Background(), rdb, "k", "t", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Done, st)
}

// A store that cannot answer is an error, never a state: the consumer fails closed on it.
func TestClaim_StoreDownIsAnError(t *testing.T) {
	mr, rdb := newValkey(t)
	mr.Close()
	_, err := deliveryclaim.Claim(context.Background(), rdb, "k", "t", time.Minute)
	require.Error(t, err)
}
