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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/alertstore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// workspaceCluster stands in for the API server as one caller sees it. The caller works in the
// namespaces in members (they may list agents there); they may read model routes and tool
// registries only in readable; clusterWide lets them read everything and list namespaces.
type workspaceCluster struct {
	clusterWide bool
	members     map[string]bool
	readable    map[string]bool
	routes      map[string][]string // namespace -> route names, sorted

	mu    sync.Mutex
	lists []string // every List made, as "<kind>@<namespace>"
	ssars []string // every access review, as "<resource>@<namespace>"
}

func (wc *workspaceCluster) record(dst *[]string, entry string) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	*dst = append(*dst, entry)
}

func (wc *workspaceCluster) count(dst *[]string, entry string) int {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	n := 0
	for _, e := range *dst {
		if e == entry {
			n++
		}
	}
	return n
}

func (wc *workspaceCluster) client(t *testing.T) client.Client {
	t.Helper()
	forbidden := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: agentsAPIGroup, Resource: resource}, "", fmt.Errorf("denied"))
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			switch o := obj.(type) {
			case *authnv1.SelfSubjectReview:
				o.Status.UserInfo = authnv1.UserInfo{Username: "alice"}
			case *authzv1.SelfSubjectAccessReview:
				ra := o.Spec.ResourceAttributes
				wc.record(&wc.ssars, ra.Resource+"@"+ra.Namespace)
				switch {
				case wc.clusterWide:
					o.Status.Allowed = true
				case ra.Namespace == "":
					o.Status.Allowed = false
				case ra.Resource == resAgentDeployments:
					o.Status.Allowed = wc.members[ra.Namespace]
				default:
					o.Status.Allowed = wc.readable[ra.Namespace]
				}
			}
			return nil
		},
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := (&client.ListOptions{}).ApplyOptions(opts)
			kind := strings.TrimPrefix(fmt.Sprintf("%T", list), "*")
			wc.record(&wc.lists, kind+"@"+lo.Namespace)
			if nl, ok := list.(*corev1.NamespaceList); ok {
				if !wc.clusterWide {
					return forbidden("namespaces")
				}
				for _, ns := range []string{"a", "b", "c", "d"} {
					nl.Items = append(nl.Items, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
				}
				return nil
			}
			if lo.Namespace == "" && !wc.clusterWide {
				return forbidden(kind)
			}
			if lo.Namespace != "" && !wc.clusterWide && !wc.readable[lo.Namespace] {
				return forbidden(kind)
			}
			rl, ok := list.(*agentsv1beta1.ModelRouteList)
			if !ok {
				return nil
			}
			var all []agentsv1beta1.ModelRoute
			for _, ns := range []string{"a", "b", "c", "d"} {
				if lo.Namespace != "" && ns != lo.Namespace {
					continue
				}
				for _, name := range wc.routes[ns] {
					all = append(all, agentsv1beta1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
				}
			}
			// Offset paging, the way the API server pages a list by key order.
			from := 0
			if lo.Continue != "" {
				n, err := strconv.Atoi(lo.Continue)
				if err != nil {
					return apierrors.NewBadRequest("invalid continue token")
				}
				from = n
			}
			to := len(all)
			if lo.Limit > 0 && from+int(lo.Limit) < len(all) {
				to = from + int(lo.Limit)
				rl.Continue = strconv.Itoa(to)
			}
			rl.Items = all[from:to]
			return nil
		},
	}).Build()
}

func newWorkspaceCluster() *workspaceCluster {
	return &workspaceCluster{
		members:  map[string]bool{"a": true, "b": true, "c": true},
		readable: map[string]bool{"a": true, "c": true},
		routes: map[string][]string{
			"a": {"r1", "r2", "r3"},
			"b": {"r1"},
			"c": {"r1", "r2"},
			"d": {"r1"},
		},
	}
}

// newWorkspaceServer serves as the cluster's caller, with the mirror holding every namespace.
func newWorkspaceServer(t *testing.T, wc *workspaceCluster, opts Options) *Server {
	t.Helper()
	mirror := namespacetenant.NewMemStore()
	for _, ns := range []string{"a", "b", "c", "d"} {
		require.NoError(t, mirror.RecordNamespace(context.Background(), ns))
	}
	opts.CallerClients = newFakeFactory(wc.client(t))
	opts.Scheme = testScheme(t)
	opts.Auth = AllowAll{}
	opts.NamespaceTenantStore = mirror
	opts.Version = "test"
	opts.Log = logr.Discard()
	return NewServer(opts)
}

