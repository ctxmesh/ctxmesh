//go:build integration

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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// TestTraceReadsFollowRealRBAC: against a real API server and the shipped viewer persona, a trace no run
// maps opens for a viewer of its agent's namespace and is one uniform 404 everywhere else, while a trace
// a run maps keeps the run's policy (403 for a caller who neither made the run nor can read its agent).
func TestTraceReadsFollowRealRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const mine, theirs = "trace-authz-mine", "trace-authz-theirs"
	for _, ns := range []string{mine, theirs} {
		require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		require.NoError(t, admin.Create(ctx, &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "aha", Namespace: ns},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "echo:1"},
		}))
	}
	viewer := personaUser(t, admin, mine, "viewer-"+mine, installPersona(t, admin, "ctxmesh_viewer_role.yaml"))

	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r-theirs", theirs, "aha", "someone-else@example.com", "tr-run-theirs", "hi")
	lf := fakeLangfuseAdapter{
		detailByID: map[string]TraceDetail{
			"tr-mine":       taggedTrace("tr-mine", mine, "aha"),
			"tr-theirs":     taggedTrace("tr-theirs", theirs, "aha"),
			"tr-deleted":    taggedTrace("tr-deleted", mine, "deleted-agent"),
			"tr-untagged":   taggedTrace("tr-untagged", "", ""),
			"tr-run-theirs": taggedTrace("tr-run-theirs", mine, "aha"),
		},
		detailErrByID: map[string]error{"tr-absent": ErrTraceNotFound},
	}
	s := NewServer(Options{
		CallerClients: newFakeFactory(viewer),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Langfuse: lf},
		RunStore:      rs,
		Version:       "test",
		Log:           logr.Discard(),
	})
	get := func(traceID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/traces/"+traceID+"/detail", nil)
		req.Header.Set("Authorization", "Bearer viewer-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := get("tr-mine")
	require.Equal(t, http.StatusOK, rec.Code, "a viewer of the trace's agent opens it: %s", rec.Body.String())

	var refusal string
	for _, id := range []string{"tr-theirs", "tr-deleted", "tr-untagged", "tr-absent"} {
		rec := get(id)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", id, rec.Body.String())
		if refusal == "" {
			refusal = rec.Body.String()
		}
		assert.Equal(t, refusal, rec.Body.String(), "%s must be indistinguishable from the other refusals", id)
	}

	// The run decides, not the trace's tag, even though the tag names an agent the viewer can read.
	rec = get("tr-run-theirs")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
