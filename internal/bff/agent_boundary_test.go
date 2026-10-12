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

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// TestAgentBoundary: the boundary an agent's run resolves credentials within (ADR 0033) is the
// registry the controller recorded in status.registryRef, the agent itself when it has none, and
// fails SAFE (per-agent, never "") when the agent or its registry can't be resolved.
func TestAgentBoundary(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	squadA := map[string]string{"squad": "a"}

	agent := func(name, registryRef string) *agentsv1beta1.AgentDeployment {
		return &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Labels: squadA},
			Status:     agentsv1beta1.AgentDeploymentStatus{RegistryRef: registryRef},
		}
	}
	registry := func(name, id string) *agentsv1beta1.AgentRegistry {
		return &agentsv1beta1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team"},
			Spec: agentsv1beta1.AgentRegistrySpec{
				RegistryId:     id,
				MemberSelector: metav1.LabelSelector{MatchLabels: squadA},
			},
		}
	}
	now := metav1.Now()
	leaving := registry("leaving", "leaving-id")
	leaving.DeletionTimestamp = &now
	leaving.Finalizers = []string{"test/hold"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		agent("worker", "squad-a-registry"),
		agent("unreconciled", ""),
		agent("orphaned", "deleted-registry"),
		agent("departing", "leaving"),
		registry("squad-a-registry", "squad-a"),
		leaving,
	).Build()

	// A member resolves to its recorded registry's boundary (its teammates share the grant).
	assert.Equal(t, credresolve.RegistryBoundary("squad-a"), agentBoundary(ctx, c, "team", "worker"))
	// Labels matching a registry's selector are not membership until the controller records it:
	// the BFF does not evaluate selectors.
	assert.Equal(t, credresolve.AgentBoundary("team", "unreconciled"), agentBoundary(ctx, c, "team", "unreconciled"))
	// A recorded registry that is gone or terminating fails safe to the agent's own boundary.
	assert.Equal(t, credresolve.AgentBoundary("team", "orphaned"), agentBoundary(ctx, c, "team", "orphaned"))
	assert.Equal(t, credresolve.AgentBoundary("team", "departing"), agentBoundary(ctx, c, "team", "departing"))
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
