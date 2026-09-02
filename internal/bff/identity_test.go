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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
)

// The review objects (SelfSubjectReview / SelfSubjectAccessReview) are virtual:
// the real API server fills their Status on the create. The fake client does NOT,
// so these interceptors stand in for the API server — they populate the review's
// Status on Create (and skip the fake store entirely, since a real cluster never
// persists them). This is the m13.2 analogue of forbiddenListInterceptor: it
// proves the SSR/SSAR wiring end to end without a live API server.

// ssrInterceptor answers every SelfSubjectReview.Create with the given identity,
// exactly as the API server would for the caller's token.
func ssrInterceptor(username string, groups []string) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			ssr, ok := obj.(*authnv1.SelfSubjectReview)
			if !ok {
				return nil
			}
			ssr.Status = authnv1.SelfSubjectReviewStatus{
				UserInfo: authnv1.UserInfo{Username: username, Groups: groups},
			}
			return nil
		},
	}
}

// ssarInterceptor answers every SelfSubjectAccessReview.Create by consulting the
// allow func for (resource, verb) — modelling the API server's RBAC decision so a
// test can assert the exact kind×verb matrix and partial denials.
func ssarInterceptor(allow func(resource, verb, namespace string) bool) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			ssar, ok := obj.(*authzv1.SelfSubjectAccessReview)
			if !ok {
				return nil
			}
			ra := ssar.Spec.ResourceAttributes
			ssar.Status = authzv1.SubjectAccessReviewStatus{
				Allowed: allow(ra.Resource, ra.Verb, ra.Namespace),
			}
			return nil
		},
	}
}

// --- GET /api/whoami --------------------------------------------------------

// TestWhoAmIMapsIdentity proves whoami issues a SelfSubjectReview through the
// caller-scoped client and maps its Status.UserInfo to {username, groups}.
func TestWhoAmIMapsIdentity(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssrInterceptor("alice", []string{"system:authenticated", "dev"})).
		Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "developer-persona-token", factory.gotToken,
		"whoami must run through the CALLER'S token, not the BFF SA")

	var body WhoAmIResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "alice", body.Username)
	assert.Equal(t, []string{"system:authenticated", "dev"}, body.Groups)
}

// TestWhoAmINoTokenIs401 proves a token-less whoami is rejected 401 by the
// caller-client factory BEFORE any SelfSubjectReview is issued.
func TestWhoAmINoTokenIs401(t *testing.T) {
	created := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				created = true
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/whoami", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, created, "no SelfSubjectReview must be issued for a token-less request")
}