func getAs(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer person-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// routePage reads one page of GET /api/modelroutes and returns its "ns/name" rows and cursor.
func routePage(t *testing.T, s *Server, query string) ([]string, string) {
	t.Helper()
	rec := getAs(t, s, "/api/modelroutes?"+query)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body ModelRouteListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	rows := make([]string, 0, len(body.Items))
	for _, it := range body.Items {
		rows = append(rows, it.Namespace+"/"+it.Name)
	}
	return rows, body.NextCursor
}

// A person bound to some namespaces pages "All workspaces" across exactly those where they may
// read, in the order a cluster-wide reader would see them, and is never shown a namespace they do
// not work in. A namespace they work in but may not read this kind in is skipped, not fatal.
func TestAllWorkspacesPagesAcrossTheCallersNamespaces(t *testing.T) {
	wc := newWorkspaceCluster()
	s := newWorkspaceServer(t, wc, Options{})

	walk := func(limit int) [][]string {
		var pages [][]string
		cursor := ""
		for range 10 {
			rows, next := routePage(t, s, "limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(cursor))
			pages = append(pages, rows)
			if next == "" {
				break
			}
			cursor = next
		}
		return pages
	}
	assert.Equal(t, [][]string{{"a/r1", "a/r2"}, {"a/r3", "c/r1"}, {"c/r2"}}, walk(2))
	assert.Equal(t, [][]string{{"a/r1", "a/r2", "a/r3"}, {"c/r1", "c/r2"}}, walk(3),
		"a page that fills exactly as a namespace runs out still leads on to the next")
	assert.Zero(t, wc.count(&wc.lists, "v1beta1.ModelRouteList@d"), "d is not the caller's workspace and is never read")
}

func TestAllWorkspacesRefusedEverywhereIs403(t *testing.T) {
	wc := newWorkspaceCluster()
	wc.readable = map[string]bool{}
	s := newWorkspaceServer(t, wc, Options{})

	rec := getAs(t, s, "/api/modelroutes")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"items"`)
}

func TestAllWorkspacesNoWorkspaceAtAllIs403(t *testing.T) {
	wc := newWorkspaceCluster()
	wc.members, wc.readable = map[string]bool{}, map[string]bool{}
	s := newWorkspaceServer(t, wc, Options{})

	assert.Equal(t, http.StatusForbidden, getAs(t, s, "/api/modelroutes").Code)
	assert.Equal(t, http.StatusForbidden, getAs(t, s, "/api/knowledgebases").Code)
}

func TestAllWorkspacesEmptyWorkspaceIs200(t *testing.T) {
	wc := newWorkspaceCluster()
	wc.members, wc.readable, wc.routes = map[string]bool{"a": true}, map[string]bool{"a": true}, nil
	s := newWorkspaceServer(t, wc, Options{})

	rows, next := routePage(t, s, "")
	assert.Empty(t, rows)
	assert.Empty(t, next)
	rec := getAs(t, s, "/api/knowledgebases")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
}

// A caller who may read cluster-wide keeps one cluster-wide read and the API server's own cursor;
// their namespaces are never even discovered.
func TestAllWorkspacesClusterWideIsOneRead(t *testing.T) {
	wc := newWorkspaceCluster()
	wc.clusterWide = true
	s := newWorkspaceServer(t, wc, Options{})

	rows, next := routePage(t, s, "limit=4")
	assert.Equal(t, []string{"a/r1", "a/r2", "a/r3", "b/r1"}, rows)
	assert.Equal(t, "4", next, "the API server's continue token passes through untouched")
	assert.Equal(t, 1, wc.count(&wc.lists, "v1beta1.ModelRouteList@"))
	assert.Zero(t, wc.count(&wc.lists, "v1.NamespaceList@"), "no discovery for a cluster-wide reader")
}

// A cursor resumes at its namespace or, once that namespace has left the caller's set, at the
// next one. A cursor this server could not have issued is a 400, not a 500.
func TestAllWorkspacesCursorResumesAndRejectsForgeries(t *testing.T) {
	wc := newWorkspaceCluster()
	s := newWorkspaceServer(t, wc, Options{})

	gone, err := encodeWorkspaceCursor(workspaceCursor{Namespace: "bb", Continue: "1"})
	require.NoError(t, err)
	rows, next := routePage(t, s, "limit=5&cursor="+url.QueryEscape(gone))
	assert.Equal(t, []string{"c/r1", "c/r2"}, rows, "resumes at the first workspace after the vanished one, from its start")
	assert.Empty(t, next)

	for _, bad := range []string{"ws.!!!", "ws." + "e30"} { // not base64; {} with no namespace
		rec := getAs(t, s, "/api/modelroutes?cursor="+url.QueryEscape(bad))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", bad, rec.Body.String())
	}
}

// A resumed page is refused exactly when the first page would be: a cursor never turns a denial
// into an empty list, and never turns an allowed caller's empty tail into a denial.
func TestAllWorkspacesResumedPageIsRefusedOnlyWhereTheFirstWouldBe(t *testing.T) {
	cursor := func(ns string) string {
		c, err := encodeWorkspaceCursor(workspaceCursor{Namespace: ns})
		require.NoError(t, err)
		return url.QueryEscape(c)
	}

	for name, setup := range map[string]func(*workspaceCluster){
		"no workspace at all":        func(wc *workspaceCluster) { wc.members, wc.readable = map[string]bool{}, map[string]bool{} },
		"workspaces that all refuse": func(wc *workspaceCluster) { wc.readable = map[string]bool{} },
	} {
		t.Run(name, func(t *testing.T) {
			wc := newWorkspaceCluster()
			setup(wc)
			s := newWorkspaceServer(t, wc, Options{})
			rec := getAs(t, s, "/api/modelroutes?cursor="+cursor("a"))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), `"items"`)
		})
	}

	t.Run("allowed before the cursor", func(t *testing.T) {
		wc := newWorkspaceCluster()
		wc.readable = map[string]bool{"a": true}
		s := newWorkspaceServer(t, wc, Options{})
		rows, next := routePage(t, s, "cursor="+cursor("b"))
		assert.Empty(t, rows, "b and c refuse; a, before the cursor, is not read again")
		assert.Empty(t, next)
	})

	t.Run("allowed cluster-wide", func(t *testing.T) {
		wc := newWorkspaceCluster()
		wc.clusterWide = true
		s := newWorkspaceServer(t, wc, Options{})
		rows, _ := routePage(t, s, "cursor="+cursor("zz"))
		assert.Empty(t, rows, "a cursor past every workspace is the end of the list")
	})
}

// A cursor from "All workspaces" sent with one namespace is the caller's mistake, a 400; the API
// server, which would be the one to reject it, answers in a way that surfaces as a 500.
func TestWorkspaceCursorWithANamespaceIs400(t *testing.T) {
	wc := newWorkspaceCluster()
	s := newWorkspaceServer(t, wc, Options{})
	c, err := encodeWorkspaceCursor(workspaceCursor{Namespace: "a", Continue: "1"})
	require.NoError(t, err)

	rec := getAs(t, s, "/api/modelroutes?namespace=a&cursor="+url.QueryEscape(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Zero(t, wc.count(&wc.lists, "v1beta1.ModelRouteList@a"), "the cursor never reaches the API server")
}

// One request discovers the caller's workspaces once, however many kinds it reads: topology
// reads three.
func TestWorkspaceDiscoveryRunsOncePerRequest(t *testing.T) {
	wc := newWorkspaceCluster()
	s := newWorkspaceServer(t, wc, Options{})

	rec := getAs(t, s, "/api/topology")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, wc.count(&wc.lists, "v1.NamespaceList@"))
	for _, ns := range []string{"a", "b", "c", "d"} {
		assert.Equal(t, 1, wc.count(&wc.ssars, resAgentDeployments+"@"+ns), "one membership probe per mirror namespace")
	}
}

// GET /api/namespaces and a list given no namespace answer from the same set.
func TestNamespacesAndListsShareTheWorkspaceSet(t *testing.T) {
	wc := newWorkspaceCluster()
	s := newWorkspaceServer(t, wc, Options{})

	rec := getAs(t, s, "/api/namespaces")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"namespaces":[{"name":"a"},{"name":"b"},{"name":"c"}]}`, rec.Body.String())
}

// countingRegistryStore records every List so a test can prove when the store was read.
type countingRegistryStore struct {
	toolregistry.Store
	mu    sync.Mutex
	lists []controlplane.ListOptions
}

func (c *countingRegistryStore) List(ctx context.Context, opts controlplane.ListOptions) (controlplane.Page[toolregistry.ToolRegistry], error) {
	c.mu.Lock()
	c.lists = append(c.lists, opts)
	c.mu.Unlock()
	return c.Store.List(ctx, opts)
}

func (c *countingRegistryStore) calls() []controlplane.ListOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.lists)
}

