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

package statelayer

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func nsObj(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// The resolver reads the AUTHORITATIVE tenant label; an unlabeled / unknown
// namespace is untenanted ("" without error), never a failure.
func TestLabelTenantResolver(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(
		nsObj("team-alpha-ns", map[string]string{tenantLabel: "team-alpha"}),
		nsObj("team-beta-ns", map[string]string{tenantLabel: "team-beta"}),
		nsObj("unlabeled-ns", nil),
		nsObj("other-label-ns", map[string]string{"foo": "bar"}),
	).Build()
	r := NewLabelTenantResolver(c)

	cases := []struct{ name, ns, want string }{
		{"labeled → tenant id", "team-alpha-ns", "team-alpha"},
		{"a different namespace → its own tenant", "team-beta-ns", "team-beta"},
		{"unlabeled → empty", "unlabeled-ns", ""},
		{"other labels only → empty", "other-label-ns", ""},
		{"missing namespace → empty (untenanted, not an error)", "ghost-ns", ""},
		{"empty namespace → empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.TenantID(context.Background(), tc.ns)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// errResolver is a TenantResolver whose backend is unreachable — used to prove the
// error path is surfaced (never silently treated as untenanted).
type errResolver struct{ err error }

func (e errResolver) TenantID(context.Context, string) (string, error) { return "", e.err }

// Server.resolveTenant threads the resolver: a tenant is (id,true); untenanted is
// ("",false); no resolver configured is ("",false); an infra error propagates.
func TestServerResolveTenant(t *testing.T) {
	mr := miniredis.RunT(t)
	store := NewRedisStore(mr.Addr(), "", "")
	c := fake.NewClientBuilder().WithObjects(
		nsObj("team-alpha-ns", map[string]string{tenantLabel: "team-alpha"}),
	).Build()

	withResolver, err := NewServer(Options{Store: store, TenantResolver: NewLabelTenantResolver(c)})
	require.NoError(t, err)

	id, ok, err := withResolver.resolveTenant(context.Background(), "team-alpha-ns")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "team-alpha", id)

	id, ok, err = withResolver.resolveTenant(context.Background(), "ghost-ns")
	require.NoError(t, err)
	assert.False(t, ok, "untenanted namespace is (\"\", false)")
	assert.Empty(t, id)

	// No resolver configured (memory-only deployment) → ("", false), no error.
	noResolver, err := NewServer(Options{Store: store})
	require.NoError(t, err)
	_, ok, err = noResolver.resolveTenant(context.Background(), "team-alpha-ns")
	require.NoError(t, err)
	assert.False(t, ok)

	// An infrastructure failure propagates (must NOT look untenanted).
	broken, err := NewServer(Options{Store: store, TenantResolver: errResolver{err: errors.New("cache unreachable")}})
	require.NoError(t, err)
	_, ok, err = broken.resolveTenant(context.Background(), "team-alpha-ns")
	require.Error(t, err)
	assert.False(t, ok)
}
