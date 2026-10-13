package controller

import (
	"os"
	"strings"
	"testing"
)

// The managed runtime decides whether it is guarded, and so whether it may stream, from the env var the
// controller injects. When the controller moved the policy into a mounted file (GUARDRAIL_POLICY_FILE)
// the SDK kept reading GUARDRAIL_POLICY, so every guarded agent streamed and every call was refused. Pin
// the SDK to the name this package injects.
func TestManagedRuntimeReadsTheGuardrailEnvTheControllerInjects(t *testing.T) {
	raw, err := os.ReadFile("../../sdk/python/src/ctxmesh/managed.py")
	if err != nil {
		t.Fatalf("reading the managed runtime: %v", err)
	}
	if !strings.Contains(string(raw), `"`+envGuardrailPolicyFile+`"`) {
		t.Errorf("sdk/python/src/ctxmesh/managed.py never reads %s, the env the controller injects for a guarded agent",
			envGuardrailPolicyFile)
	}
}
