// Unit test (no build tag — runs in make test / tier0).
package controller

import "testing"

// One AgentDeployment worker means one blocked reconcile stops every agent (the M181 wedge).
func TestAgentDeploymentReconcilesConcurrently(t *testing.T) {
	if agentDeploymentWorkers < 4 {
		t.Errorf("agentDeploymentWorkers = %d; at least 4, or one blocked reconcile stalls every agent", agentDeploymentWorkers)
	}
}
