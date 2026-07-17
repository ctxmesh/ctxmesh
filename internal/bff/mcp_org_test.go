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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// TestSetOrgCredentialPromotesAndStores proves the admin flow: a registered personal server
// is promoted to org scope (owner label dropped) and its shared credential is stored in the
// org Secret — so every invoker resolves it (no per-user consent).
func TestSetOrgCredentialPromotesAndStores(t *testing.T) {
	ctx := context.Background()
	tr := scopedRegistry("scalekit", scopePersonal, userGrantHash("alice@example.com"))
	// No SSR interceptor: handleSetOrgCredential doesn't resolve the caller identity (the
	// admin gate is the caller-scoped ToolRegistry update), and ssrInterceptor swallows every
	// non-SSR create — which would silently drop the org Secret write.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr).Build()
	s, _, _ := newMCPServer(t, c, false)

	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod", Credential: "SHARED-ORG-KEY"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// The ToolRegistry is now org-scoped with no owner.
	var after agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "scalekit", Namespace: "prod"}, &after))
	assert.Equal(t, scopeOrg, after.Labels[labelMCPScope])
	_, hasOwner := after.Labels[labelMCPOwner]
	assert.False(t, hasOwner, "org scope drops the single-owner label")

	// The shared credential is stored in the org Secret (server-side only).
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: credresolve.OrgSecretName("scalekit"), Namespace: "prod"}, &sec))
	assert.Equal(t, "SHARED-ORG-KEY", string(sec.Data[credresolve.KeyOrgCredential]))
	assert.Equal(t, credresolve.ManagedByOrgCredential, sec.Labels[credresolve.LabelManagedBy])

	// The credential never appears in the response.
	assert.NotContains(t, rec.Body.String(), "SHARED-ORG-KEY")
}

// TestSetOrgCredentialRejectsMissingFields proves required-field validation.
func TestSetOrgCredentialRejectsMissingFields(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod"}) // no credential
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