// TestWhoAmIGroupsNeverNull proves the groups slice is [] (not null) on the wire
// even when the API server reports a user with no groups.
func TestWhoAmIGroupsNeverNull(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssrInterceptor("nobody", nil)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"groups":[]`, "groups must be [] not null")
}

// TestWhoAmIAPIRejectSurfaced proves an API-server rejection of the
// SelfSubjectReview surfaces honestly (401 for Unauthorized) — NOT masked as a
// 500 and NOT reported as an empty-but-successful identity.
func TestWhoAmIAPIRejectSurfaced(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewUnauthorized("token rejected by the API server")
			},
		}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer expired-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"an API-server authn rejection must surface as 401, not a 500")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}

// --- GET /api/capabilities --------------------------------------------------

// TestCapabilitiesBatchesTheMatrix proves the handler issues exactly one SSAR per
// golden resource × verb, in the agents group, for the requested namespace, and
// folds them into the flat allowed map — all through the caller's token.
func TestCapabilitiesBatchesTheMatrix(t *testing.T) {
	type probe struct{ resource, verb, namespace, group string }
	var (
		mu  sync.Mutex
		got []probe
	)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			// The SSARs run concurrently in the handler; guard the recorder.
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				ssar := obj.(*authzv1.SelfSubjectAccessReview)
				ra := ssar.Spec.ResourceAttributes
				mu.Lock()
				got = append(got, probe{ra.Resource, ra.Verb, ra.Namespace, ra.Group})
				mu.Unlock()
				ssar.Status = authzv1.SubjectAccessReviewStatus{Allowed: true}
				return nil
			},
		}).
		Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "operator-persona-token", factory.gotToken)

	// Exactly the golden cross product (resources × verbs) in the agents group + the requested
	// namespace, PLUS the two synthetic CORE-group probes: `get pods/log` (M100 UI99-logs)
	// and `create secrets` (M153 — connecting a provider writes a core Secret as well as a
	// SecretBinding, and gating the console on the binding alone invited a user to type an
	// API key into a request the API server would refuse).
	want := map[probe]bool{}
	for _, res := range goldenResources {
		for _, verb := range goldenVerbs {
			want[probe{res, verb, "prod", agentsAPIGroup}] = true
		}
	}
	want[probe{"pods", "get", "prod", ""}] = true       // the logs subresource probe (core group)
	want[probe{"secrets", "create", "prod", ""}] = true // the connect-a-provider probe (core group)
	require.Len(t, got, len(want), "one SSAR per golden resource×verb, plus the two core-group probes")
	for _, p := range got {
		assert.Contains(t, want, p, "unexpected SSAR probe: %+v", p)
		delete(want, p)
	}
	assert.Empty(t, want, "every golden resource×verb + both core-group probes must be probed exactly once")

	// The response echoes the namespace and carries the full flat matrix + both core cells.
	var body CapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "prod", body.Namespace)
	require.Len(t, body.Allowed, len(goldenResources)+2) // golden kinds + "logs" + "secrets"
	for _, res := range goldenResources {
		require.Contains(t, body.Allowed, res)
		for _, verb := range goldenVerbs {
			assert.True(t, body.Allowed[res][verb], "%s/%s should be allowed", res, verb)
		}
	}
	require.Contains(t, body.Allowed, resLogs)
	assert.True(t, body.Allowed[resLogs]["get"], "the logs capability should reflect the pods/log SSAR")
	// The console gates connect-a-provider on this; without it the SPA falls back to
	// optimistic-allow and re-opens the flow that refuses with the key already sent.
	require.Contains(t, body.Allowed, resSecrets)
	assert.True(t, body.Allowed[resSecrets][verbCreate], "the secrets capability should reflect the core-group SSAR")
}

// TestCapabilitiesProbesLogsGate proves the synthetic `logs` capability tracks the caller's
// `get pods/log` decision (M100 UI99-logs) — so the console can GATE the agent-detail Logs tab. A
// caller allowed pods/log gets logs.get=true; one denied it gets false (the tab is then hidden).
func TestCapabilitiesProbesLogsGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowPods bool
	}{
		{"can read logs", true},
		{"cannot read logs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				// Everything else allowed; pods/log gated by the case (resource "pods" is the log probe).
				WithInterceptorFuncs(ssrInterceptorForLogs(tc.allowPods)).
				Build()
			s := newCallerServer(t, &fakeCallerClientFactory{client: c})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/capabilities?namespace=prod", nil)
			req.Header.Set("Authorization", "Bearer some-token")
			s.Handler().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var body CapabilitiesResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body.Allowed, resLogs)
			assert.Equal(t, tc.allowPods, body.Allowed[resLogs]["get"],
				"logs.get must mirror the pods/log SSAR decision")
		})
	}
}

// ssrInterceptorForLogs answers SSARs: every agents-group golden probe is allowed, and the core
// `pods`/log probe is allowed iff allowPods. Models a persona whose only relevant difference is
// pod-log access, so the Logs-tab gate is exercised in isolation.
func ssrInterceptorForLogs(allowPods bool) interceptor.Funcs {
	return ssarInterceptor(func(resource, _, _ string) bool {
		if resource == "pods" {
			return allowPods
		}
		return true
	})
}

// TestCapabilitiesPartialDenials proves a viewer-shaped RBAC (reads allowed,
// writes denied) maps correctly cell-by-cell — the honest input for the console
// to disable write affordances.
func TestCapabilitiesPartialDenials(t *testing.T) {
	readVerbs := map[string]bool{"get": true, "list": true}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssarInterceptor(func(_, verb, _ string) bool {
			// The viewer shape: read verbs allowed, write verbs denied — regardless
			// of resource or namespace.
			return readVerbs[verb]
		})).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities?namespace=team-a", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body CapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, res := range goldenResources {
		assert.True(t, body.Allowed[res]["get"], "%s/get should be allowed for a viewer", res)
		assert.True(t, body.Allowed[res]["list"], "%s/list should be allowed for a viewer", res)
		assert.False(t, body.Allowed[res]["create"], "%s/create must be denied for a viewer", res)
		assert.False(t, body.Allowed[res]["update"], "%s/update must be denied for a viewer", res)
		assert.False(t, body.Allowed[res]["delete"], "%s/delete must be denied for a viewer", res)
	}
}

// TestCapabilitiesClusterWideNamespace proves an absent ?namespace probes the
// empty namespace (cluster-wide / "all namespaces") and echoes "".
func TestCapabilitiesClusterWideNamespace(t *testing.T) {
	var (
		mu         sync.Mutex
		namespaces = map[string]bool{}
	)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssarInterceptor(func(_, _, namespace string) bool {
			// The SSARs run concurrently; record every namespace seen.
			mu.Lock()
			namespaces[namespace] = true
			mu.Unlock()
			return true
		})).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, map[string]bool{"": true}, namespaces,
		"absent namespace probes the empty (all-namespaces) namespace")
	var body CapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.Namespace)
}

// TestCapabilitiesAPIErrorSurfaced proves a true API error on a SSAR (not an
// allow/deny answer) fails the whole request honestly rather than reporting a
// half-filled matrix as success.
func TestCapabilitiesAPIErrorSurfaced(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewInternalError(assert.AnError)
			},
		}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"allowed"`,
		"a probe error must not report a partial matrix as success")
}

