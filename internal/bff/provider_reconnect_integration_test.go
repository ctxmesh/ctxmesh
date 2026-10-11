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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// TestProviderReconnect_RotatesTheKeyAgainstARealAPIServer connects a provider and then
// connects it again with a new key, as a user holding only the shipped personas. The API server
// refuses a custom-resource update without metadata.resourceVersion (a core Secret may be updated
// without one). The fake client models that rule too, but its dry-run checks nothing and it
// enforces no RBAC, so the pre-flight and the personas are proven here. The user holds no read
// verb on Secrets (ADR 0136).
func TestProviderReconnect_RotatesTheKeyAgainstARealAPIServer(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	credentialAdmin := installPersona(t, admin, "ctxmesh_credential_admin_role.yaml")
	for _, persona := range []string{"ctxmesh_developer_role.yaml", "ctxmesh_operator_role.yaml"} {
		t.Run(persona, func(t *testing.T) {
			role := installPersona(t, admin, persona)
			ns := "reconnect-" + strings.TrimPrefix(role, "ctxmesh-")
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
			caller := personaUser(t, admin, ns, "alice-"+ns, role, credentialAdmin)
			var secret corev1.Secret

			prov := anyKeyProvider(t)
			s, _, _ := newRBACConnectServer(t, caller)
			connect := func(key string) {
				t.Helper()
				body, err := json.Marshal(ConnectProviderRequest{
					Provider: providerCustom, APIKey: key, BaseURL: prov.URL, Namespace: ns,
				})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer user-token")
				s.Handler().ServeHTTP(rec, req)
				require.Equal(t, http.StatusCreated, rec.Code, "connect with %s: %s", key, rec.Body.String())
			}

			connect("sk-first-key")
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &secret))
			assert.Equal(t, "sk-first-key", string(secret.Data[secretKeyAPIKey]))

			connect("sk-rotated-key")
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &secret))
			assert.Equal(t, "sk-rotated-key", string(secret.Data[secretKeyAPIKey]), "the second connect rotates the key")

			var route agentsv1beta1.ModelRoute
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &route))
			require.Len(t, route.Spec.Providers, 1)
			assert.Equal(t, prov.URL, route.Spec.Providers[0].APIBase)
			var binding agentsv1beta1.SecretBinding
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &binding))
			assert.Equal(t, providerCustom, binding.Spec.SecretRef.Name)
		})
	}
}

