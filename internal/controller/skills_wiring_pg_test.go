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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/bff"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/pgtest"
)

// boundCaller is a caller whose SelfSubjectAccessReviews allow skills in one namespace only,
// which is what a developer bound there by RoleBinding gets from the API server.
type boundCaller struct{ c client.Client }

func (b boundCaller) ForRequest(*http.Request) (client.Client, error) { return b.c, nil }

func (boundCaller) PodLogsForRequest(*http.Request) (bff.PodLogAccessor, error) {
	return nil, fmt.Errorf("pod logs are not part of this test")
}

func developerIn(t *testing.T, namespace string) boundCaller {
	t.Helper()
	return boundCaller{c: fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				ssar, ok := obj.(*authzv1.SelfSubjectAccessReview)
				if !ok {
					return fmt.Errorf("unexpected create of %T", obj)
				}
				ra := ssar.Spec.ResourceAttributes
				ssar.Status.Allowed = ra != nil && ra.Resource == "skills" && ra.Namespace == namespace
				return nil
			},
		}).Build()}
}

// TestASkillCreatedInTheConsoleResolvesForAnAgent: the BFF and the controller, each wired the way
// its binary wires it (UseControlPlaneDB on the control-plane database), share skills. A
// developer creates a skill and a version through the API; an agent referencing it with an alias
// carries the pinned digest and the description; a reference to a skill nobody created, or one
// in another namespace, does not resolve.
func TestASkillCreatedInTheConsoleResolvesForAnAgent(t *testing.T) {
	db := pgtest.Open(t, "controller_skills")
	ctx := context.Background()
	// The schema outlives the run, so each run works in a namespace of its own.
	ns := fmt.Sprintf("skills-%d", time.Now().UnixNano())
	const digest = "sha256:5f2b8c0e9a7d4c3b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b"

	opts := bff.Options{
		CallerClients: developerIn(t, ns),
		Scheme:        podTemplateScheme(t),
		Auth:          bff.AllowAll{},
		Version:       "test",
		Log:           logr.Discard(),
	}
	opts.UseControlPlaneDB(db)
	console := bff.NewServer(opts).Handler()
	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer developer")
		console.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/api/skills", `{"namespace":"`+ns+`","name":"summarise","description":"Summarise a long document"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = post("/api/skills/"+ns+"/summarise/versions",
		`{"digest":"`+digest+`","source":"upload","objectKey":"skills/`+ns+`/summarise.md","sizeBytes":42}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = post("/api/skills", `{"namespace":"elsewhere","name":"summarise"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, "bound to %s, refused another namespace: %s", ns, rec.Body.String())

	r := &AgentDeploymentReconciler{
		Client: fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}},
		).Build(),
		Scheme: podTemplateScheme(t),
	}
	r.UseControlPlaneDB(db, agentversion.NewMemStore())
	agent := func(namespace string, refs ...string) *agentsv1beta1.AgentDeployment {
		return &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: namespace},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", SkillRefs: refs},
		}
	}

	pod, err := r.buildPodTemplate(ctx, agent(ns, "summarise@latest"))
	require.NoError(t, err)
	env := map[string]string{}
	for _, e := range pod.containers[0].Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "summarise@"+digest, env[envSkillRefs], "the alias is pinned to the digest the console added")
	var descriptions map[string]string
	require.NoError(t, json.Unmarshal([]byte(env[envSkillDescriptions]), &descriptions))
	assert.Equal(t, map[string]string{"summarise": "Summarise a long document"}, descriptions)

	_, err = r.buildPodTemplate(ctx, agent(ns, "translate@latest"))
	require.Error(t, err, "a skill nobody created does not resolve")
	assert.Contains(t, err.Error(), `"translate"`)

	_, err = r.buildPodTemplate(ctx, agent("elsewhere", "summarise@latest"))
	require.Error(t, err, "a skill resolves only in its own namespace")
	assert.Contains(t, err.Error(), `"summarise"`)
}
