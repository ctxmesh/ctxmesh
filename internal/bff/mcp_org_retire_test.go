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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agentry/internal/credresolve"
)

// verbAuthorizer allows every verb except those in deny — it lets a test model a
// caller who can READ but not UPDATE, the exact distinction the org-promote admin
// gate turns on.
type verbAuthorizer struct {
	deny map[string]bool
	last authz.Action
}

func (a *verbAuthorizer) Authorize(_ context.Context, _ client.Client, action authz.Action) error {
	a.last = action
	if a.deny[action.Verb] {
		return authz.ErrForbidden
	}
	return nil
}

func retireOrgServer(t *testing.T, c client.Client, auth authz.Authorizer) (*Server, toolregistry.Store) {
	t.Helper()
	s, _, _ := newMCPServer(t, c, false)
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store
	s.authorizer = auth
	_, err := store.Upsert(context.Background(),
		crdToolRegistryToStore(scopedRegistry("scalekit", scopePersonal, userGrantHash("alice@example.com"))))
	require.NoError(t, err)
	return s, store
}

// Retired org-promote: the scope flip persists to the STORE, the shared org
// credential Secret is written server-side, and the SSAR was the update verb.
func TestSetOrgCredential_RetirePromotesInStore(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	auth := &verbAuthorizer{}
	s, store := retireOrgServer(t, c, auth)

	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod", Credential: "SHARED-ORG-KEY"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := store.Get(ctx, "prod", "scalekit")
	require.NoError(t, err)
	assert.Equal(t, scopeOrg, after.Labels[labelMCPScope])
	_, hasOwner := after.Labels[labelMCPOwner]
	assert.False(t, hasOwner, "org scope drops the single-owner label")
	assert.Equal(t, authz.VerbUpdate, auth.last.Verb)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: credresolve.OrgSecretName("scalekit"), Namespace: "prod"}, &sec))
	assert.Equal(t, "SHARED-ORG-KEY", string(sec.Data[credresolve.KeyOrgCredential]))
	assert.NotContains(t, rec.Body.String(), "SHARED-ORG-KEY")
}

// THE CRUX (Fable): a caller who can READ but not UPDATE the ToolRegistry is denied
// 403, and NO promotion + NO org credential is delivered. The SSAR VerbUpdate IS the
// security boundary — it survives retirement intact.
func TestSetOrgCredential_RetireUpdateDeniedNoCredential(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, store := retireOrgServer(t, c, &verbAuthorizer{deny: map[string]bool{authz.VerbUpdate: true}})

	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod", Credential: "SHARED-ORG-KEY"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer viewer-tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	after, err := store.Get(ctx, "prod", "scalekit")
	require.NoError(t, err)
	assert.Equal(t, scopePersonal, after.Labels[labelMCPScope], "a denied promote does NOT change scope")
	var sec corev1.Secret
	err = c.Get(ctx, client.ObjectKey{Name: credresolve.OrgSecretName("scalekit"), Namespace: "prod"}, &sec)
	assert.True(t, apierrors.IsNotFound(err), "a denied promote delivers NO org credential")
}