// TestProviderRotate_RewritesTheKeyWithoutReadingIt drives the console's Rotate key action,
// POST /api/providers/{name}/rotate, as a user holding only the shipped personas. No shipped
// persona grants get on Secrets and none may (ADR 0136), so a rotate that reads the stored Secret
// is refused for everyone while the console offers the button.
func TestProviderRotate_RewritesTheKeyWithoutReadingIt(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	credentialAdmin := installPersona(t, admin, "ctxmesh_credential_admin_role.yaml")
	for _, persona := range []string{"ctxmesh_developer_role.yaml", "ctxmesh_operator_role.yaml"} {
		t.Run(persona, func(t *testing.T) {
			role := installPersona(t, admin, persona)
			ns := "rotate-" + strings.TrimPrefix(role, "ctxmesh-")
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
			alice := personaUser(t, admin, ns, "alice-"+ns, role, credentialAdmin)
			bob := personaUser(t, admin, ns, "bob-"+ns, role)

			prov := anyKeyProvider(t)
			aliceSrv, _, _ := newRBACConnectServer(t, alice)
			body, err := json.Marshal(ConnectProviderRequest{
				Provider: providerCustom, APIKey: "sk-first-key", BaseURL: prov.URL, Namespace: ns,
			})
			require.NoError(t, err)
			rec := postAs(aliceSrv, "/api/providers", body)
			require.Equal(t, http.StatusCreated, rec.Code, "connect: %s", rec.Body.String())

			rotate := func(s *Server, key string) *httptest.ResponseRecorder {
				t.Helper()
				body, err := json.Marshal(RotateProviderKeyRequest{APIKey: key, Namespace: ns})
				require.NoError(t, err)
				return postAs(s, "/api/providers/"+providerCustom+"/rotate", body)
			}

			rec = rotate(aliceSrv, "sk-rotated-key")
			require.Equal(t, http.StatusOK, rec.Code, "rotate: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "sk-rotated-key", "the key never appears in the response")
			var secret corev1.Secret
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &secret))
			assert.Equal(t, "sk-rotated-key", string(secret.Data[secretKeyAPIKey]), "the rotate stored the new key")
			assert.Equal(t, managedByConnect, secret.Labels[labelManagedBy], "the rebuilt Secret is still connect-managed")

			var route agentsv1beta1.ModelRoute
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &route))
			require.Len(t, route.Spec.Providers, 1)
			assert.Equal(t, prov.URL, route.Spec.Providers[0].APIBase, "the custom endpoint survives the rotate")
			assert.Equal(t, providerCustom, route.Labels[labelProvider])
			var binding agentsv1beta1.SecretBinding
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &binding))
			assert.Equal(t, providerCustom, binding.Spec.SecretRef.Name)

			// Without credential-admin the rotate is refused before anything is written, and the
			// refusal names the verb the user lacks.
			bobSrv, _, _ := newRBACConnectServer(t, bob)
			rec = rotate(bobSrv, "sk-bobs-key")
			require.Equal(t, http.StatusForbidden, rec.Code, "rotate without credential-admin: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "not allowed to update Secret")
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &secret))
			assert.Equal(t, "sk-rotated-key", string(secret.Data[secretKeyAPIKey]), "a refused rotate writes nothing")
			var after agentsv1beta1.ModelRoute
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &after))
			assert.Equal(t, route.ResourceVersion, after.ResourceVersion, "a refused rotate leaves the route untouched")

			rec = rotate(aliceSrv, "sk-another-key")
			require.Equal(t, http.StatusOK, rec.Code, "a second rotate: %s", rec.Body.String())

			body, err = json.Marshal(RotateProviderKeyRequest{APIKey: "sk-any", Namespace: ns})
			require.NoError(t, err)
			rec = postAs(aliceSrv, "/api/providers/absent/rotate", body)
			assert.Equal(t, http.StatusNotFound, rec.Code, "rotating a provider that is not connected: %s", rec.Body.String())
		})
	}
}

// TestProviderConnect_RefusesAModelRouteItDidNotCreate: a connection name can name a ModelRoute
// made by hand or by GitOps. Connecting under that name is refused with 409 by the pre-flight,
// before the Secret is written, and the route is left as it was. The fake client's dry-run checks
// nothing, so only a real API server shows that the refusal comes before any write.
func TestProviderConnect_RefusesAModelRouteItDidNotCreate(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	credentialAdmin := installPersona(t, admin, "ctxmesh_credential_admin_role.yaml")
	role := installPersona(t, admin, "ctxmesh_developer_role.yaml")
	const ns = "connect-handmade"
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	alice := personaUser(t, admin, ns, "alice-"+ns, role, credentialAdmin)

	route := &agentsv1beta1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-llm", Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/instance": "argocd-prod"},
		},
		Spec: agentsv1beta1.ModelRouteSpec{Providers: []agentsv1beta1.ProviderRef{
			{Provider: "openai", Model: "gpt-5", Priority: 1, SecretBindingRef: "openai"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 2, SecretBindingRef: "anthropic"},
		}},
	}
	require.NoError(t, admin.Create(ctx, route))

	prov := anyKeyProvider(t)
	s, _, _ := newRBACConnectServer(t, alice)
	body, err := json.Marshal(ConnectProviderRequest{
		Provider: providerCustom, Connection: "prod-llm", APIKey: "sk-first-key", BaseURL: prov.URL, Namespace: ns,
	})
	require.NoError(t, err)
	rec := postAs(s, "/api/providers", body)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `ModelRoute \"prod-llm\" exists and was not created by connecting a provider`)

	var after agentsv1beta1.ModelRoute
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(route), &after))
	assert.Equal(t, route.ResourceVersion, after.ResourceVersion, "the route is untouched")
	assert.Len(t, after.Spec.Providers, 2)
	for _, o := range []client.Object{&corev1.Secret{}, &agentsv1beta1.SecretBinding{}} {
		err := admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: "prod-llm"}, o)
		assert.True(t, apierrors.IsNotFound(err), "%T must not be written by a refused connect; got %v", o, err)
	}
}

