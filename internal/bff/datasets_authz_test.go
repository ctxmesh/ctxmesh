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
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// Every dataset route authorizes the caller on evalsuites in the dataset's namespace before it touches
// the dataset store, the run store or the trace backend; the counters below are how a test proves the
// "before".

type countingDatasetStore struct {
	dataset.Store
	calls atomic.Int64
}

func (c *countingDatasetStore) EnsureDataset(ctx context.Context, ns, name string) (*dataset.Dataset, error) {
	c.calls.Add(1)
	return c.Store.EnsureDataset(ctx, ns, name)
}

func (c *countingDatasetStore) GetDataset(ctx context.Context, ns, name string) (*dataset.Dataset, error) {
	c.calls.Add(1)
	return c.Store.GetDataset(ctx, ns, name)
}

func (c *countingDatasetStore) AppendCase(ctx context.Context, id string, cs dataset.Case) (string, error) {
	c.calls.Add(1)
	return c.Store.AppendCase(ctx, id, cs)
}

func (c *countingDatasetStore) AppendLabel(ctx context.Context, caseID string, l dataset.Label) error {
	c.calls.Add(1)
	return c.Store.AppendLabel(ctx, caseID, l)
}

func (c *countingDatasetStore) PinVersion(ctx context.Context, id string) (int, error) {
	c.calls.Add(1)
	return c.Store.PinVersion(ctx, id)
}

func (c *countingDatasetStore) ResolveVersion(ctx context.Context, ns, name string, v int) ([]dataset.ResolvedCase, error) {
	c.calls.Add(1)
	return c.Store.ResolveVersion(ctx, ns, name, v)
}

func (c *countingDatasetStore) ResolveRef(ctx context.Context, ns, ref string) ([]dataset.ResolvedCase, int, error) {
	c.calls.Add(1)
	return c.Store.ResolveRef(ctx, ns, ref)
}

func (c *countingDatasetStore) ListCases(ctx context.Context, id string) ([]dataset.Case, error) {
	c.calls.Add(1)
	return c.Store.ListCases(ctx, id)
}

func (c *countingDatasetStore) ListDatasets(ctx context.Context, ns string) ([]dataset.Dataset, error) {
	c.calls.Add(1)
	return c.Store.ListDatasets(ctx, ns)
}

func (c *countingDatasetStore) LatestLabel(ctx context.Context, caseID string) (*dataset.Label, error) {
	c.calls.Add(1)
	return c.Store.LatestLabel(ctx, caseID)
}

type countingLangfuse struct {
	LangfuseAdapter
	calls atomic.Int64
}

func (c *countingLangfuse) TraceURL(id string) (string, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.TraceURL(id)
}

func (c *countingLangfuse) RecentRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.RecentRuns(ctx, limit)
}

func (c *countingLangfuse) RunsForAgent(ctx context.Context, ns, name string, limit int) ([]RunSummary, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.RunsForAgent(ctx, ns, name, limit)
}

func (c *countingLangfuse) CostUsage(ctx context.Context) (CostSummary, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.CostUsage(ctx)
}

func (c *countingLangfuse) TraceDetail(ctx context.Context, id string) (TraceDetail, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.TraceDetail(ctx, id)
}

func (c *countingLangfuse) FilteredRuns(ctx context.Context, f RunFilter) (RunListPage, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.FilteredRuns(ctx, f)
}

func (c *countingLangfuse) TraceScores(ctx context.Context, id string) ([]FeedbackScore, error) {
	c.calls.Add(1)
	return c.LangfuseAdapter.TraceScores(ctx, id)
}

// countingRunStore counts the run-store calls a dataset route can make: from-run resolves a trace id to
// a run, export creates one.
type countingRunStore struct {
	run.Store
	calls atomic.Int64
}

func (c *countingRunStore) Get(id string) (*run.Run, error) {
	c.calls.Add(1)
	return c.Store.Get(id)
}

func (c *countingRunStore) GetByTraceID(id string) (*run.Run, error) {
	c.calls.Add(1)
	return c.Store.GetByTraceID(id)
}

func (c *countingRunStore) Create(r *run.Run) error {
	c.calls.Add(1)
	return c.Store.Create(r)
}

// policyAuthorizer answers each authorization with decide and records what was asked.
type policyAuthorizer struct {
	decide func(a authz.Action) error
	asked  []authz.Action
}

func (p *policyAuthorizer) Authorize(_ context.Context, _ client.Client, a authz.Action) error {
	p.asked = append(p.asked, a)
	return p.decide(a)
}

// readOnly is a viewer's answer: list on evalsuites, nothing else.
func readOnly(a authz.Action) error {
	if a.Resource == resEvalSuites && a.Verb == authz.VerbList {
		return nil
	}
	return authz.ErrForbidden
}

func allowAll(authz.Action) error { return nil }

func denyAll(authz.Action) error { return authz.ErrForbidden }

func tokenRejected(authz.Action) error {
	return apierrors.NewUnauthorized("token rejected")
}

