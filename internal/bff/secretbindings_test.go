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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// sbNS is the namespace used in SecretBinding tests.
const sbNS = "team-b"

// --- fixture helpers ---------------------------------------------------------

// mockSecretBinding builds a SecretBinding pointing at a Kubernetes Secret.
// The Secret object itself is NOT created — only the reference (name+key) is
// stored in the SecretBinding spec. This mirrors production: the BFF never
// reads the Secret's data, only the CRD's reference fields.
func mockSecretBinding(name, ns, secretName, secretKey string) *agentsv1alpha1.SecretBinding {
	return &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  secretKey,
			},
		},
	}
}

// resolvedSB sets the "Resolved" condition on a SecretBinding (simulates a
// reconciled object where the referenced Secret exists and has the key).
func resolvedSB(sb *agentsv1alpha1.SecretBinding) *agentsv1alpha1.SecretBinding {
	sb.Status.Conditions = []metav1.Condition{
		{
			Type:               "Resolved",
			Status:             metav1.ConditionTrue,
			Reason:             "SecretFound",
			Message:            "referenced secret found",
			LastTransitionTime: metav1.Now(),
		},
	}
	return sb
}

// --- request helpers ---------------------------------------------------------

// getSecretBindings drives GET /api/secretbindings with a caller token.
func getSecretBindings(t *testing.T, s *Server, rawQuery string) (SecretBindingListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/secretbindings"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body SecretBindingListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// getSecretBinding drives GET /api/secretbindings/{sbNS}/{name} with a caller token.
func getSecretBinding(t *testing.T, s *Server, name string) (*SecretBindingDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/secretbindings/"+sbNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail SecretBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// createSecretBinding drives POST /api/secretbindings with the given body.
func createSecretBinding(t *testing.T, s *Server, reqBody SecretBindingCreateRequest) (*SecretBindingDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/secretbindings", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail SecretBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// putSecretBinding drives PUT /api/secretbindings/sbNS/{name} with the given body.
// The name parameter is kept for future multi-namespace or multi-name tests —
// the unparam linter is suppressed because the signature should not be over-specialised.
//
//nolint:unparam
func putSecretBinding(t *testing.T, s *Server, name string, reqBody SecretBindingUpdateRequest) (*SecretBindingDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/secretbindings/"+sbNS+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail SecretBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// deleteSecretBinding drives DELETE /api/secretbindings/sbNS/{name} with a caller token.
func deleteSecretBinding(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/secretbindings/"+sbNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/secretbindings — list contract
// =============================================================================

// TestListSecretBindingsEmpty proves an empty cluster yields
// {"items":[],"nextCursor":""} — never null slices.
func TestListSecretBindingsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getSecretBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListSecretBindingsReturnsItems proves seeded SecretBindings appear in the
// response with the correct projections (ref metadata, never the secret value).
func TestListSecretBindingsReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockSecretBinding("openai-key", sbNS, "openai-secret", "api-key"),
		mockSecretBinding("anthropic-key", sbNS, "anthropic-secret", "api-key"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getSecretBindings(t, s, "namespace="+sbNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, sbNS, item.Namespace)
		assert.Equal(t, "kubernetes", item.Backend)
		assert.NotEmpty(t, item.SecretRef.Name, "secretRef.name must be present")
		assert.NotEmpty(t, item.SecretRef.Key, "secretRef.key must be present")
	}
	assert.True(t, names["openai-key"])
	assert.True(t, names["anthropic-key"])
}

// TestListSecretBindingsQFilter proves ?q is a case-insensitive windowed
// substring filter on the binding name.
func TestListSecretBindingsQFilter(t *testing.T) {
	objs := []client.Object{
		mockSecretBinding("anthropic-prod", sbNS, "anthropic-secret", "api-key"),
		mockSecretBinding("ANTHROPIC-dev", sbNS, "anthropic-dev-secret", "api-key"),
		mockSecretBinding("openai-prod", sbNS, "openai-secret", "api-key"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getSecretBindings(t, s, "q=anthropic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2, "q must match both anthropic variants case-insensitively")
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["anthropic-prod"])
	assert.True(t, names["ANTHROPIC-dev"])
	assert.False(t, names["openai-prod"])

	// No match → [] not null.
	body, code = getSecretBindings(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListSecretBindingsNamespaceScoping proves ?namespace scopes the list to
// one namespace and an absent ?namespace returns all namespaces.
func TestListSecretBindingsNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockSecretBinding("prod-key", "prod", "prod-secret", "api-key"),
		mockSecretBinding("dev-key", "dev", "dev-secret", "api-key"),
		mockSecretBinding("dev-key2", "dev", "dev-secret2", "api-key"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Scoped.
	body, code := getSecretBindings(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	// Unscoped → all.
	body, code = getSecretBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 3)
}

// TestListSecretBindingsPaging proves limit/cursor paging works with
// the list contract.
func TestListSecretBindingsPaging(t *testing.T) {
	all := []*agentsv1alpha1.SecretBinding{
		mockSecretBinding("key-000", sbNS, "secret-000", "api-key"),
		mockSecretBinding("key-001", sbNS, "secret-001", "api-key"),
		mockSecretBinding("key-002", sbNS, "secret-002", "api-key"),
		mockSecretBinding("key-003", sbNS, "secret-003", "api-key"),
		mockSecretBinding("key-004", sbNS, "secret-004", "api-key"),
	}

	pagingFn := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			lo.ApplyOptions(opts)
			start := 0
			if lo.Continue != "" {
				n, err := strconv.Atoi(lo.Continue)
				if err != nil {
					return fmt.Errorf("bad continue token %q", lo.Continue)
				}
				start = n
			}
			end := len(all)
			if lo.Limit > 0 && start+int(lo.Limit) < end {
				end = start + int(lo.Limit)
			}
			sbList, ok := list.(*agentsv1alpha1.SecretBindingList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, sb := range all[start:end] {
				sbList.Items = append(sbList.Items, *sb)
			}
			if end < len(all) {
				sbList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(pagingFn).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Page 1: limit=2 → 2 items + nextCursor.
	page1, code := getSecretBindings(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor, "a non-exhausted list must expose a nextCursor")

	// Page 2 via cursor round-trip.
	page2, code := getSecretBindings(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name, "page 2 must be a different window")

	// Drain to exhaustion.
	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getSecretBindings(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every binding exactly once")
}

// TestListSecretBindingsForbiddenIs403 proves a Forbidden on the list surfaces
// as 403, not an empty [].
func TestListSecretBindingsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "secretbindings"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getSecretBindings(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/secretbindings/{ns}/{name} — detail
// SECURITY: no secret value/data in the response
// =============================================================================

// TestGetSecretBindingReturnsDetail proves a seeded SecretBinding is returned
// with all ref fields projected correctly — name, secretRef.name, secretRef.key,
// backend, phase, ready. The referenced Secret's data is NEVER in the response.
func TestGetSecretBindingReturnsDetail(t *testing.T) {
	sb := resolvedSB(mockSecretBinding("my-binding", sbNS, "my-secret", "api-key"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getSecretBinding(t, s, "my-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-binding", detail.Name)
	assert.Equal(t, sbNS, detail.Namespace)
	assert.Equal(t, "kubernetes", detail.Backend)
	assert.Equal(t, "my-secret", detail.SecretRef.Name, "secretRef.name must be the Secret name")
	assert.Equal(t, "api-key", detail.SecretRef.Key, "secretRef.key must be the key within the Secret")
	assert.True(t, detail.Ready, "Resolved condition → ready=true")
	assert.Equal(t, phaseReady, detail.Phase)
}

// TestGetSecretBindingNoValueInResponse is THE NO-ECHO ASSERTION. It proves
// that even if a Kubernetes Secret with matching name exists in the fake store,
// the BFF response NEVER includes a "value", "data", or "credential" field.
// The BFF reads only the SecretBinding CRD — it never fetches the Secret object.
func TestGetSecretBindingNoValueInResponse(t *testing.T) {
	sb := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")

	// NOTE: we do NOT add a corev1.Secret to the fake client. If the handler
	// were to try fetching it and return its data, the test would prove it fails
	// (no Secret in store → error). But more importantly: we assert the JSON
	// shape itself never contains a value/data/credential field — regardless of
	// whether a Secret exists or not.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/secretbindings/"+sbNS+"/my-binding", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Parse the raw JSON body into a generic map to check for forbidden fields.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "response must be valid JSON")

	// THE NO-ECHO PROPERTY: none of these keys may appear in the response.
	forbiddenKeys := []string{"value", "data", "credential", "secret_value", "apiKey", "api_key"}
	for _, k := range forbiddenKeys {
		_, present := raw[k]
		assert.False(t, present, "response must NOT contain field %q (secret material must never be echoed)", k)
	}

	// The secretRef sub-object may appear — but it must ONLY have name and key
	// (the reference metadata), never a value field nested inside it.
	if ref, ok := raw["secretRef"].(map[string]any); ok {
		_, valuePresent := ref["value"]
		assert.False(t, valuePresent, "secretRef must NOT contain a 'value' field")
		_, dataPresent := ref["data"]
		assert.False(t, dataPresent, "secretRef must NOT contain a 'data' field")
		// Confirm the ref metadata IS present (the positive assertion).
		assert.Equal(t, "my-secret", ref["name"], "secretRef.name must be the Secret name")
		assert.Equal(t, "api-key", ref["key"], "secretRef.key must be the key within the Secret")
	} else {
		t.Error("response must contain a secretRef object")
	}
}

// TestListSecretBindingsNoValueInResponse asserts the list response items
// also never contain a secret value/data field.
func TestListSecretBindingsNoValueInResponse(t *testing.T) {
	sb := mockSecretBinding("binding-a", sbNS, "real-secret", "password")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/secretbindings?namespace="+sbNS, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))

	items, ok := raw["items"].([]any)
	require.True(t, ok, "response must have an items array")
	require.NotEmpty(t, items)

	for _, itemAny := range items {
		item, ok := itemAny.(map[string]any)
		require.True(t, ok)
		for _, k := range []string{"value", "data", "credential", "apiKey"} {
			_, present := item[k]
			assert.False(t, present, "list item must NOT contain field %q", k)
		}
		if ref, ok := item["secretRef"].(map[string]any); ok {
			_, valuePresent := ref["value"]
			assert.False(t, valuePresent, "secretRef in list item must NOT contain 'value'")
		}
	}
}

// TestGetSecretBindingNotFoundIs404 proves a missing SecretBinding yields 404.
func TestGetSecretBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getSecretBinding(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetSecretBindingForbiddenIs403 proves a caller denied Get sees an honest 403.
func TestGetSecretBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "secretbindings"},
					"my-binding", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getSecretBinding(t, s, "my-binding")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/secretbindings — create
// =============================================================================

// TestCreateSecretBindingSucceeds proves a valid SecretBinding create returns
// 201 with the ref DTO — and never a secret value.
func TestCreateSecretBindingSucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Name:      "my-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	detail, code, body := createSecretBinding(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-binding", detail.Name)
	assert.Equal(t, sbNS, detail.Namespace)
	assert.Equal(t, "kubernetes", detail.Backend)
	assert.Equal(t, "my-secret", detail.SecretRef.Name)
	assert.Equal(t, "api-key", detail.SecretRef.Key)

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.SecretBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sbNS, Name: "my-binding"}, &got))
	assert.Equal(t, "my-secret", got.Spec.SecretRef.Name)
	assert.Equal(t, "api-key", got.Spec.SecretRef.Key)
}

// TestCreateSecretBindingNoValueInResponse proves the create response body
// never contains a secret value/data field.
func TestCreateSecretBindingNoValueInResponse(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Try to smuggle a "value" field in the body — the Go decoder should
	// ignore any field not in SecretBindingCreateRequest.
	smuggleBody := `{"name":"smuggle-binding","namespace":"` + sbNS + `","secretRef":{"name":"my-secret","key":"api-key"},"value":"SK-LEAKED","data":"also-leaked"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/secretbindings", strings.NewReader(smuggleBody))
	req.Header.Set("Authorization", "Bearer caller-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, k := range []string{"value", "data", "credential"} {
		_, present := raw[k]
		assert.False(t, present, "create response must NOT echo field %q", k)
	}

	// The persisted CRD must also not have leaked the value.
	var got agentsv1alpha1.SecretBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sbNS, Name: "smuggle-binding"}, &got))
	assert.Equal(t, "my-secret", got.Spec.SecretRef.Name)
	assert.Equal(t, "api-key", got.Spec.SecretRef.Key)
	// SecretKeyRef has no value field — this is a compile-time guarantee:
	// the type has only Name and Key, so any "value" in the JSON body is
	// silently ignored and can never be persisted.
	assert.Equal(t, "kubernetes", got.Spec.Backend) // default applied by handler
}

// TestCreateSecretBindingMissingNameIs400 proves a missing name yields 400.
func TestCreateSecretBindingMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := createSecretBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateSecretBindingMissingSecretRefNameIs400 proves a missing
// secretRef.name yields 400.
func TestCreateSecretBindingMissingSecretRefNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Name:      "my-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Key: "api-key"}, // Name is empty
	}
	_, code, body := createSecretBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "secretRef.name")
}

// TestCreateSecretBindingMissingSecretRefKeyIs400 proves a missing
// secretRef.key yields 400.
func TestCreateSecretBindingMissingSecretRefKeyIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Name:      "my-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret"}, // Key is empty
	}
	_, code, body := createSecretBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "secretRef.key")
}

// TestCreateSecretBindingAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateSecretBindingAlreadyExistsIs409(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Name:      "my-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := createSecretBinding(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateSecretBindingForbiddenIs403 proves a viewer's create surfaces the
// API server's real 403.
func TestCreateSecretBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "secretbindings"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := SecretBindingCreateRequest{
		Name:      "no-perm-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := createSecretBinding(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateSecretBindingWithoutTokenIs401 proves a token-less POST is rejected
// 401 before any K8s call.
func TestCreateSecretBindingWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(SecretBindingCreateRequest{
		Name:      "binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "s", Key: "k"},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/secretbindings", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// TestCreateSecretBindingAPIServerRejectionSurfaces4xx proves that when the
// API server rejects a create, the BFF surfaces it as an honest 4xx (422),
// never a 500.
func TestCreateSecretBindingAPIServerRejectionSurfaces4xx(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: secretBindingKind},
					obj.GetName(),
					nil,
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingCreateRequest{
		Name:      "bad-binding",
		Namespace: sbNS,
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := createSecretBinding(t, s, req)
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
}

// =============================================================================
// PUT /api/secretbindings/{ns}/{name} — update via SSA
// =============================================================================

// TestUpdateSecretBindingEditsField proves a PUT edits the secretRef via SSA
// and the changed value is visible in the fake store.
func TestUpdateSecretBindingEditsField(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "old-secret", "old-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingUpdateRequest{
		Name:      "my-binding",
		SecretRef: SecretRefDTO{Name: "new-secret", Key: "new-key"},
	}
	detail, code, body := putSecretBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "new-secret", detail.SecretRef.Name)
	assert.Equal(t, "new-key", detail.SecretRef.Key)

	// Confirm the change landed.
	var got agentsv1alpha1.SecretBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sbNS, Name: "my-binding"}, &got))
	assert.Equal(t, "new-secret", got.Spec.SecretRef.Name)
	assert.Equal(t, "new-key", got.Spec.SecretRef.Key)
}

// TestUpdateSecretBindingNoValueInResponse proves the PUT response never
// contains a secret value field.
func TestUpdateSecretBindingNoValueInResponse(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "old-secret", "old-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Try to smuggle a value in the PUT body.
	smuggleBody := `{"name":"my-binding","secretRef":{"name":"new-secret","key":"new-key"},"value":"LEAKED","data":"also-leaked"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/secretbindings/"+sbNS+"/my-binding", strings.NewReader(smuggleBody))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, k := range []string{"value", "data", "credential"} {
		_, present := raw[k]
		assert.False(t, present, "PUT response must NOT echo field %q", k)
	}
}

// TestUpdateSecretBindingRenameGuardIs400 proves a spec name that does not match
// the URL name is rejected 400 (a PUT is not a rename).
func TestUpdateSecretBindingRenameGuardIs400(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingUpdateRequest{
		Name:      "different-name", // mismatch
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := putSecretBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, strings.ToLower(body), "rename")
}

// TestUpdateSecretBindingAbsentNameInBodyIsOK proves omitting Name in the body
// does not trigger the rename guard.
func TestUpdateSecretBindingAbsentNameInBodyIsOK(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "old-secret", "old-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := SecretBindingUpdateRequest{
		// Name is empty — URL is authoritative.
		SecretRef: SecretRefDTO{Name: "updated-secret", Key: "updated-key"},
	}
	detail, code, body := putSecretBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "updated-secret", detail.SecretRef.Name)
}

// TestUpdateSecretBindingForbiddenIs403 proves a viewer's PUT surfaces the API
// server's real 403.
func TestUpdateSecretBindingForbiddenIs403(t *testing.T) {
	existing := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "secretbindings"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := SecretBindingUpdateRequest{
		SecretRef: SecretRefDTO{Name: "my-secret", Key: "api-key"},
	}
	_, code, body := putSecretBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateSecretBindingWithoutTokenIs401 proves a token-less PUT is rejected
// 401 before any K8s call.
func TestUpdateSecretBindingWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(SecretBindingUpdateRequest{
		SecretRef: SecretRefDTO{Name: "s", Key: "k"},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/secretbindings/"+sbNS+"/my-binding", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/secretbindings/{ns}/{name} — delete
// =============================================================================

// TestDeleteSecretBindingRemovesObject proves a DELETE succeeds (204) and the
// SecretBinding is gone from the fake store.
func TestDeleteSecretBindingRemovesObject(t *testing.T) {
	sb := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteSecretBinding(t, s, "my-binding")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.SecretBinding
	err := c.Get(context.Background(), client.ObjectKey{Namespace: sbNS, Name: "my-binding"}, &got)
	require.True(t, apierrors.IsNotFound(err), "SecretBinding must be gone after a successful DELETE")
}

// TestDeleteSecretBindingNotFoundIs404 proves deleting a missing binding yields 404.
func TestDeleteSecretBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteSecretBinding(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteSecretBindingForbiddenIs403 proves a viewer's DELETE surfaces the
// API server's real 403 — the BFF never pre-empts the decision (ADR 0011).
func TestDeleteSecretBindingForbiddenIs403(t *testing.T) {
	sb := mockSecretBinding("my-binding", sbNS, "my-secret", "api-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sb).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "secretbindings"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteSecretBinding(t, s, "my-binding")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteSecretBindingWithoutTokenIs401 proves a token-less DELETE is
// rejected 401 before any K8s call.
func TestDeleteSecretBindingWithoutTokenIs401(t *testing.T) {
	deleteCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				deleteCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/secretbindings/"+sbNS+"/my-binding", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