// TestProviderRotate_RefusesARouteRepointedDuringTheProbe: an operator re-points the route while the
// rotate is probing the provider with the new key. The API server bumps the route's generation, the
// rotate's pre-flight sees it and refuses with 409, and nothing is written: the re-point stays and so
// does the old key. Status writes do not bump the generation, so the controller's never trip this.
func TestProviderRotate_RefusesARouteRepointedDuringTheProbe(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	credentialAdmin := installPersona(t, admin, "ctxmesh_credential_admin_role.yaml")
	role := installPersona(t, admin, "ctxmesh_developer_role.yaml")
	const (
		ns        = "rotate-repointed"
		repointed = "https://llm-eu.example.internal/v1"
	)
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	alice := personaUser(t, admin, ns, "alice-"+ns, role, credentialAdmin)

	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-rotated-key" {
			var route agentsv1beta1.ModelRoute
			if err := admin.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: providerCustom}, &route); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			route.Spec.Providers[0].APIBase = repointed
			if err := admin.Update(r.Context(), &route); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-large"}]}`))
	}))
	t.Cleanup(prov.Close)

	s, _, _ := newRBACConnectServer(t, alice)
	body, err := json.Marshal(ConnectProviderRequest{
		Provider: providerCustom, APIKey: "sk-first-key", BaseURL: prov.URL, Namespace: ns,
	})
	require.NoError(t, err)
	rec := postAs(s, "/api/providers", body)
	require.Equal(t, http.StatusCreated, rec.Code, "connect: %s", rec.Body.String())

	body, err = json.Marshal(RotateProviderKeyRequest{APIKey: "sk-rotated-key", Namespace: ns})
	require.NoError(t, err)
	rec = postAs(s, "/api/providers/"+providerCustom+"/rotate", body)
	require.Equal(t, http.StatusConflict, rec.Code, "rotate: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "was changed while the key was being rotated")

	var route agentsv1beta1.ModelRoute
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &route))
	assert.Equal(t, repointed, route.Spec.Providers[0].APIBase, "the re-point is kept")
	var secret corev1.Secret
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &secret))
	assert.Equal(t, "sk-first-key", string(secret.Data[secretKeyAPIKey]), "a refused rotate writes nothing")
}

// newRBACConnectServer is newConnectServer with the production authorizer, so connect's
// pre-flight SelfSubjectAccessReviews are answered by the real API server and the shipped
// personas are proven to pass it, not an allow-all stand-in.
func newRBACConnectServer(t *testing.T, c client.Client) (*Server, *fakeCallerClientFactory, *logBuffer) {
	t.Helper()
	s, f, lb := newConnectServer(t, c)
	s.authorizer = authz.SSARAuthorizer{}
	return s, f, lb
}

// personaUser binds user to the given ClusterRoles in ns, asserts the user cannot read Secrets
// there, and returns a client acting as that user.
func personaUser(t *testing.T, admin client.Client, ns, user string, roles ...string) client.Client {
	t.Helper()
	ctx := context.Background()
	for _, r := range roles {
		require.NoError(t, admin.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: user + "-" + r, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r},
			Subjects:   []rbacv1.Subject{{Kind: "User", Name: user, APIGroup: rbacv1.GroupName}},
		}))
	}
	authed, err := rbacEnv.AddUser(envtest.User{Name: user, Groups: []string{"system:authenticated"}}, nil)
	require.NoError(t, err)
	c, err := client.New(authed.Config(), client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	var secret corev1.Secret
	err = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "anything"}, &secret)
	require.True(t, apierrors.IsForbidden(err), "%s must not be able to read Secrets; got %v", user, err)
	return c
}

// installPersona creates a shipped persona ClusterRole from config/rbac under its built name
// (config/default's namePrefix ctxmesh-) and returns that name.
func installPersona(t *testing.T, admin client.Client, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", file))
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(raw, &role))
	require.Equal(t, "ClusterRole", role.Kind, "%s must hold one ClusterRole", file)
	role.Name = "ctxmesh-" + role.Name
	if err := admin.Create(context.Background(), &role); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	return role.Name
}

// anyKeyProvider is an OpenAI-compatible endpoint that lists one model for any bearer key.
func anyKeyProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-large"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}