func seedMCPRegistry(t *testing.T, store toolregistry.Store, ns, name, owner string) {
	t.Helper()
	labels := map[string]string{labelManagedBy: managedByMCP, labelMCPVisibility: visibilityTeam}
	if owner != "" {
		labels[labelMCPVisibility] = visibilityPrivate
		labels[labelMCPOwner] = userGrantHash(owner)
	}
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
		Namespace: ns, Name: name, Labels: labels,
		Tools: []toolregistry.ToolEntry{{Name: name + "_tool", URL: "http://" + name + ".svc/mcp"}},
	})
	require.NoError(t, err)
}

// The store-backed "All workspaces" read authorizes before it reads: a caller allowed nowhere costs
// no store read at all, and an allowed caller's read is narrowed to their namespaces in the query.
func TestAllWorkspacesReadsNoStoreBeforeAuthorization(t *testing.T) {
	for _, path := range []string{"/api/tools", "/api/mcpservers", "/api/toolregistries"} {
		t.Run(path, func(t *testing.T) {
			wc := newWorkspaceCluster()
			wc.readable = map[string]bool{}
			store := &countingRegistryStore{Store: toolregistry.NewMemStore()}
			for _, ns := range []string{"a", "b", "c", "d"} {
				seedMCPRegistry(t, store.Store, ns, ns+"-tools", "")
			}
			s := newWorkspaceServer(t, wc, Options{MCPEnabled: true, ToolRegistryStore: store})

			rec := getAs(t, s, path)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Empty(t, store.calls(), "a refused caller must cost no store read")

			wc.readable = map[string]bool{"a": true, "c": true}
			rec = getAs(t, s, path)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "a-tools")
			assert.Contains(t, rec.Body.String(), "c-tools")
			assert.NotContains(t, rec.Body.String(), "b-tools")
			assert.NotContains(t, rec.Body.String(), "d-tools")
			for _, call := range store.calls() {
				assert.Equal(t, []string{"a", "c"}, call.Namespaces, "the query itself is narrowed")
			}
		})
	}
}

