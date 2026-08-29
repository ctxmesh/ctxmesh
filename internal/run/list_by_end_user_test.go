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

package run

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkEndUserRun creates a run owned by caller at (ns, agent) with a given updatedAt (so newest-first
// ordering is observable) and returns its id.
func mkEndUserRun(t *testing.T, s Store, caller, ns, agent string, updated time.Time) string {
	t.Helper()
	id := "r-" + agent + "-" + caller + "-" + updated.Format("150405.000")
	r := New(id, ns, agent, []byte(`{"input":"x"}`), "", updated)
	r.CallerUsername = caller
	r.CreatedAt = updated
	r.UpdatedAt = updated
	require.NoError(t, s.Create(r))
	return id
}

// TestListByEndUser_OwnershipAndHostScoping proves the my-runs list is scoped to BOTH the caller
// principal AND the (namespace, agent) host — an end-user sees only their own runs at the agent they
// are chatting with, never another principal's and never their own runs on a different agent (M137/EU1c).
func TestListByEndUser_OwnershipAndHostScoping(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	const alice = "oidc:https://idp.example.com#alice"
	const bob = "oidc:https://idp.example.com#bob"
	t0 := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	aliceOld := mkEndUserRun(t, s, alice, "eu-tenant", "chatbot", t0)
	aliceNew := mkEndUserRun(t, s, alice, "eu-tenant", "chatbot", t0.Add(2*time.Minute))
	_ = mkEndUserRun(t, s, bob, "eu-tenant", "chatbot", t0.Add(time.Minute))     // another principal
	_ = mkEndUserRun(t, s, alice, "eu-tenant", "other-agent", t0.Add(time.Hour)) // alice on a DIFFERENT agent

	got, err := s.ListByEndUser(ctx, alice, "eu-tenant", "chatbot", 0)
	require.NoError(t, err)
	require.Len(t, got, 2, "only alice's runs at chatbot (not bob's, not alice's other-agent run)")
	// Newest-updated first.
	assert.Equal(t, aliceNew, got[0].ID)
	assert.Equal(t, aliceOld, got[1].ID)

	// Bob sees only his own.
	bobRuns, err := s.ListByEndUser(ctx, bob, "eu-tenant", "chatbot", 0)
	require.NoError(t, err)
	require.Len(t, bobRuns, 1)

	// A different agent host yields alice's other-agent run only.
	otherRuns, err := s.ListByEndUser(ctx, alice, "eu-tenant", "other-agent", 0)
	require.NoError(t, err)
	require.Len(t, otherRuns, 1)
}

// TestListByEndUser_FailClosedAndLimit proves the query is fail-closed on a blank identity/host (never a
// list-all) and that limit bounds the page.
func TestListByEndUser_FailClosedAndLimit(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	const alice = "oidc:https://idp.example.com#alice"
	t0 := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		mkEndUserRun(t, s, alice, "eu-tenant", "chatbot", t0.Add(time.Duration(i)*time.Minute))
	}

	// Fail-closed: any blank component returns nothing (never all runs).
	for _, tc := range []struct{ caller, ns, agent string }{
		{"", "eu-tenant", "chatbot"},
		{alice, "", "chatbot"},
		{alice, "eu-tenant", ""},
	} {
		got, err := s.ListByEndUser(ctx, tc.caller, tc.ns, tc.agent, 0)
		require.NoError(t, err)
		assert.Empty(t, got, "blank identity/host must never list runs")
	}

	// Limit bounds the page (newest first).
	got, err := s.ListByEndUser(ctx, alice, "eu-tenant", "chatbot", 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}