// --- GET /api/namespaces ----------------------------------------------------

// TestNamespacesLists proves the handler lists namespaces through the caller's
// client and projects them to sorted flat summaries.
func TestNamespacesLists(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}},
		).
		Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/namespaces", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "viewer-persona-token", factory.gotToken)
	var body NamespaceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Namespaces, 3)
	assert.Equal(t, []NamespaceSummary{{Name: "dev"}, {Name: "prod"}, {Name: "staging"}}, body.Namespaces)
}

// TestNamespacesEmptyNotNull proves an empty result is {"namespaces":[]} (not
// null) — an honest "no namespaces" state distinct from a denial.
func TestNamespacesEmptyNotNull(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/namespaces", nil)
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"namespaces":[]}`, rec.Body.String())
}

// TestNamespacesForbiddenIs403 proves a caller whose RBAC cannot list namespaces
// gets an honest 403 with an error body — NEVER a silent {"namespaces":[]} that
// would masquerade as "no namespaces exist".
func TestNamespacesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "namespaces"}, "", assert.AnError)
			},
		}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/namespaces", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code,
		"a namespace read denial must be a 403, not a swallowed empty list")
	assert.NotContains(t, rec.Body.String(), `"namespaces"`,
		"the 403 body must NOT be the empty-list success shape")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}

// TestNamespacesNoTokenIs401 proves a token-less namespaces request is rejected
// 401 by the factory before any list.
func TestNamespacesNoTokenIs401(t *testing.T) {
	listed := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorListFlag(&listed)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/namespaces", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, listed, "no namespace list must run for a token-less request")
}

// TestNamespacesDisplayNameAnnotation proves the display-name annotation is
// projected into the NamespaceSummary.DisplayName field and omitted when unset.
func TestNamespacesDisplayNameAnnotation(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:        "prod",
				Annotations: map[string]string{annNamespaceDisplayName: "Production"},
			}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev"}},
		).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/namespaces", nil)
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body NamespaceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Namespaces, 2)
	// dev: no annotation → DisplayName is empty (omitted from wire).
	assert.Equal(t, "dev", body.Namespaces[0].Name)
	assert.Empty(t, body.Namespaces[0].DisplayName)
	// prod: annotation present → DisplayName populated.
	assert.Equal(t, "prod", body.Namespaces[1].Name)
	assert.Equal(t, "Production", body.Namespaces[1].DisplayName)
	// On the wire, displayName must be absent for dev (omitempty).
	assert.NotContains(t, rec.Body.String(), `"displayName":""`)
}

// --- PUT /api/namespaces/{name}/display-name --------------------------------

// TestSetNamespaceDisplayNameSets proves a PUT with a non-empty displayName
// stamps the annotation on the namespace and returns the updated summary.
func TestSetNamespaceDisplayNameSets(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body := `{"displayName":"Production"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/namespaces/prod/display-name",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var summary NamespaceSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Equal(t, "prod", summary.Name)
	assert.Equal(t, "Production", summary.DisplayName)

	// Confirm the annotation was written through to the fake store.
	var ns corev1.Namespace
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "prod"}, &ns))
	assert.Equal(t, "Production", ns.Annotations[annNamespaceDisplayName])
}

// TestSetNamespaceDisplayNameClears proves an empty displayName removes the
// annotation from the namespace.
func TestSetNamespaceDisplayNameClears(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        "prod",
			Annotations: map[string]string{annNamespaceDisplayName: "Production"},
		}}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body := `{"displayName":""}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/namespaces/prod/display-name",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var summary NamespaceSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Equal(t, "prod", summary.Name)
	assert.Empty(t, summary.DisplayName)

	// Annotation removed from the store.
	var ns corev1.Namespace
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "prod"}, &ns))
	assert.Empty(t, ns.Annotations[annNamespaceDisplayName])
}

// TestSetNamespaceDisplayNameForbiddenIs403 proves a caller denied "update
// namespaces" by the API server gets an honest 403 — never a silent success.
func TestSetNamespaceDisplayNameForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "namespaces"}, "prod", assert.AnError)
			},
		}).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/namespaces/prod/display-name",
		strings.NewReader(`{"displayName":"x"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}
