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

// M91 EU2 — invoke audit attribution. `/api/invoke` and the durable `/api/runs` create resolve the caller
// but previously emitted no audit event, so "who invoked which agent/run" was unrecorded. These tests pin
// that an invoke now emits an `invoke` audit row naming the caller + agent (+ run id for the durable path).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/run"
)

func invokeAuditServer(t *testing.T, audit auditlog.Store) *Server {
	t.Helper()
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).
		WithInterceptorFuncs(ssrInterceptor("alice@example.com", nil)).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: &fakeInvokeAdapter{traceID: "tr-run", resp: []byte(`{"output":"ok"}`)}},
		AuditStore:    audit,
		RunStore:      run.NewMemStore(),
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func findInvokeAudit(entries []auditlog.Entry) *auditlog.Entry {
	for i := range entries {
		if entries[i].Action == auditActionInvoke {
			return &entries[i]
		}
	}
	return nil
}

// The durable /api/runs create emits an invoke audit row naming the caller + agent + the run id.
func TestCreateRun_EmitsInvokeAudit(t *testing.T) {
	audit := &captureAuditStore{}
	s := invokeAuditServer(t, audit)

	created := createRun(t, s, InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`),
	})

	e := findInvokeAudit(audit.all())
	require.NotNil(t, e, "the durable run create must emit an invoke audit event")
	assert.Equal(t, "alice@example.com", e.Actor, "the audit names the invoking end-user")
	assert.Equal(t, auditKindAgent, e.ResourceKind)
	assert.Equal(t, "echo", e.ResourceName, "the audit names the invoked agent")
	assert.Equal(t, "prod", e.Namespace)
	assert.Equal(t, created.ID, e.TraceID, "the audit links to the run id")
	assert.Equal(t, "success", e.Outcome)
}

// The synchronous /api/invoke emits an invoke audit row naming the caller + agent (no durable run id).
func TestInvoke_EmitsInvokeAudit(t *testing.T) {
	audit := &captureAuditStore{}
	s := invokeAuditServer(t, audit)

	raw, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer some-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	e := findInvokeAudit(audit.all())
	require.NotNil(t, e, "a synchronous invoke must emit an invoke audit event")
	assert.Equal(t, "alice@example.com", e.Actor)
	assert.Equal(t, "echo", e.ResourceName)
	assert.Empty(t, e.TraceID, "the synchronous invoke has no durable run id")
}

// A server with NO audit store wired must not panic + must still serve the invoke (best-effort, never a gate).
func TestInvoke_NoAuditStore_IsNoOp(t *testing.T) {
	s := invokeAuditServer(t, nil)
	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`)})
	assert.NotEmpty(t, created.ID, "the run is created even with no audit store")
}