type datasetAuthzFixture struct {
	s     *Server
	ds    *countingDatasetStore
	lf    *countingLangfuse
	runs  *countingRunStore
	auth  *policyAuthorizer
	inner dataset.Store
}

// newDatasetAuthzServer serves the dataset routes with counted stores. The caller can read the labeling
// agent (default/labeling-agent); agentGet, when set, answers the caller's AgentDeployment reads instead.
func newDatasetAuthzServer(t *testing.T, decide func(authz.Action) error, agentGet func(key client.ObjectKey) error) datasetAuthzFixture {
	t.Helper()
	sc := testScheme(t)
	b := fake.NewClientBuilder().WithScheme(sc).WithObjects(
		readyAgent(labelingAgentName, labelingAgentNs, ""),
		readyAgent("chatbot", "team-a", ""),
	)
	funcs := ssrInterceptor("alice", nil)
	if agentGet != nil {
		funcs.Get = func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*agentsv1beta1.AgentDeployment); ok {
				if err := agentGet(key); err != nil {
					return err
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		}
	}
	c := b.WithInterceptorFuncs(funcs).Build()
	inner := dataset.NewMemStore()
	f := datasetAuthzFixture{
		ds:    &countingDatasetStore{Store: inner},
		lf:    &countingLangfuse{LangfuseAdapter: newFakeLabelingLangfuse(t, "tr-1", "question", "answer")},
		runs:  &countingRunStore{Store: run.NewMemStore()},
		auth:  &policyAuthorizer{decide: decide},
		inner: inner,
	}
	f.s = NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      f.runs,
		DatasetStore:  f.ds,
		Adapters:      Adapters{Langfuse: f.lf},
	})
	f.s.authorizer = f.auth
	return f
}

type datasetRoute struct {
	name, method, path string
	body               any
	write              bool
	ok                 int
}

// datasetRoutes are every dataset route, addressed at namespace ns, for a dataset "goldens" there whose one
// case is caseID.
func datasetRoutes(ns, caseID string) []datasetRoute {
	q := "?namespace=" + ns
	return []datasetRoute{
		{"list", http.MethodGet, "/api/datasets" + q, nil, false, http.StatusOK},
		{"cases", http.MethodGet, "/api/datasets/goldens/cases" + q, nil, false, http.StatusOK},
		{
			"label", http.MethodPost, "/api/datasets/goldens/cases/" + caseID + "/labels" + q,
			AppendLabelRequest{Value: "pass"},
			true, http.StatusCreated,
		},
		{
			"from-run", http.MethodPost, "/api/datasets/goldens/cases/from-run" + q,
			FromRunRequest{TraceID: "tr-1"},
			true, http.StatusCreated,
		},
		{"pin", http.MethodPost, "/api/datasets/goldens/pin" + q, nil, true, http.StatusOK},
		{
			"export", http.MethodPost, "/api/datasets/goldens/export" + q,
			ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"},
			true, http.StatusAccepted,
		},
	}
}

func seedGoldens(t *testing.T, f datasetAuthzFixture, ns string) string {
	t.Helper()
	caseID := seedCase(t, f.inner, ns, "goldens", "What is 2+2?")
	f.ds.calls.Store(0)
	return caseID
}

// A caller refused by RBAC gets the refusal before any store or trace-backend read, on every route; a
// token the API server rejects is a 401 the same way. The question asked is evalsuites in the dataset's
// namespace: list to read, update to write.
func TestDatasets_RefusedBeforeAnyRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide func(authz.Action) error
		status func(write bool) int
	}{
		{"denied", denyAll, func(bool) int { return http.StatusForbidden }},
		{"token rejected", tokenRejected, func(bool) int { return http.StatusUnauthorized }},
		{"read-only caller writing", readOnly, func(write bool) int {
			if write {
				return http.StatusForbidden
			}
			return 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, rt := range datasetRoutes("team-a", "case-x") {
				want := tc.status(rt.write)
				if want == 0 {
					continue
				}
				t.Run(rt.name, func(t *testing.T) {
					f := newDatasetAuthzServer(t, tc.decide, nil)
					seedGoldens(t, f, "team-a")
					code, body := labelingRequest(t, f.s, rt.method, rt.path, rt.body)
					require.Equal(t, want, code, "body: %s", body)
					assert.Zero(t, f.ds.calls.Load(), "no dataset store call before authorization")
					assert.Zero(t, f.lf.calls.Load(), "no trace backend call before authorization")
					assert.Zero(t, f.runs.calls.Load(), "no run store call before authorization")
					require.NotEmpty(t, f.auth.asked)
					first := f.auth.asked[0]
					assert.Equal(t, resEvalSuites, first.Resource)
					assert.Equal(t, "team-a", first.Namespace)
					if rt.write {
						assert.Equal(t, authz.VerbUpdate, first.Verb)
					} else {
						assert.Equal(t, authz.VerbList, first.Verb)
					}
				})
			}
		})
	}
}

