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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// TestProviderReconnect_RotatesTheKeyAgainstARealAPIServer connects a provider and then
// connects it again with a new key, as a user holding only the shipped personas. The API server
// refuses a custom-resource update without metadata.resourceVersion (a core Secret may be updated
// without one), and the fake client does not model that, so only a real API server can prove the
// second connect works. The user holds no read verb on Secrets (ADR 0136).
func TestProviderReconnect_RotatesTheKeyAgainstARealAPIServer(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	credentialAdmin := installPersona(t, admin, "ctxmesh_credential_admin_role.yaml")
	for _, persona := range []string{"ctxmesh_developer_role.yaml", "ctxmesh_operator_role.yaml"} {
		t.Run(persona, func(t *testing.T) {
			role := installPersona(t, admin, persona)
			ns := "reconnect-" + strings.TrimPrefix(role, "ctxmesh-")
			user := "alice-" + ns
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
			for _, r := range []string{role, credentialAdmin} {
				require.NoError(t, admin.Create(ctx, &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: r, Namespace: ns},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r},
					Subjects:   []rbacv1.Subject{{Kind: "User", Name: user, APIGroup: rbacv1.GroupName}},
				}))
			}
			authed, err := rbacEnv.AddUser(envtest.User{Name: user, Groups: []string{"system:authenticated"}}, nil)
			require.NoError(t, err)
			caller, err := client.New(authed.Config(), client.Options{Scheme: testScheme(t)})
			require.NoError(t, err)

			var secret corev1.Secret
			err = caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: "anything"}, &secret)
			require.True(t, apierrors.IsForbidden(err), "the user must not be able to read Secrets; got %v", err)

			prov := anyKeyProvider(t)
			s, _, _ := newConnectServer(t, caller)
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

			var route agentsv1alpha1.ModelRoute
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &route))
			require.Len(t, route.Spec.Providers, 1)
			assert.Equal(t, prov.URL, route.Spec.Providers[0].APIBase)
			var binding agentsv1alpha1.SecretBinding
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerCustom}, &binding))
			assert.Equal(t, providerCustom, binding.Spec.SecretRef.Name)
		})
	}
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
