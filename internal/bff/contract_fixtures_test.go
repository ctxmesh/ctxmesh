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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// contractFixturesPath is where the generated fixtures land — under the UI test
// tree so a vitest can import them. Relative to this package's directory
// (internal/bff) → the repo's ui/src/test.
const contractFixturesPath = "../../ui/src/test/contract-fixtures.json"

// buildContractFixtures constructs ONE representative instance of every DTO a UI
// page consumes, marshalled through the SAME json tags the BFF emits at runtime.
// The whole map is marshalled once (encoding/json sorts map keys) so the emitted
// document is deterministic.
func buildContractFixtures() map[string]any {
	provider := ProviderSummary{
		Name:        "anthropic",
		Namespace:   "default",
		Provider:    "anthropic",
		DisplayName: "Anthropic",
		Models:      []string{"claude-opus-4", "claude-sonnet-4"},
		SecretName:  "anthropic",
		Ready:       true,
	}
	mcpServer := MCPServerSummary{
		Name:       "acme-mcp",
		Namespace:  "default",
		URL:        "https://mcp.acme.dev/sse",
		ToolCount:  3,
		Status:     "approved",
		SecretName: "acme-key",
	}
	return map[string]any{
		"ConnectProviderResponse": ConnectProviderResponse{
			Provider: provider,
			Created:  []createdObject{{Kind: "Secret", Name: "anthropic", Namespace: "default"}},
		},
		"ProviderListResponse": ProviderListResponse{
			Providers: []ProviderSummary{provider},
			Items:     []ProviderSummary{provider},
		},
		"MCPServerListResponse": MCPServerListResponse{
			Servers: []MCPServerSummary{mcpServer},
			Items:   []MCPServerSummary{mcpServer},
		},
		"AgentDetailResponse": AgentDetailResponse{
			Name:           "echo",
			Namespace:      "default",
			Image:          "img:1",
			ExecutionModel: "serving",
			Role:           "worker",
			PromptRef:      "system-prompt-v2",
			ModelRoute:     "anthropic",
			Scaling:        AgentScaling{Min: 1, Max: 5},
			Phase:          phaseReady,
			Ready:          true,
			URL:            "http://echo.default.example",
			LatestVersion:  "echo-abc123",
			Conditions: []AgentCondition{{
				Type: "Ready", Status: "True", Reason: "Deployed", Message: "serving",
				LastTransitionTime: "2026-07-12T10:00:00Z",
			}},
			Bindings: []AgentBinding{{Kind: "tool", Name: "echo-search", Detail: "search", Ready: true}},
			Versions: []string{"echo-abc123"},
		},
	}
}

// TestContractFixtures is the mock-drift GUARD (m18.4). It marshals the real BFF
// DTOs and compares them to the committed JSON the UI vitest imports. If a DTO's
// shape drifts, this fails until the fixtures are regenerated:
//
//	UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestContractFixtures
//
// A UI component test that builds its mock from this fixture can therefore never
// again pass against a shape the BFF does not actually send (the two live crashes
// that motivated M18).
func TestContractFixtures(t *testing.T) {
	got, err := json.MarshalIndent(buildContractFixtures(), "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(contractFixturesPath), 0o755))
		require.NoError(t, os.WriteFile(contractFixturesPath, got, 0o644))
		t.Logf("wrote %s", contractFixturesPath)
		return
	}

	want, err := os.ReadFile(contractFixturesPath)
	require.NoError(t, err,
		"contract fixtures missing — run: UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestContractFixtures")
	require.Equal(t, string(want), string(got),
		"BFF DTO shape drifted from the committed UI contract fixtures — regenerate with "+
			"UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestContractFixtures")
}
