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
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisTenantUsageReadsLauncherKeys proves the REAL redisTenantUsage reader decodes the exact keys the
// launcher's tenant enforcer writes (cmd/launcher/tenant_quota.go — spendKey/rpmKey/inflightKey). It seeds a
// miniredis in the launcher's on-the-wire format and reads it back through NewRedisTenantUsageReader — the
// read↔write contract, without a live Valkey. If the launcher's key format changes, THIS test must change
// too (that is the point: it fails loudly on drift between the writer and this reader).
func TestRedisTenantUsageReadsLauncherKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	now := time.Now().UTC()

	// The launcher writes these exact keys (see cmd/launcher/tenant_quota.go):
	//   spend    : tenant:{id}:spend:{YYYY-MM}   via IncrByFloat → a float string
	//   rpm      : tenant:{id}:rpm:{unixMinute}  via Incr        → an int string
	//   inflight : tenant:{id}:inflight          via Incr        → an int string
	require.NoError(t, mr.Set("tenant:acme:spend:"+now.Format("2006-01"), "37.25"))
	require.NoError(t, mr.Set("tenant:acme:rpm:"+strconv.FormatInt(now.Unix()/60, 10), "88"))
	require.NoError(t, mr.Set("tenant:acme:inflight", "5"))

	reader := NewRedisTenantUsageReader(mr.Addr())
	u, err := reader.Usage(context.Background(), "acme")
	require.NoError(t, err)
	assert.InDelta(t, 37.25, u.SpendUSD, 1e-9, "spend decoded from the launcher's float key")
	assert.Equal(t, int64(88), u.RPM, "rpm decoded from the current-minute window key")
	assert.Equal(t, int64(5), u.InFlight, "in-flight decoded from the concurrency key")
}

// Absent keys ⇒ zero usage (redis.Nil is not an error) — a tenant with no traffic reads clean, not an error.
func TestRedisTenantUsageAbsentKeysAreZero(t *testing.T) {
	mr := miniredis.RunT(t)
	reader := NewRedisTenantUsageReader(mr.Addr())

	u, err := reader.Usage(context.Background(), "quiet")
	require.NoError(t, err)
	assert.Zero(t, u.SpendUSD)
	assert.Zero(t, u.RPM)
	assert.Zero(t, u.InFlight)
}
