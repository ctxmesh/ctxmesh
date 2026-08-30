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

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// newHandoffDelegate builds a delegateServer with a fixed roster so handoff's fail-fast membership check
// has a boundary to enforce.
func newHandoffDelegate(t *testing.T, client spawnClient, roster []string) *delegateServer {
	t.Helper()
	mr := miniredis.RunT(t)
	guard := NewSpawnGuard(newRedisSpawnStore(mr.Addr()))
	cfg := delegateConfig{SelfName: "planner", Namespace: "team-ns", Scope: "t1", Roster: roster, Budget: openBudget}
	return newDelegateServer(cfg, guard, client, nil)
}

func callHandoff(t *testing.T, ds *delegateServer, capToken string, body handoffRequest) handoffResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/handoff", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	ds.handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "handoff always 200s; the outcome is in the body")
	var resp handoffResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// TestHandoff_HappyPath — a roster-member target: resolve B's ksvc URL → relay to the BFF → the transfer
// outcome returns (ok=true). NO await, NO spawn-guard slot (it is a transfer, not a delegation).
func TestHandoff_HappyPath(t *testing.T) {
	fc := &fakeSpawnClient{handoffRes: handoffResult{RunID: "hand-1", SourceRunID: "A-1", HandedOffTo: "billing"}}
	ds := newHandoffDelegate(t, fc, []string{"billing", "research"})

	resp := callHandoff(t, ds, "cap-token", handoffRequest{TargetAgent: "billing", Message: "refund needed"})

	assert.True(t, resp.OK)
	assert.Equal(t, "hand-1", resp.RunID)
	assert.Equal(t, "A-1", resp.SourceRun)
	assert.Equal(t, "billing", resp.HandedOffTo)
	assert.Equal(t, 1, fc.handoffs)
	assert.Equal(t, 0, fc.spawned, "a handoff is NOT a spawn (no delegation)")
	assert.Equal(t, "cap-token", fc.gotCap, "the run capability is relayed to the BFF")
	assert.Equal(t, "http://billing.team-ns.svc.cluster.local", fc.gotHandoffBody.TargetEndpoint,
		"roster ksvc URL resolved")
	assert.Equal(t, "refund needed", fc.gotHandoffBody.Message)
	assert.Nil(t, fc.gotHandoffBody.IncludeHistory,
		"a handoff with no include_history relays nil ⇒ the BFF defaults to replay (unchanged)")
}

// TestHandoff_RelaysIncludeHistory — the m83.6 input filter: the SDK's include_history rides verbatim
// through the launcher into the BFF handoff body (a *bool: nil when absent, an explicit false when the
// author opted to hand off with a summary). The launcher does not interpret it — it just relays.
func TestHandoff_RelaysIncludeHistory(t *testing.T) {
	falseVal := false
	trueVal := true
	for _, tc := range []struct {
		name string
		in   *bool
	}{
		{"explicit-false-skips-replay", &falseVal},
		{"explicit-true-replays", &trueVal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeSpawnClient{handoffRes: handoffResult{RunID: "hand-1"}}
			ds := newHandoffDelegate(t, fc, []string{"billing"})

			resp := callHandoff(t, ds, "cap", handoffRequest{
				TargetAgent:    "billing",
				Message:        "summary of the thread so far",
				IncludeHistory: tc.in,
			})

			assert.True(t, resp.OK)
			require.NotNil(t, fc.gotHandoffBody.IncludeHistory, "include_history relayed to the BFF")
			assert.Equal(t, *tc.in, *fc.gotHandoffBody.IncludeHistory, "relayed verbatim")
		})
	}
}

// TestHandoff_NonMemberTargetRefused — a target NOT in the roster is refused fail-fast at the launcher,
// WITHOUT any BFF hop (the trust boundary — same as delegate_to). The SDK sees an honest ok=false.
func TestHandoff_NonMemberTargetRefused(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newHandoffDelegate(t, fc, []string{"billing", "research"})

	resp := callHandoff(t, ds, "cap", handoffRequest{TargetAgent: "attacker-agent"})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "not a member")
	assert.Equal(t, 0, fc.handoffs, "a non-member target never reaches the BFF")
}

// TestHandoff_MissingCapIsRefused — the handoff needs the authenticated run's capability to relay.
func TestHandoff_MissingCapIsRefused(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newHandoffDelegate(t, fc, []string{"billing"})

	resp := callHandoff(t, ds, "", handoffRequest{TargetAgent: "billing"})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "no run capability")
	assert.Equal(t, 0, fc.handoffs)
}

// TestHandoff_MissingTargetIsRefused — an empty target is rejected before any hop.
func TestHandoff_MissingTargetIsRefused(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newHandoffDelegate(t, fc, []string{"billing"})

	resp := callHandoff(t, ds, "cap", handoffRequest{TargetAgent: ""})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "required")
	assert.Equal(t, 0, fc.handoffs)
}

// TestHandoff_BFFFailureIsToolError — a BFF rejection comes back as an honest ok=false, not a crash.
func TestHandoff_BFFFailureIsToolError(t *testing.T) {
	fc := &fakeSpawnClient{handoffErr: assertErr("the source run has already finished")}
	ds := newHandoffDelegate(t, fc, []string{"billing"})

	resp := callHandoff(t, ds, "cap", handoffRequest{TargetAgent: "billing"})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "handoff failed")
	assert.Equal(t, 1, fc.handoffs)
}

// TestHandoff_EmptyRosterAdmitsAny — an unconfigured/legacy launcher (no roster env) admits any target;
// the BFF is still the authoritative gate. Proves inRoster's empty-roster branch.
func TestHandoff_EmptyRosterAdmitsAny(t *testing.T) {
	fc := &fakeSpawnClient{handoffRes: handoffResult{RunID: "hand-2"}}
	ds := newHandoffDelegate(t, fc, nil) // empty roster

	resp := callHandoff(t, ds, "cap", handoffRequest{TargetAgent: "anyone"})

	assert.True(t, resp.OK, "an empty roster admits any target (the BFF is authoritative)")
	assert.Equal(t, 1, fc.handoffs)
}

// TestParseRosterNames — DELEGATE_ROSTER (a JSON array of {name,description}) yields the member names.
func TestParseRosterNames(t *testing.T) {
	names := parseRosterNames(`[{"name":"billing","description":"handles billing"},{"name":"research"}]`)
	assert.Equal(t, []string{"billing", "research"}, names)
	assert.Nil(t, parseRosterNames(""), "empty env → nil (admit-any at the launcher)")
	assert.Nil(t, parseRosterNames("not json"), "malformed env → nil")
}

// assertErr is a tiny error type for the BFF-failure test (avoids importing errors just for New).
type assertErr string

func (e assertErr) Error() string { return string(e) }
