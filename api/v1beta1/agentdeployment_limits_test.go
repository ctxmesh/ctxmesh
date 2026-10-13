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

package v1beta1

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The constant other code validates agent names with is the limit the generated CRD enforces.
func TestAgentDeploymentNameMaxLength_MatchesTheCRD(t *testing.T) {
	crd, err := os.ReadFile("../../config/crd/bases/agents.ctxmesh.ai_agentdeployments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rule := fmt.Sprintf("size(self.metadata.name) <= %d", AgentDeploymentNameMaxLength)
	if !strings.Contains(string(crd), rule) {
		t.Fatalf("the AgentDeployment CRD has no rule %q", rule)
	}
}
