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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func saObj(ns, name string, labels map[string]string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
}

func TestSARegistryResolver(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(
		saObj("team-alpha", "agent-support", map[string]string{registryIDLabelKey: "reg-1"}),
		saObj("team-alpha", "agent-solo", nil), // no registry label
	)
	r := NewSARegistryResolver(cs)

	reg, err := r.Registry(ctx, "team-alpha", "agent-support")
	require.NoError(t, err)
	assert.Equal(t, "reg-1", reg, "the registry comes from the SA label the controller stamps")

	reg, err = r.Registry(ctx, "team-alpha", "agent-solo")
	require.NoError(t, err)
	assert.Empty(t, reg, "an SA with no registry label → no registry (private fallback)")

	// A missing identity SA → "" (private fallback), NOT an infra error.
	reg, err = r.Registry(ctx, "team-alpha", "agent-ghost")
	require.NoError(t, err)
	assert.Empty(t, reg)
}

// The resolver caches so a hot memory path doesn't hit the API server on every request.
func TestSARegistryResolverCaches(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(saObj("ns", "agent-x", map[string]string{registryIDLabelKey: "reg-9"}))
	var gets int
	cs.PrependReactor("get", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil // fall through to the default tracker
	})
	r := NewSARegistryResolver(cs)

	for range 3 {
		reg, err := r.Registry(ctx, "ns", "agent-x")
		require.NoError(t, err)
		assert.Equal(t, "reg-9", reg)
	}
	assert.Equal(t, 1, gets, "repeated lookups within the TTL hit the cache — one API GET")
}