// Whether "All workspaces" is 403 or 200 must not depend on what the store holds: a namespace
// whose only server is someone else's private one answers exactly as an empty namespace does.
func TestAllWorkspacesHidesWhetherAnotherUsersPrivateServerExists(t *testing.T) {
	answer := func(t *testing.T, readable map[string]bool, withPrivate bool, path string) (int, string) {
		t.Helper()
		wc := newWorkspaceCluster()
		wc.readable = readable
		store := toolregistry.NewMemStore()
		if withPrivate {
			seedMCPRegistry(t, store, "a", "bobs-server", "bob")
		}
		s := newWorkspaceServer(t, wc, Options{MCPEnabled: true, ToolRegistryStore: store})
		rec := getAs(t, s, path)
		return rec.Code, rec.Body.String()
	}
	for _, path := range []string{"/api/tools", "/api/mcpservers"} {
		allowed := map[string]bool{"a": true}
		emptyCode, emptyBody := answer(t, allowed, false, path)
		privCode, privBody := answer(t, allowed, true, path)
		assert.Equal(t, http.StatusOK, emptyCode, path)
		assert.Equal(t, emptyCode, privCode, path)
		assert.JSONEq(t, emptyBody, privBody, path)

		nowhere := map[string]bool{}
		emptyCode, emptyBody = answer(t, nowhere, false, path)
		privCode, privBody = answer(t, nowhere, true, path)
		assert.Equal(t, http.StatusForbidden, emptyCode, path)
		assert.Equal(t, emptyCode, privCode, path)
		assert.JSONEq(t, emptyBody, privBody, path)
	}
}

// The alerts feed for "All workspaces" is the newest alerts across the caller's namespaces.
func TestAllWorkspacesAlertsAreTheNewestAcrossTheCallersNamespaces(t *testing.T) {
	wc := newWorkspaceCluster()
	alerts := alertstore.NewMemStore()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i, ns := range []string{"a", "c", "a", "d", "b"} {
		_, err := alerts.Append(context.Background(), alertstore.Alert{
			Namespace: ns, PolicyName: "p", Condition: "cond-" + strconv.Itoa(i), FiredAt: base.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}
	s := newWorkspaceServer(t, wc, Options{AlertStore: alerts})

	rec := getAs(t, s, "/api/alerts?limit=2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body AlertListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	got := make([]string, 0, len(body.Items))
	for _, a := range body.Items {
		got = append(got, a.Namespace+"/"+a.Condition)
	}
	assert.Equal(t, []string{"a/cond-2", "c/cond-1"}, got, "d (not theirs) and b (not readable) never appear")
	assert.True(t, body.Truncated, "a third readable alert exists")
}

// Skills for "All workspaces" are the caller's readable namespaces' skills, and a cluster-wide
// reader gets every namespace's rather than none.
func TestAllWorkspacesSkills(t *testing.T) {
	store := skill.NewMemStore()
	for _, ns := range []string{"a", "b", "c", "d"} {
		require.NoError(t, store.UpsertSkill(context.Background(), skill.Skill{Namespace: ns, Name: "summarise"}))
	}
	skillNamespaces := func(t *testing.T, wc *workspaceCluster) []string {
		t.Helper()
		rec := getAs(t, newWorkspaceServer(t, wc, Options{SkillStore: store}), "/api/skills")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body SkillListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		out := make([]string, 0, len(body.Skills))
		for _, sk := range body.Skills {
			out = append(out, sk.Namespace)
		}
		return out
	}

	assert.Equal(t, []string{"a", "c"}, skillNamespaces(t, newWorkspaceCluster()))
	everywhere := newWorkspaceCluster()
	everywhere.clusterWide = true
	assert.Equal(t, []string{"a", "b", "c", "d"}, skillNamespaces(t, everywhere))
}
