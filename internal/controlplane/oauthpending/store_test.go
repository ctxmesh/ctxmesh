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
	"database/sql"
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

// clockedStore is a Store under test plus a way to move its clock forward.
type clockedStore struct {
	oauthpending.Store
	// advance moves the store's clock forward by d. The Postgres store's clock is the database's,
	// which a test cannot move, so there it moves every stored deadline back by d instead: the
	// same thing as far as any record can tell.
	advance func(t *testing.T, d time.Duration)
}

// openPG opens the oauth_pending table in its own schema (so another package's test cannot
// truncate this one's rows), empty.
func openPG(t *testing.T, schema string) *sql.DB {
	t.Helper()
	db := pgtest.Open(t, schema)
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_pending (LIKE public.oauth_pending INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = db.Exec(`TRUNCATE oauth_pending`)
	require.NoError(t, err)
	return db
}

// eachStore runs one contract against the in-memory twin and, when CONTROLPLANE_TEST_DSN is set, the
// Postgres store.
func eachStore(t *testing.T, fn func(t *testing.T, s clockedStore)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) {
		cur := time.Now()
		fn(t, clockedStore{
			Store:   oauthpending.NewMemStoreWithClock(func() time.Time { return cur }),
			advance: func(_ *testing.T, d time.Duration) { cur = cur.Add(d) },
		})
	})
	t.Run("postgres", func(t *testing.T) {
		db := openPG(t, "oauthpending_conformance")
		fn(t, clockedStore{
			Store: oauthpending.NewPostgresStore(db),
			advance: func(t *testing.T, d time.Duration) {
				_, err := db.Exec(`UPDATE oauth_pending SET expires_at = expires_at - make_interval(secs => $1)`, d.Seconds())
				require.NoError(t, err)
			},
		})
	})
}

func handle(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestTakeIsSingleUse(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte(`{"v":1}`), time.Minute))

		got, ok, err := s.Take(ctx, oauthpending.KindFlow, h)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, `{"v":1}`, string(got))

		_, ok, err = s.Take(ctx, oauthpending.KindFlow, h)
		require.NoError(t, err)
		assert.False(t, ok, "a second take finds nothing")
	})
}

func TestConcurrentTakersGetOneRecord(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("p"), time.Minute))

		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				_, ok, err := s.Take(ctx, oauthpending.KindTicket, h)
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
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("ticket"), time.Minute))

		_, ok, err := s.Take(ctx, oauthpending.KindFlow, h)
		require.NoError(t, err)
		assert.False(t, ok, "a ticket is not a flow")

		got, ok, err := s.Take(ctx, oauthpending.KindTicket, h)
		require.NoError(t, err)
		require.True(t, ok, "taking the wrong kind did not consume the ticket")
		assert.Equal(t, "ticket", string(got))
	})
}

func TestExpiryIsEnforcedOnRead(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		h, live := handle(t), handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("p"), time.Minute))
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, live, []byte("live"), time.Hour))
		s.advance(t, time.Minute)

		_, ok, err := s.Take(ctx, oauthpending.KindFlow, h)
		require.NoError(t, err)
		assert.False(t, ok, "a record is expired at its expiry instant")

		s.advance(t, -time.Hour)
		_, ok, err = s.Take(ctx, oauthpending.KindFlow, h)
		require.NoError(t, err)
		assert.False(t, ok, "the expired take removed the record")

		got, ok, err := s.Take(ctx, oauthpending.KindFlow, live)
		require.NoError(t, err)
		require.True(t, ok, "a record inside its ttl is live")
		assert.Equal(t, "live", string(got))
	})
}

func TestDeleteExpiredKeepsLiveRecords(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		old, live := handle(t), handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, old, []byte("old"), time.Minute))
		require.NoError(t, s.Put(ctx, oauthpending.KindTicket, live, []byte("live"), time.Hour))

		n, err := s.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "nothing has expired yet")

		s.advance(t, time.Minute)
		n, err = s.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)

		n, err = s.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "nothing left to sweep")

		got, ok, err := s.Take(ctx, oauthpending.KindTicket, live)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "live", string(got))
	})
}

func TestRefusesWeakHandlesUnknownKindsAndNoTTL(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		err := s.Put(ctx, oauthpending.KindFlow, "short", []byte("p"), time.Minute)
		require.ErrorIs(t, err, controlplane.ErrInvalid)
		err = s.Put(ctx, oauthpending.Kind("other"), handle(t), []byte("p"), time.Minute)
		require.ErrorIs(t, err, controlplane.ErrInvalid)
		for _, ttl := range []time.Duration{0, -time.Second} {
			err = s.Put(ctx, oauthpending.KindFlow, handle(t), []byte("p"), ttl)
			require.ErrorIs(t, err, controlplane.ErrInvalid, "ttl %s", ttl)
		}
		_, _, err = s.Take(ctx, oauthpending.KindFlow, "short")
		require.ErrorIs(t, err, controlplane.ErrInvalid)
	})
}

func TestDuplicatePutIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s clockedStore) {
		ctx := context.Background()
		h := handle(t)
		require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("a"), time.Minute))
		require.Error(t, s.Put(ctx, oauthpending.KindFlow, h, []byte("b"), time.Minute))
	})
}

// TestPostgresExpiryIsTheDatabaseClock: the deadline a row carries is the database's now() plus
// the ttl, exactly, and the database's clock alone decides when it has passed. A replica whose own
// clock runs ahead therefore cannot expire, or sweep, a record another replica wrote.
func TestPostgresExpiryIsTheDatabaseClock(t *testing.T) {
	db := openPG(t, "oauthpending_dbclock")
	s := oauthpending.NewPostgresStore(db)
	ctx := context.Background()

	h := handle(t)
	require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("p"), 90*time.Second))
	var exact bool
	require.NoError(t, db.QueryRow(
		`SELECT expires_at - created_at = interval '90 seconds' FROM oauth_pending`).Scan(&exact))
	assert.True(t, exact, "the deadline is the database's now() plus the ttl")

	n, err := s.DeleteExpired(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a sweep removes nothing the database clock says is live")
	_, ok, err := s.Take(ctx, oauthpending.KindTicket, h)
	require.NoError(t, err)
	assert.True(t, ok)

	// A deadline one second behind the database clock has passed, whatever this process's clock says.
	h = handle(t)
	require.NoError(t, s.Put(ctx, oauthpending.KindTicket, h, []byte("p"), time.Hour))
	_, err = db.Exec(`UPDATE oauth_pending SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)
	_, ok, err = s.Take(ctx, oauthpending.KindTicket, h)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestNothingReadableAtRest reads the Postgres row directly: neither the handle nor the payload
// appears in it.
func TestNothingReadableAtRest(t *testing.T) {
	db := openPG(t, "oauthpending_atrest")
	s := oauthpending.NewPostgresStore(db)

	ctx := context.Background()
	h := handle(t)
	const secret = "caller-bearer-token-SECRET-do-not-store"
	require.NoError(t, s.Put(ctx, oauthpending.KindFlow, h, []byte(`{"callerToken":"`+secret+`"}`), time.Minute))

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

	got, ok, err := s.Take(ctx, oauthpending.KindFlow, h)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, string(got), secret, "the handle opens its own record")
}
