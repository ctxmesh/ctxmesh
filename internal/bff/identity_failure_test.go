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
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// createCountingRuns is a run store that counts the runs created through it.
type createCountingRuns struct {
	run.Store
	created int
}

func (c *createCountingRuns) Create(r *run.Run) error {
	c.created++
	return c.Store.Create(r)
}

// failingIdentity answers the SelfSubjectReview with err and stores every other create.
func failingIdentity(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*authnv1.SelfSubjectReview); ok {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// TestStartingAJob_IdentityFailures: when the caller's identity cannot be read while starting an
// ingestion or a dataset export, a token the API server rejects is a 401 and any other failure is a
// 500. A 401 for an API-server hiccup would sign the person out of the console, which reads every 401
// as an expired session. Nothing is started either way.
func TestStartingAJob_IdentityFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"the token is rejected", apierrors.NewUnauthorized("token expired"), http.StatusUnauthorized},
		{"the API server fails", errors.New("connection reset by peer"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run("ingest: "+tc.name, func(t *testing.T) {
			s, docStore := newIngestEndpointServer(t, nil)
			runs := &createCountingRuns{Store: s.runStore}
			s.runStore = runs
			kb := mockKnowledgeBase("my-kb", kbNS)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(kb).
				WithInterceptorFuncs(failingIdentity(tc.err)).Build()
			s.callerClients = newFakeFactory(c)
			require.NoError(t, docStore.Put(context.Background(), objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md"),
				bytes.NewReader([]byte("text")), -1, "text/markdown"))
			code, body := postIngest(t, s, "my-kb", kbNS)
			assert.Equal(t, tc.want, code, string(body))
			assert.Zero(t, runs.created, "no ingestion was started")
		})
		t.Run("export: "+tc.name, func(t *testing.T) {
			runs := &createCountingRuns{Store: run.NewMemStore()}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(readyAgent("chatbot", "team-a", "")).
				WithInterceptorFuncs(failingIdentity(tc.err)).Build()
			s := NewServer(Options{
				CallerClients: newFakeFactory(c),
				Scheme:        testScheme(t),
				Auth:          AllowAll{},
				Log:           logr.Discard(),
				RunStore:      runs,
				DatasetStore:  dataset.NewMemStore(),
				Adapters:      Adapters{Langfuse: newFakeLabelingLangfuse(t, "tr-1", "q", "a"), Invoke: &fakeInvokeAdapter{}},
			})
			s.authorizer = &recordingAuthorizer{}
			code, body := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/export?namespace=team-a",
				ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"})
			assert.Equal(t, tc.want, code, string(body))
			assert.Zero(t, runs.created, "no export was started")
		})
	}
}
