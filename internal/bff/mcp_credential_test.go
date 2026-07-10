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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// TestSharedCredentialResolverReturnsSharedKey proves the M14 (server, user) seam
// resolves the SHARED register-flow key: the SecretBinding named after the server
// → its Secret → the bearer value. It ignores `user` in M14 (all users share the
// service key) — but takes it, so M17 per-user OAuth is a resolver swap, not a
// caller change. Two different users resolve the SAME shared key today.
func TestSharedCredentialResolverReturnsSharedKey(t *testing.T) {
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "weather-mcp", Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "weather-mcp", Key: secretKeyAPIKey},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "weather-mcp", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theMCPKey)},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(binding, secret).Build()
	r := NewSharedSecretCredentialResolver(c)

	// Two distinct invoking users resolve the SAME shared credential in M14.
	for _, user := range []string{"alice", "bob"} {
		cred, err := r.Resolve(context.Background(), "prod", "weather-mcp", user)
		require.NoError(t, err, user)
		assert.Equal(t, "bearer", cred.Kind)
		assert.Equal(t, theMCPKey, cred.Value, "M14: the shared/service key, keyed (server,user)")
	}
}

// TestSharedCredentialResolverOpenServer proves a server registered WITHOUT a key
// (no SecretBinding) yields errNoMCPCredential — the caller attaches nothing.
func TestSharedCredentialResolverOpenServer(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := NewSharedSecretCredentialResolver(c)

	_, err := r.Resolve(context.Background(), "prod", "open-mcp", "alice")
	assert.ErrorIs(t, err, errNoMCPCredential)
}
