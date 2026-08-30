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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/credresolve"
)

// TestAgentBoundary: the boundary an agent's run resolves credentials within (ADR 0033) is its
// registry when it belongs to one, the agent itself when standalone, and fails SAFE (per-agent,
// never "") when the agent can't be resolved.
func TestAgentBoundary(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)

	member := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker", Namespace: "team", Labels: map[string]string{"squad": "a"},
		},
	}
	solo := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "team"},
	}
	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-a-registry", Namespace: "team"},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     "squad-a",
			MemberSelector: metav1.LabelSelector{MatchLabels: map[string]string{"squad": "a"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(member, solo, reg).Build()

	// A registry member resolves to its registry's boundary (its teammates share the grant).
	assert.Equal(t, credresolve.RegistryBoundary("squad-a"), agentBoundary(ctx, c, "team", "worker"))
	// A standalone agent (no matching registry) is its own boundary.
	assert.Equal(t, credresolve.AgentBoundary("team", "solo"), agentBoundary(ctx, c, "team", "solo"))
	// An unresolvable agent fails safe to the per-agent boundary — never the legacy unscoped "".
	assert.Equal(t, credresolve.AgentBoundary("team", "ghost"), agentBoundary(ctx, c, "team", "ghost"))
	assert.NotEmpty(t, agentBoundary(ctx, c, "team", "ghost"))
}

// TestEndUserAgentBoundary (M137/EU1b): the end-user boundary is the per-agent standalone boundary,
// computed with NO K8s client (no registry read → no BFF-SA grant), and never the legacy unscoped "".
func TestEndUserAgentBoundary(t *testing.T) {
	assert.Equal(t, credresolve.AgentBoundary("team", "chatbot"), endUserAgentBoundary("team", "chatbot"))
	assert.Equal(t, "a:team/chatbot", endUserAgentBoundary("team", "chatbot"))
	assert.NotEmpty(t, endUserAgentBoundary("team", "chatbot"), "never the legacy unscoped boundary")
}
