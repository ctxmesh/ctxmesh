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

// Envtest-backed proof of the m25.1b security boundary (ADR 0029 §7): MCP grant
// Secrets live in a LOCKED credential namespace whose Secret reads are RBAC'd to the
// credential-component SA only, so a tenant sharing the cluster can never read another
// user's OAuth tokens. Runs only under `make test-integration` (build tag integration).
package bff

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	rbacEnv    *envtest.Environment
	rbacCfg    *rest.Config
	rbacScheme *runtime.Scheme
)

// TestMain bootstraps a bare envtest control plane (no CRDs — this suite exercises
// only core Secrets + RBAC) with the default authorization-mode=RBAC, so a scoped
// user's access is really enforced by the API server.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	rbacScheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(rbacScheme); err != nil {
		panic("add client-go scheme: " + err.Error())
	}

	rbacEnv = &envtest.Environment{}
	cfg, err := rbacEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}
	rbacCfg = cfg

	code := m.Run()
	_ = rbacEnv.Stop()
	os.Exit(code)
}

// TestCredentialNamespaceRBACReadIsolation is the m25.1b acceptance: with the locked
// namespace + a Role granting Secret access to the credential-component subject only,
// (1) the privileged credential client writes+reads a grant and the OBO resolver reads
// the token back, while (2) a tenant user is FORBIDDEN from reading that grant Secret —
// the cross-tenant token-disclosure hole the boundary closes.
func TestCredentialNamespaceRBACReadIsolation(t *testing.T) {
	ctx := context.Background()
	const (
		credNS       = "ae-credentials"
		sourceNs     = "team-alpha"
		server       = "weather-mcp"
		credSubject  = "agent-engine-credential-component"
		tenantUser   = "tenant-bob"
		accessToken  = "ALICE-OAUTH-ACCESS-TOKEN"
		grantForUser = "alice@example.com"
	)

	admin, err := client.New(rbacCfg, client.Options{Scheme: rbacScheme})
	require.NoError(t, err)

	// The locked platform namespace.
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: credNS}}))

	// RBAC: full Secret access in the credential namespace, bound to the
	// credential-component subject ONLY. No binding exists for any tenant — the
	// Helm chart will encode exactly this (credential-component SA + egress-gateway SA).
	require.NoError(t, admin.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "credential-component", Namespace: credNS},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}))
	require.NoError(t, admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "credential-component", Namespace: credNS},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "credential-component"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: credSubject, APIGroup: rbacv1.GroupName}},
	}))

	// Two scoped clients: the credential component (bound) and a tenant (unbound).
	credUser, err := rbacEnv.AddUser(envtest.User{Name: credSubject}, nil)
	require.NoError(t, err)
	credClient, err := client.New(credUser.Config(), client.Options{Scheme: rbacScheme})
	require.NoError(t, err)

	tenant, err := rbacEnv.AddUser(envtest.User{Name: tenantUser, Groups: []string{"system:authenticated"}}, nil)
	require.NoError(t, err)
	tenantClient, err := client.New(tenant.Config(), client.Options{Scheme: rbacScheme})
	require.NoError(t, err)

	// --- (1) the credential component writes the grant through the real coordinates ---
	userHash := userGrantHash(grantForUser)
	grantNS, grantName := grantSecretCoordinates(credNS, sourceNs, "", server, userHash)
	require.Equal(t, credNS, grantNS, "the grant must land in the locked namespace")

	grant := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantName,
			Namespace: grantNS,
			Labels:    grantSecretLabels(server, userHash, sourceNs, ""),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyOAuthAccessToken:   []byte(accessToken),
			secretKeyOAuthTokenEndpoint: []byte("https://issuer/token"),
			secretKeyOAuthExpiry:        []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	require.NoError(t, credClient.Create(ctx, grant), "the credential-component SA can write grants")

	// The privileged (credential-plane) client reads alice's grant from the locked
	// namespace — the runtime resolve path lives in internal/credresolve now
	// (TestIntegrationPerUserIsolation there); here we prove the RBAC ISOLATION around it.
	var privileged corev1.Secret
	require.NoError(t, credClient.Get(ctx, client.ObjectKey{Namespace: credNS, Name: grantName}, &privileged),
		"the privileged client reads the grant from the credential namespace")
	assert.Equal(t, accessToken, string(privileged.Data[secretKeyOAuthAccessToken]))

	// --- (2) a tenant is FORBIDDEN from reading the grant Secret (the whole point) ---
	var stolen corev1.Secret
	err = tenantClient.Get(ctx, client.ObjectKey{Namespace: credNS, Name: grantName}, &stolen)
	require.Error(t, err, "a tenant must not read another user's grant")
	assert.Truef(t, apierrors.IsForbidden(err),
		"tenant read of the locked-namespace grant must be Forbidden (RBAC isolation); got %v", err)

	// A tenant also cannot LIST grants in the credential namespace (no enumeration).
	var list corev1.SecretList
	err = tenantClient.List(ctx, &list, client.InNamespace(credNS))
	require.Error(t, err)
	assert.Truef(t, apierrors.IsForbidden(err), "tenant list in the locked namespace must be Forbidden; got %v", err)
}
