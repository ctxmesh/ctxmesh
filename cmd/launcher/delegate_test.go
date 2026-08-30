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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// fakeSpawnClient records the Spawn call + returns a programmed await result.
type fakeSpawnClient struct {
	spawned  int
	gotBody  bffSpawnBody
	gotCap   string
	subRunID string
	spawnErr error
	awaitRes spawnedRunResult
	awaitErr error

	// handoff (m67.6)
	handoffs       int
	gotHandoffBody bffHandoffBody
	handoffRes     handoffResult
	handoffErr     error
}

func (f *fakeSpawnClient) Spawn(_ context.Context, capToken string, body bffSpawnBody) (string, error) {
	f.spawned++
	f.gotCap = capToken
	f.gotBody = body
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	if f.subRunID == "" {
		f.subRunID = "sub-xyz"
	}
	return f.subRunID, nil
}

func (f *fakeSpawnClient) Await(_ context.Context, _, _ string) (spawnedRunResult, error) {
	return f.awaitRes, f.awaitErr
}

// Handoff records the handoff call + returns a programmed result (m67.6). Delegate tests never call it.
func (f *fakeSpawnClient) Handoff(_ context.Context, capToken string, body bffHandoffBody) (handoffResult, error) {
	f.handoffs++
	f.gotCap = capToken
	f.gotHandoffBody = body
	if f.handoffErr != nil {
		return handoffResult{}, f.handoffErr
	}
	res := f.handoffRes
	if res.RunID == "" {
		res.RunID = "hand-xyz"
	}
	if res.HandedOffTo == "" {
		res.HandedOffTo = body.TargetAgent
	}
	return res, nil
}

func newDelegate(t *testing.T, client spawnClient, budget SpawnBudget) *delegateServer {
	t.Helper()
	mr := miniredis.RunT(t)
	guard := NewSpawnGuard(newRedisSpawnStore(mr.Addr()))
	cfg := delegateConfig{SelfName: "planner", Namespace: "team-ns", Scope: "t1", Budget: budget}
	return newDelegateServer(cfg, guard, client, nil)
}

func callDelegate(
	t *testing.T, ds *delegateServer, capToken string, body delegateRequest, headers map[string]string,
) delegateResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/delegate", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ds.handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "delegate always 200s; the outcome is in the body")
	var resp delegateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func delegBody() delegateRequest {
	return delegateRequest{SubAgent: "researcher", Input: json.RawMessage(`"find it"`), Step: "1", CallID: "c1"}
}

var openBudget = SpawnBudget{MaxFanOut: 4, MaxSpawnDepth: 3, MaxTotalSpawns: 20}

// TestDelegate_HappyPath — admit → spawn → await(succeeded) → the answer returns as the tool result.
func TestDelegate_HappyPath(t *testing.T) {
	fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "succeeded", Answer: "the docs are at /x"}}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegBody(), nil)

	assert.True(t, resp.OK)
	assert.Equal(t, "the docs are at /x", resp.Answer)
	assert.Equal(t, 1, fc.spawned)
	assert.Equal(t, "cap-token", fc.gotCap, "the parent capability is relayed to the BFF")
	assert.Equal(t, "http://researcher.team-ns.svc.cluster.local", fc.gotBody.Endpoint, "roster ksvc URL resolved")
	assert.Equal(t, "c1", fc.gotBody.CallID)
}

// TestDelegate_GuardDenialIsToolError — a depth-exceeded delegation is refused WITHOUT spawning; the
// model sees an honest tool result (ok=false), never an HTTP error.
func TestDelegate_GuardDenialIsToolError(t *testing.T) {
	fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "succeeded"}}
	ds := newDelegate(t, fc, SpawnBudget{MaxFanOut: 4, MaxSpawnDepth: 2, MaxTotalSpawns: 20})

	// depth header 2 ⇒ childDepth 3 > maxSpawnDepth 2 → denied.
	resp := callDelegate(t, ds, "cap", delegBody(), map[string]string{headerSpawnDepth: "2"})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "refused")
	assert.Contains(t, resp.Error, "spawn_depth_exceeded")
	assert.Equal(t, 0, fc.spawned, "a denied delegation never spawns")
}

