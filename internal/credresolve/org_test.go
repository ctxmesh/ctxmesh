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

package credresolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewOrgCredentialFunc(t *testing.T) {
	ctx := context.Background()
	orgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OrgSecretName("weather"),
			Namespace: "team-alpha",
			Labels:    OrgSecretLabels("weather"),
		},
		Data: OrgSecretData("SHARED-ORG-KEY"),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(orgSecret).Build()

	orgScoped := func(context.Context, string, string) (bool, error) { return true, nil }
	notOrg := func(context.Context, string, string) (bool, error) { return false, nil }

	t.Run("org-scoped + secret exists → shared bearer", func(t *testing.T) {
		got, err := NewOrgCredentialFunc(cl, "", orgScoped)(ctx, "team-alpha", "weather")
		require.NoError(t, err)
		assert.Equal(t, KindBearer, got.Kind)
		assert.Equal(t, "SHARED-ORG-KEY", got.Value)
	})

	t.Run("not org-scoped → fall through", func(t *testing.T) {
		_, err := NewOrgCredentialFunc(cl, "", notOrg)(ctx, "team-alpha", "weather")
		assert.ErrorIs(t, err, ErrNoCredential)
	})

	t.Run("org-scoped but no secret → fall through", func(t *testing.T) {
		_, err := NewOrgCredentialFunc(cl, "", orgScoped)(ctx, "team-alpha", "no-such-server")
		assert.ErrorIs(t, err, ErrNoCredential)
	})

	t.Run("nil isOrgScoped → fail-closed", func(t *testing.T) {
		_, err := NewOrgCredentialFunc(cl, "", nil)(ctx, "team-alpha", "weather")
		assert.ErrorIs(t, err, ErrNoCredential)
	})
}