// An authorized caller still can, on every route.
func TestDatasets_AuthorizedCallerSucceeds(t *testing.T) {
	for _, rt := range datasetRoutes(labelingAgentNs, "") {
		t.Run(rt.name, func(t *testing.T) {
			f := newDatasetAuthzServer(t, allowAll, nil)
			caseID := seedGoldens(t, f, labelingAgentNs)
			path := rt.path
			if rt.name == "label" {
				path = fmt.Sprintf("/api/datasets/goldens/cases/%s/labels?namespace=%s", caseID, labelingAgentNs)
			}
			code, body := labelingRequest(t, f.s, rt.method, path, rt.body)
			assert.Equal(t, rt.ok, code, "body: %s", body)
		})
	}
}

// A viewer reads datasets and cannot change them.
func TestDatasets_ReadOnlyCallerReads(t *testing.T) {
	for _, rt := range datasetRoutes("team-a", "") {
		if rt.write {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			f := newDatasetAuthzServer(t, readOnly, nil)
			seedGoldens(t, f, "team-a")
			code, body := labelingRequest(t, f.s, rt.method, rt.path, rt.body)
			assert.Equal(t, http.StatusOK, code, "body: %s", body)
		})
	}
}

// Export copies an agent's traces, so the caller must also be able to read that agent: a denial is 403,
// a missing agent 404, a rejected token 401, and in each case no export run exists.
func TestExportDataset_RequiresReadOnTheSourceAgent(t *testing.T) {
	gr := k8sschema.GroupResource{Group: agentsv1beta1.GroupVersion.Group, Resource: "agentdeployments"}
	for _, tc := range []struct {
		name  string
		agent func(key client.ObjectKey) error
		body  ExportRequest
		want  int
	}{
		{
			"forbidden", func(k client.ObjectKey) error { return apierrors.NewForbidden(gr, k.Name, errors.New("no")) },
			ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"},
			http.StatusForbidden,
		},
		{
			"token rejected", func(client.ObjectKey) error { return apierrors.NewUnauthorized("no") },
			ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"},
			http.StatusUnauthorized,
		},
		{"missing", nil, ExportRequest{AgentNamespace: "team-a", AgentName: "absent"}, http.StatusNotFound},
		{"another namespace's agent", func(k client.ObjectKey) error {
			if k.Namespace != "team-a" {
				return apierrors.NewForbidden(gr, k.Name, errors.New("no"))
			}
			return nil
		}, ExportRequest{AgentNamespace: "victim-ns", AgentName: "victim"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDatasetAuthzServer(t, allowAll, tc.agent)
			code, body := labelingRequest(t, f.s, http.MethodPost, "/api/datasets/goldens/export?namespace=team-a", tc.body)
			require.Equal(t, tc.want, code, "body: %s", body)
			assert.Zero(t, f.runs.calls.Load(), "a refused export creates no run")
			assert.Zero(t, f.ds.calls.Load())
			assert.Zero(t, f.lf.calls.Load())
		})
	}
}

// A label names its case by id, so the case must belong to the dataset in the path, in the namespace the
// caller is authorized for: otherwise a writer in one namespace could label another's cases.
func TestAppendLabel_CaseMustBelongToTheAuthorizedDataset(t *testing.T) {
	f := newDatasetAuthzServer(t, func(a authz.Action) error {
		if a.Namespace == "team-a" {
			return nil
		}
		return authz.ErrForbidden
	}, nil)
	theirCase := seedCase(t, f.inner, "victim-ns", "goldens", "their question")
	otherCase := seedCase(t, f.inner, "team-a", "other", "a case in another dataset")
	seedCase(t, f.inner, "team-a", "goldens", "my question")

	for name, path := range map[string]string{
		"another namespace's case":    "/api/datasets/goldens/cases/" + theirCase + "/labels?namespace=team-a",
		"another dataset's case":      "/api/datasets/goldens/cases/" + otherCase + "/labels?namespace=team-a",
		"a dataset that is not there": "/api/datasets/absent/cases/" + otherCase + "/labels?namespace=team-a",
	} {
		t.Run(name, func(t *testing.T) {
			code, body := labelingRequest(t, f.s, http.MethodPost, path, AppendLabelRequest{Value: "fail"})
			require.Equal(t, http.StatusNotFound, code, "body: %s", body)
		})
	}
	for _, id := range []string{theirCase, otherCase} {
		label, err := f.inner.LatestLabel(context.Background(), id)
		require.NoError(t, err)
		assert.Nil(t, label, "a refused label is not written")
	}
}

// Reading a dataset, or pinning one, that does not exist is a 404 and does not create it.
func TestDatasets_ReadOrPinOfAMissingDatasetCreatesNothing(t *testing.T) {
	f := newDatasetAuthzServer(t, allowAll, nil)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/datasets/absent/cases?namespace=team-a"},
		{http.MethodPost, "/api/datasets/absent/pin?namespace=team-a"},
	} {
		code, body := labelingRequest(t, f.s, rt.method, rt.path, nil)
		require.Equal(t, http.StatusNotFound, code, "%s %s: %s", rt.method, rt.path, body)
	}
	all, err := f.inner.ListDatasets(context.Background(), "team-a")
	require.NoError(t, err)
	assert.Empty(t, all)
}
