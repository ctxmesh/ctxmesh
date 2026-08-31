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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// stubRegistryReader returns a fixed result — used to drive resolveAgentBindings'
// not-found-vs-error contract (ADR 0043) deterministically.
type stubRegistryReader struct {
	reg *agentsv1alpha1.ToolRegistry
	err error
}

func (s stubRegistryReader) GetRegistry(context.Context, string, string) (*agentsv1alpha1.ToolRegistry, error) {
	return s.reg, s.err
}

func bindingResolveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return scheme
}

func mkResolveBinding(name, agentRef, registryRef, toolName string) *agentsv1alpha1.MCPToolBinding {
	b := &agentsv1alpha1.MCPToolBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	b.Spec.AgentRef = agentRef
	b.Spec.RegistryRef = registryRef
	b.Spec.ToolName = toolName
	b.Spec.Mode = "remote"
	b.Spec.Server.URL = "https://mcp.example"
	return b
}

// The load-bearing ADR 0043 invariant: a genuine ErrNotFound from the reader
// leaves the registry absent → the binding is reported RegistryNotFound (a
// declarative state, not a failure); resolveAgentBindings does NOT error.
func TestResolveAgentBindings_NotFoundMarksRegistryNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(bindingResolveScheme(t)).
		WithObjects(mkResolveBinding("b1", "agent1", "gone", "t1")).Build()

	valid, validations, err := resolveAgentBindings(
		context.Background(), c, stubRegistryReader{err: controlplane.ErrNotFound}, "default", "agent1")
	require.NoError(t, err)
	assert.Empty(t, valid)
	assert.Equal(t, reasonRegistryNotFound, validations["b1"].Reason)
	assert.False(t, validations["b1"].Valid)
}

// The other half of the contract: a REAL store/read error is not "not found" —
// resolveAgentBindings returns it (so the reconcile requeues) rather than
// silently downgrading a valid binding to RegistryNotFound during an outage.
func TestResolveAgentBindings_StoreErrorRequeues(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(bindingResolveScheme(t)).
		WithObjects(mkResolveBinding("b1", "agent1", "reg1", "t1")).Build()

	boom := errors.New("dial tcp: connection refused")
	_, _, err := resolveAgentBindings(
		context.Background(), c, stubRegistryReader{err: boom}, "default", "agent1")
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

// The Postgres-backed reader projects a store row back into the CRD shape the
// binding path consumes (the inverse of the BFF mirror): tools + annotations +
// verbatim InputSchema round-trip. Wired against the mem twin (no DB).
func TestPostgresRegistryReader_ProjectsStoreRow(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	schema := []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace:   "default",
		Name:        "reg1",
		Annotations: map[string]string{mcpAuthTypeAnnotation: mcpOAuthAuthType},
		Tools: []toolregistry.ToolEntry{
			{Name: "search", URL: "https://mcp.example", Description: "d", Source: "user-added", InputSchema: schema},
		},
	})
	require.NoError(t, err)

	reader := NewPostgresRegistryReader(store)
	got, err := reader.GetRegistry(ctx, "default", "reg1")
	require.NoError(t, err)
	assert.Equal(t, "reg1", got.Name)
	assert.Equal(t, mcpOAuthAuthType, got.Annotations[mcpAuthTypeAnnotation])
	require.Len(t, got.Spec.Tools, 1)
	assert.Equal(t, "search", got.Spec.Tools[0].Name)
	assert.Equal(t, "https://mcp.example", got.Spec.Tools[0].URL)
	require.NotNil(t, got.Spec.Tools[0].InputSchema)
	assert.JSONEq(t, string(schema), string(got.Spec.Tools[0].InputSchema.Raw))
}

// A missing row surfaces as controlplane.ErrNotFound (the sentinel the reader
// uses) so resolveAgentBindings can apply the not-found branch uniformly.
func TestPostgresRegistryReader_NotFound(t *testing.T) {
	reader := NewPostgresRegistryReader(toolregistry.NewMemStore())
	_, err := reader.GetRegistry(context.Background(), "default", "nope")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}
