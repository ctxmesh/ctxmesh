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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// TestDatasetsFollowRealRBAC: against a real API server and the shipped personas, bound in one namespace,
// a viewer reads that namespace's datasets and changes none, a developer reads and changes them, and
// neither reaches another namespace's datasets or exports another namespace's agent.
func TestDatasetsFollowRealRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const mine, theirs = "datasets-authz-mine", "datasets-authz-theirs"
	for _, ns := range []string{mine, theirs} {
		require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		require.NoError(t, admin.Create(ctx, &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "aha", Namespace: ns},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "echo:1"},
		}))
	}
	viewerRole := installPersona(t, admin, "ctxmesh_viewer_role.yaml")
	developerRole := installPersona(t, admin, "ctxmesh_developer_role.yaml")

	store := dataset.NewMemStore()
	seed := func(ns string) string {
		t.Helper()
		return seedCase(t, store, ns, "goldens", "question in "+ns)
	}
	myCase, theirCase := seed(mine), seed(theirs)

	serve := func(caller client.Client) *Server {
		return NewServer(Options{
			CallerClients: newFakeFactory(caller),
			Scheme:        testScheme(t),
			Auth:          AllowAll{},
			Adapters:      Adapters{Langfuse: fakeLangfuseAdapter{detailByID: map[string]TraceDetail{"tr-mine": taggedTrace("tr-mine", mine, "aha")}}},
			RunStore:      run.NewMemStore(),
			DatasetStore:  store,
			Version:       "test",
			Log:           logr.Discard(),
		})
	}
	q := func(ns string) string { return "?namespace=" + ns }

	t.Run("viewer", func(t *testing.T) {
		s := serve(personaUser(t, admin, mine, "viewer-"+mine, viewerRole))
		for _, path := range []string{"/api/datasets" + q(mine), "/api/datasets/goldens/cases" + q(mine)} {
			code, body := labelingRequest(t, s, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, code, "%s: %s", path, body)
		}
		for _, path := range []string{"/api/datasets" + q(theirs), "/api/datasets/goldens/cases" + q(theirs)} {
			code, body := labelingRequest(t, s, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, code, "%s: %s", path, body)
		}
		for _, w := range []struct {
			path string
			body any
		}{
			{"/api/datasets/goldens/cases/" + myCase + "/labels" + q(mine), AppendLabelRequest{Value: "pass"}},
			{"/api/datasets/goldens/pin" + q(mine), nil},
			{"/api/datasets/goldens/cases/from-run" + q(mine), FromRunRequest{TraceID: "tr-mine"}},
			{"/api/datasets/goldens/export" + q(mine), ExportRequest{AgentNamespace: mine, AgentName: "aha"}},
		} {
			code, body := labelingRequest(t, s, http.MethodPost, w.path, w.body)
			assert.Equal(t, http.StatusForbidden, code, "a viewer changes no dataset: %s: %s", w.path, body)
		}
	})

	t.Run("developer", func(t *testing.T) {
		s := serve(personaUser(t, admin, mine, "developer-"+mine, developerRole))

		code, body := labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/cases/"+myCase+"/labels"+q(mine), AppendLabelRequest{Value: "pass"})
		assert.Equal(t, http.StatusCreated, code, "label: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/cases/from-run"+q(mine), FromRunRequest{TraceID: "tr-mine"})
		assert.Equal(t, http.StatusCreated, code, "from-run: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/pin"+q(mine), nil)
		assert.Equal(t, http.StatusOK, code, "pin: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/export"+q(mine), ExportRequest{AgentNamespace: mine, AgentName: "aha"})
		assert.Equal(t, http.StatusAccepted, code, "export: %s", body)

		// Another namespace: neither its datasets nor its agent.
		code, body = labelingRequest(t, s, http.MethodGet, "/api/datasets"+q(theirs), nil)
		assert.Equal(t, http.StatusForbidden, code, "list theirs: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/cases/"+theirCase+"/labels"+q(theirs), AppendLabelRequest{Value: "fail"})
		assert.Equal(t, http.StatusForbidden, code, "label theirs: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/cases/"+theirCase+"/labels"+q(mine), AppendLabelRequest{Value: "fail"})
		assert.Equal(t, http.StatusNotFound, code, "their case through my namespace: %s", body)
		code, body = labelingRequest(t, s, http.MethodPost,
			"/api/datasets/goldens/export"+q(mine), ExportRequest{AgentNamespace: theirs, AgentName: "aha"})
		assert.Equal(t, http.StatusForbidden, code, "export their agent: %s", body)

		label, err := store.LatestLabel(ctx, theirCase)
		require.NoError(t, err)
		assert.Nil(t, label, "their case was never labeled")
	})
}
