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

package oauthpending_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/oauthpending"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/pgtest"
)

// eachStore runs one contract against the in-memory twin and, when CONTROLPLANE_TEST_DSN is set, the
// Postgres store in its own schema (so another package's test cannot truncate this one's rows).
func eachStore(t *testing.T, fn func(t *testing.T, s oauthpending.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, oauthpending.NewMemStore()) })
	t.Run("postgres", func(t *testing.T) {
		db := pgtest.Open(t, "oauthpending_conformance")
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_pending (LIKE public.oauth_pending INCLUDING ALL)`)
		require.NoError(t, err)
		_, err = db.Exec(`TRUNCATE oauth_pending`)
		require.NoError(t, err)
		fn(t, oauthpending.NewPostgresStore(db))
	})
}

func handle(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}

var base = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func TestTakeIsSingleUse(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte(`{"v":1}`), base.Add(time.Minute)))

		got, ok, err := s.Take(ctx, oauthpending.KindFlow, h, base)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, `{"v":1}`, string(got))

		_, ok, err = s.Take(ctx, oauthpending.KindFlow, h, base)
		require.NoError(t, err)
		assert.False(t, ok, "a second take finds nothing")
	})
}

func TestConcurrentTakersGetOneRecord(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("p"), base.Add(time.Minute)))

		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				_, ok, err := s.Take(ctx, oauthpending.KindTicket, h, base)
				assert.NoError(t, err)
				if ok {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load())
	})
}

func TestKindsAreSeparate(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("ticket"), base.Add(time.Minute)))

		_, ok, err := s.Take(ctx, oauthpending.KindFlow, h, base)
		require.NoError(t, err)
		assert.False(t, ok, "a ticket is not a flow")

		got, ok, err := s.Take(ctx, oauthpending.KindTicket, h, base)
		require.NoError(t, err)
		require.True(t, ok, "taking the wrong kind did not consume the ticket")
		assert.Equal(t, "ticket", string(got))
	})
}

func TestExpiryIsEnforcedOnRead(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		h := handle(t)
		exp := base.Add(time.Minute)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("p"), exp))

		_, ok, err := s.Take(ctx, oauthpending.KindFlow, h, exp)
		require.NoError(t, err)
		assert.False(t, ok, "a record is expired at its expiry instant")

		_, ok, err = s.Take(ctx, oauthpending.KindFlow, h, base)
		require.NoError(t, err)
		assert.False(t, ok, "the expired take removed the record")
	})
}

func TestDeleteExpiredKeepsLiveRecords(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		old, live := handle(t), handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, old, []byte("old"), base))
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, live, []byte("live"), base.Add(time.Hour)))

		n, err := s.DeleteExpired(ctx, base)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)

		n, err = s.DeleteExpired(ctx, base)
		require.NoError(t, err)
		assert.Zero(t, n, "nothing left to sweep")

		got, ok, err := s.Take(ctx, oauthpending.KindTicket, live, base)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "live", string(got))
	})
}

func TestRefusesWeakHandlesAndUnknownKinds(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		err := s.Put(ctx, oauthpending.KindFlow, "short", []byte("p"), base.Add(time.Minute))
		require.ErrorIs(t, err, controlplane.ErrInvalid)
		err = s.Put(ctx, oauthpending.Kind("other"), handle(t), []byte("p"), base.Add(time.Minute))
		require.ErrorIs(t, err, controlplane.ErrInvalid)
		_, _, err = s.Take(ctx, oauthpending.KindFlow, "short", base)
		require.ErrorIs(t, err, controlplane.ErrInvalid)
	})
}

func TestDuplicatePutIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s oauthpending.Store) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("a"), base.Add(time.Minute)))
		require.Error(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("b"), base.Add(time.Minute)))
	})
}

// TestNothingReadableAtRest reads the Postgres row directly: neither the handle nor the payload
// appears in it.
func TestNothingReadableAtRest(t *testing.T) {
	db := pgtest.Open(t, "oauthpending_atrest")
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_pending (LIKE public.oauth_pending INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = db.Exec(`TRUNCATE oauth_pending`)
	require.NoError(t, err)
	s := oauthpending.NewPostgresStore(db)

	ctx := context.Background()
	h := handle(t)
	const secret = "caller-bearer-token-SECRET-do-not-store"
	require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte(`{"callerToken":"`+secret+`"}`), base.Add(time.Minute)))

	var (
		id, kind string
		sealed   []byte
	)
	require.NoError(t, db.QueryRow(`SELECT id, kind, sealed FROM oauth_pending`).Scan(&id, &kind, &sealed))
	assert.Equal(t, "flow", kind)
	assert.NotContains(t, id, h)
	assert.False(t, bytes.Contains(sealed, []byte(h)), "the handle is not stored")
	assert.False(t, bytes.Contains(sealed, []byte(secret)), "the payload is sealed")
	assert.False(t, strings.Contains(string(sealed), "callerToken"))

	got, ok, err := s.Take(ctx, oauthpending.KindFlow, h, base)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, string(got), secret, "the handle opens its own record")
}