// TestDelegate_CycleDenied — delegating to an ancestor is refused (the spawn-ancestry cycle guard).
func TestDelegate_CycleDenied(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	body := delegBody()
	body.SubAgent = "planner" // the supervisor itself is in the ancestry (self is appended)
	resp := callDelegate(t, ds, "cap", body, map[string]string{headerSpawnPath: "planner"})
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "spawn_cycle_detected")
	assert.Equal(t, 0, fc.spawned)
}

// TestDelegate_SubRunFailedIsToolError — a failed sub-run surfaces its error as the tool result.
func TestDelegate_SubRunFailedIsToolError(t *testing.T) {
	fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "failed", Error: "the tool 404'd"}}
	ds := newDelegate(t, fc, openBudget)
	resp := callDelegate(t, ds, "cap", delegBody(), nil)
	assert.False(t, resp.OK)
	assert.Equal(t, "the tool 404'd", resp.Error)
	assert.Equal(t, "sub-xyz", resp.SubRun)
}

// TestDelegate_MissingCapIsRefused — no capability ⇒ no delegation.
func TestDelegate_MissingCapIsRefused(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	resp := callDelegate(t, ds, "", delegBody(), nil)
	assert.False(t, resp.OK)
	assert.Equal(t, 0, fc.spawned)
}

// TestDelegate_MissingFieldsIsRefused — an incomplete delegate body is refused before the guard.
func TestDelegate_MissingFieldsIsRefused(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	body := delegBody()
	body.CallID = ""
	resp := callDelegate(t, ds, "cap", body, nil)
	assert.False(t, resp.OK)
	assert.Equal(t, 0, fc.spawned)
}

// TestDelegate_SuspendSignalDepth0 — L7 (ADR 0091): at depth 0, a Suspend request budget-checks + resolves
// the target endpoint and returns a suspend-SIGNAL — no spawn, no blocking await. The SDK collects these
// and suspends once; the BFF worker creates the child at Endpoint inside the suspend transaction.
func TestDelegate_SuspendSignalDepth0(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	body := delegBody()
	body.Suspend = true

	resp := callDelegate(t, ds, "cap", body, nil) // no depth header ⇒ depth 0 (a root supervisor)

	assert.True(t, resp.OK)
	assert.True(t, resp.Suspend, "a depth-0 suspend request returns a suspend-signal")
	assert.Equal(t, "http://researcher.team-ns.svc.cluster.local", resp.Endpoint, "roster endpoint resolved")
	assert.Empty(t, resp.Answer)
	assert.Equal(t, 0, fc.spawned, "the suspend path never spawns or blocks — the BFF creates the child")
}

// TestDelegate_SuspendSignalAtDepth — depth-agnostic suspend (ADR 0108, M138): a Suspend request at
// depth>0 now returns a suspend-SIGNAL too (the depth-0 gate was lifted). A sub-run that is itself a
// supervisor budget-checks + resolves the endpoint and suspends — it does NOT spawn or block inline.
func TestDelegate_SuspendSignalAtDepth(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	body := delegBody()
	body.Suspend = true

	resp := callDelegate(t, ds, "cap", body, map[string]string{headerSpawnDepth: "1"}) // depth>0

	assert.True(t, resp.OK)
	assert.True(t, resp.Suspend, "a supervisor suspends at ANY depth now (nested suspend lifted)")
	assert.Equal(t, "http://researcher.team-ns.svc.cluster.local", resp.Endpoint, "roster endpoint resolved")
	assert.Empty(t, resp.Answer)
	assert.Equal(t, 0, fc.spawned, "the suspend path never spawns or blocks — the BFF creates the child")
}

// TestDelegate_SuspendStillGuarded — the suspend path is gated by the SAME admission guard: a depth-0
// suspend that would form a cycle (delegating to an ancestor) is refused as an honest tool result, never
// resolved into a suspend-signal.
func TestDelegate_SuspendStillGuarded(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)
	body := delegBody()
	body.SubAgent = "planner" // the supervisor itself — a cycle (self is in the ancestry)
	body.Suspend = true

	resp := callDelegate(t, ds, "cap", body, map[string]string{headerSpawnPath: "planner"})

	assert.False(t, resp.OK, "a cyclic suspend is refused, not signalled")
	assert.False(t, resp.Suspend)
	assert.Contains(t, resp.Error, "spawn_cycle_detected")
}
