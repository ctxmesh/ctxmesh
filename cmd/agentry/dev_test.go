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

package main

import (
	"strings"
	"testing"

	"github.com/ctxmesh/agentry/internal/gateway"
	"gopkg.in/yaml.v3"
)

// ── mock-response in-sync guard ───────────────────────────────────────────────

// TestMockResponseInSyncWithGateway asserts the dev command's mock completion is
// byte-identical to the production render's MockResponse, so the local loop and
// the in-cluster gateway return the SAME MOCK_OK marker (parity — the whole point
// of `dev`). If the controller's mock text changes, this fails and forces a sync.
func TestMockResponseInSyncWithGateway(t *testing.T) {
	if mockResponseText != gateway.MockResponse {
		t.Fatalf("dev mock text drifted from gateway.MockResponse:\n dev:     %q\n gateway: %q",
			mockResponseText, gateway.MockResponse)
	}
	if !strings.Contains(mockResponseText, "MOCK_OK") {
		t.Fatalf("mock text missing MOCK_OK marker: %q", mockResponseText)
	}
}

// ── parseDevYAML ──────────────────────────────────────────────────────────────

func TestParseDevYAML(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantName  string
		wantImage string
		wantRoute string // "" ⇒ model.route absent
		wantErr   bool
		wantCode  int
	}{
		{
			name:      "minimal name+image",
			yaml:      "name: echo\nimage: echo-agent:latest\n",
			wantName:  "echo",
			wantImage: "echo-agent:latest",
		},
		{
			name:      "with model.route",
			yaml:      "name: chat\nimage: chat:latest\nmodel:\n  route: gpt4\n",
			wantName:  "chat",
			wantImage: "chat:latest",
			wantRoute: "gpt4",
		},
		{
			name:      "superset agent.yaml (unknown fields tolerated)",
			yaml:      "name: echo\nimage: echo:latest\nbudget:\n  perAgentUSD: 5\nscaling:\n  min: 1\n",
			wantName:  "echo",
			wantImage: "echo:latest",
		},
		{
			name:     "missing name",
			yaml:     "image: echo:latest\n",
			wantErr:  true,
			wantCode: exitValidation,
		},
		{
			name:     "missing image",
			yaml:     "name: echo\n",
			wantErr:  true,
			wantCode: exitValidation,
		},
		{
			name:     "malformed yaml",
			yaml:     "name: echo\n  image: : :\n",
			wantErr:  true,
			wantCode: exitParse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dy, err := parseDevYAML([]byte(tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				var xe *expandError
				if !isExpandError(err, &xe) {
					t.Fatalf("expected *expandError, got %T: %v", err, err)
				}
				if xe.code != tt.wantCode {
					t.Fatalf("exit code = %d, want %d", xe.code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dy.Name != tt.wantName {
				t.Errorf("name = %q, want %q", dy.Name, tt.wantName)
			}
			if dy.Image != tt.wantImage {
				t.Errorf("image = %q, want %q", dy.Image, tt.wantImage)
			}
			gotRoute := ""
			if dy.Model != nil {
				gotRoute = dy.Model.Route
			}
			if gotRoute != tt.wantRoute {
				t.Errorf("route = %q, want %q", gotRoute, tt.wantRoute)
			}
		})
	}
}

// ── resolveDevFlags ───────────────────────────────────────────────────────────

func TestResolveDevFlags(t *testing.T) {
	t.Run("mock defaults", func(t *testing.T) {
		f, err := resolveDevFlags(devFlagValues{file: "agent.yaml", port: 8080, provider: "mock"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Provider != providerMock {
			t.Errorf("provider = %q, want mock", f.Provider)
		}
		if f.Port != 8080 {
			t.Errorf("port = %d, want 8080", f.Port)
		}
	})

	t.Run("provider case-insensitive", func(t *testing.T) {
		f, err := resolveDevFlags(devFlagValues{file: "a.yaml", port: 8080, provider: "MOCK"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Provider != providerMock {
			t.Errorf("provider = %q, want mock", f.Provider)
		}
	})

	t.Run("port out of range", func(t *testing.T) {
		_, err := resolveDevFlags(devFlagValues{port: 0, provider: "mock"})
		assertValidationErr(t, err)
	})

	t.Run("unknown provider", func(t *testing.T) {
		_, err := resolveDevFlags(devFlagValues{port: 8080, provider: "bogus"})
		assertValidationErr(t, err)
	})

	t.Run("real requires model", func(t *testing.T) {
		_, err := resolveDevFlags(devFlagValues{port: 8080, provider: "real", keyEnv: "K"})
		assertValidationErr(t, err)
	})

	t.Run("real requires key-env", func(t *testing.T) {
		_, err := resolveDevFlags(devFlagValues{port: 8080, provider: "real", realModel: "openai/gpt-4o-mini"})
		assertValidationErr(t, err)
	})

	t.Run("real with empty key env value", func(t *testing.T) {
		t.Setenv("DEV_TEST_EMPTY_KEY", "")
		_, err := resolveDevFlags(devFlagValues{
			port: 8080, provider: "real", realModel: "openai/gpt-4o-mini", keyEnv: "DEV_TEST_EMPTY_KEY",
		})
		assertValidationErr(t, err)
	})

	t.Run("real resolves key from env", func(t *testing.T) {
		t.Setenv("DEV_TEST_KEY", "sk-secret-value")
		f, err := resolveDevFlags(devFlagValues{
			port: 9090, provider: "real", realModel: "openai/gpt-4o-mini",
			realBase: "https://api.example.com", keyEnv: "DEV_TEST_KEY",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.RealAPIKey != "sk-secret-value" {
			t.Errorf("key = %q, want resolved from env", f.RealAPIKey)
		}
		if f.RealModel != "openai/gpt-4o-mini" || f.RealBaseURL != "https://api.example.com" {
			t.Errorf("real model/base not carried through: %+v", f)
		}
	})
}

// ── buildDevPlan / route defaulting ───────────────────────────────────────────

func TestBuildDevPlan_RouteDefaulting(t *testing.T) {
	t.Run("uses agent.yaml route when set", func(t *testing.T) {
		dy := &devYAML{Name: "a", Image: "img:1", Model: &modelYAML{Route: "gpt4"}}
		plan, err := buildDevPlan(dy, devFlags{Port: 8080, Provider: providerMock}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.Route != "gpt4" {
			t.Errorf("route = %q, want gpt4", plan.Route)
		}
		if !strings.Contains(plan.GatewayConfigYAML, "model_name: gpt4") {
			t.Errorf("gateway config missing the declared route:\n%s", plan.GatewayConfigYAML)
		}
	})

	t.Run("defaults route when absent", func(t *testing.T) {
		dy := &devYAML{Name: "a", Image: "img:1"}
		plan, err := buildDevPlan(dy, devFlags{Port: 8080, Provider: providerMock}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.Route != defaultRoute {
			t.Errorf("route = %q, want %q", plan.Route, defaultRoute)
		}
	})

	t.Run("invoke URL reflects port", func(t *testing.T) {
		dy := &devYAML{Name: "a", Image: "img:1"}
		plan, err := buildDevPlan(dy, devFlags{Port: 9191, Provider: providerMock}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.InvokeURL() != "http://localhost:9191/invoke" {
			t.Errorf("invoke URL = %q", plan.InvokeURL())
		}
	})
}

// ── renderGatewayConfig ───────────────────────────────────────────────────────

func TestRenderGatewayConfig_Mock(t *testing.T) {
	cfg, err := renderGatewayConfig("myroute", devFlags{Provider: providerMock})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Valid YAML.
	var parsed map[string]any
	if uErr := yaml.Unmarshal([]byte(cfg), &parsed); uErr != nil {
		t.Fatalf("rendered gateway config is not valid YAML: %v\n%s", uErr, cfg)
	}
	if !strings.Contains(cfg, "model_name: myroute") {
		t.Errorf("missing route name:\n%s", cfg)
	}
	if !strings.Contains(cfg, "mock_response:") || !strings.Contains(cfg, "MOCK_OK") {
		t.Errorf("mock config missing mock_response/MOCK_OK:\n%s", cfg)
	}
}

func TestRenderGatewayConfig_Real_NeverWritesKey(t *testing.T) {
	cfg, err := renderGatewayConfig("myroute", devFlags{
		Provider:    providerReal,
		RealModel:   "openai/gpt-4o-mini",
		RealBaseURL: "https://api.example.com",
		RealAPIKey:  "sk-super-secret", // MUST NOT appear in the rendered file
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(cfg, "sk-super-secret") {
		t.Fatalf("SECRET LEAK: API key literal written into gateway config:\n%s", cfg)
	}
	if !strings.Contains(cfg, "os.environ/DEV_PROVIDER_KEY") {
		t.Errorf("real config should reference the key via env, got:\n%s", cfg)
	}
	if !strings.Contains(cfg, "model: openai/gpt-4o-mini") {
		t.Errorf("real config missing upstream model:\n%s", cfg)
	}
	if !strings.Contains(cfg, "api_base: https://api.example.com") {
		t.Errorf("real config missing base URL:\n%s", cfg)
	}
}

func TestRenderGatewayConfig_Real_RequiresModel(t *testing.T) {
	_, err := renderGatewayConfig("r", devFlags{Provider: providerReal})
	assertValidationErr(t, err)
}

// ── renderCompose ─────────────────────────────────────────────────────────────

func TestRenderCompose_FullContract(t *testing.T) {
	dy := &devYAML{Name: "echo", Image: "echo-agent:latest", Model: &modelYAML{Route: "gpt4"}}
	plan, err := buildDevPlan(dy, devFlags{Port: 8085, Provider: providerMock}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	compose := plan.ComposeYAML

	// Valid YAML.
	var parsed map[string]any
	if uErr := yaml.Unmarshal([]byte(compose), &parsed); uErr != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", uErr, compose)
	}

	// All four services present.
	for _, svc := range []string{"agent:", "gateway:", "memory:", "discovery:"} {
		if !strings.Contains(compose, svc) {
			t.Errorf("compose missing service %q:\n%s", svc, compose)
		}
	}

	// The agent image + published port.
	if !strings.Contains(compose, "image: echo-agent:latest") {
		t.Errorf("compose missing agent image:\n%s", compose)
	}
	if !strings.Contains(compose, `"8085:8080"`) {
		t.Errorf("compose missing published port 8085:\n%s", compose)
	}

	// The FULL runtime contract env on the agent.
	contract := []string{
		`MODEL_GATEWAY_URL: "http://gateway:4000/v1"`, // gateway → mock
		`MODEL_ROUTE: "gpt4"`,
		`MEMORY_BACKEND_ADDR: "memory:6379"`, // :2998 launcher endpoint wiring
		`MEMORY_PORT: "2998"`,
		`FEEDBACK_PORT: "2995"`,       // :2995 launcher endpoint wiring
		`LANGFUSE_HOST:`,              // gates the feedback listener
		`DISCOVERY_HOST: "discovery"`, // :2999 sidecar
		`DISCOVERY_PORT: "2999"`,
		`AGENT_NAME: "echo"`,
		`AGENT_PORT: "8080"`,
	}
	for _, want := range contract {
		if !strings.Contains(compose, want) {
			t.Errorf("compose missing contract env %q:\n%s", want, compose)
		}
	}

	// Uses the SAME mock gateway image production uses.
	if !strings.Contains(compose, litellmImage) {
		t.Errorf("compose missing the LiteLLM gateway image %q:\n%s", litellmImage, compose)
	}
}

func TestRenderCompose_MockModeHasNoProviderKeyEnv(t *testing.T) {
	dy := &devYAML{Name: "echo", Image: "echo:latest"}
	plan, err := buildDevPlan(dy, devFlags{Port: 8080, Provider: providerMock}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(plan.ComposeYAML, "DEV_PROVIDER_KEY") {
		t.Errorf("mock compose should not reference a provider key:\n%s", plan.ComposeYAML)
	}
}

func TestRenderCompose_RealModeInjectsKeyIntoGatewayOnly(t *testing.T) {
	dy := &devYAML{Name: "echo", Image: "echo:latest"}
	plan, err := buildDevPlan(dy, devFlags{
		Port: 8080, Provider: providerReal, RealModel: "openai/gpt-4o-mini", RealAPIKey: "sk-x",
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The key VALUE is never in the file — only the ${DEV_PROVIDER_KEY} reference,
	// and only under the gateway service.
	if strings.Contains(plan.ComposeYAML, "sk-x") {
		t.Fatalf("SECRET LEAK: real key value in compose:\n%s", plan.ComposeYAML)
	}
	if !strings.Contains(plan.ComposeYAML, "DEV_PROVIDER_KEY") {
		t.Errorf("real compose should reference DEV_PROVIDER_KEY on the gateway:\n%s", plan.ComposeYAML)
	}
	// The reference must be after the gateway service and not on the agent — assert
	// the agent service block does not carry it.
	agentStart := strings.Index(plan.ComposeYAML, "  agent:")
	gatewayStart := strings.Index(plan.ComposeYAML, "  gateway:")
	agentBlock := plan.ComposeYAML[agentStart:gatewayStart]
	if strings.Contains(agentBlock, "DEV_PROVIDER_KEY") {
		t.Errorf("provider key must not be on the agent service:\n%s", agentBlock)
	}
}

// ── renderCompose (replay mode) ────────────────────────────────────────────────

// TestRenderCompose_ReplaySwapsGatewayAndRewritesTools proves that in replay mode renderCompose
// swaps the LiteLLM gateway service for the replay-serve mock UNDER THE SAME `gateway` service
// name + internal port (so MODEL_GATEWAY_URL is unchanged), and the discovery manifest points the
// fixture's tools at the replay mock's /mcp channel (ADR 0071 §3a).
func TestRenderCompose_ReplaySwapsGatewayAndRewritesTools(t *testing.T) {
	dy := &devYAML{Name: "planner", Image: "planner:latest"}
	plan, err := buildDevPlan(dy, devFlags{Port: 8080, Provider: providerMock, ReplayFixture: "/some/fixture.json"},
		[]string{"search", "send_email"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	compose := plan.ComposeYAML

	// Valid YAML.
	var parsed map[string]any
	if uErr := yaml.Unmarshal([]byte(compose), &parsed); uErr != nil {
		t.Fatalf("rendered replay compose is not valid YAML: %v\n%s", uErr, compose)
	}

	// The gateway service is swapped for the replay image + replay-serve command; the LiteLLM
	// image is GONE.
	if !strings.Contains(compose, "agentry-replay:") {
		t.Errorf("replay compose should use the replay image:\n%s", compose)
	}
	if strings.Contains(compose, litellmImage) {
		t.Errorf("replay compose must NOT keep the LiteLLM gateway image:\n%s", compose)
	}
	if !strings.Contains(compose, `"replay-serve"`) {
		t.Errorf("replay compose should run the replay-serve command:\n%s", compose)
	}
	if !strings.Contains(compose, replayFixtureMountPath) {
		t.Errorf("replay compose should mount the fixture at %s:\n%s", replayFixtureMountPath, compose)
	}

	// The model channel is UNCHANGED — the agent still points at http://gateway:4000/v1.
	if !strings.Contains(compose, `MODEL_GATEWAY_URL: "http://gateway:4000/v1"`) {
		t.Errorf("replay compose must keep MODEL_GATEWAY_URL on the same gateway host:\n%s", compose)
	}

	// The agent must not restart (a restart must surface as index-overflow, not double-consume).
	agentStart := strings.Index(compose, "  agent:")
	gatewayStart := strings.Index(compose, "  gateway:")
	agentBlock := compose[agentStart:gatewayStart]
	if !strings.Contains(agentBlock, `restart: "no"`) {
		t.Errorf("agent must be restart: \"no\" in replay mode:\n%s", agentBlock)
	}

	// The discovery manifest lists the fixture's tools pointing at the replay /mcp channel.
	if !strings.Contains(plan.ToolsJSON, replayToolEndpoint()) {
		t.Errorf("tools.json should point tools at the replay /mcp endpoint %q:\n%s",
			replayToolEndpoint(), plan.ToolsJSON)
	}
	for _, name := range []string{"search", "send_email"} {
		if !strings.Contains(plan.ToolsJSON, `"`+name+`"`) {
			t.Errorf("tools.json should advertise recorded tool %q:\n%s", name, plan.ToolsJSON)
		}
	}
}

// TestRenderReplayToolsJSON_DedupesAndEmpty proves the replay manifest render dedupes tool names
// and yields the empty manifest for a fixture with no tools.
func TestRenderReplayToolsJSON_DedupesAndEmpty(t *testing.T) {
	if got := renderReplayToolsJSON(nil); !strings.Contains(got, `"tools":[]`) {
		t.Errorf("no tools should render the empty manifest, got %q", got)
	}
	got := renderReplayToolsJSON([]string{"search", "search", "  ", "fetch"})
	if strings.Count(got, `"name":"search"`) != 1 {
		t.Errorf("duplicate tool names should be deduped:\n%s", got)
	}
	if !strings.Contains(got, `"name":"fetch"`) {
		t.Errorf("expected 'fetch' in the manifest:\n%s", got)
	}
}

// TestResolveDevFlags_ReplayRejectsRealProvider proves --replay + --provider real is rejected.
func TestResolveDevFlags_ReplayRejectsRealProvider(t *testing.T) {
	_, err := resolveDevFlags(devFlagValues{
		port: 8080, provider: "real", replay: "/tmp/fixture.json",
		realModel: "openai/gpt-4o-mini", keyEnv: "X",
	})
	assertValidationErr(t, err)
}

// ── composeEnv ────────────────────────────────────────────────────────────────

func TestComposeEnv(t *testing.T) {
	if got := composeEnv(devFlags{Provider: providerMock}); len(got) != 0 {
		t.Errorf("mock composeEnv = %v, want empty", got)
	}
	got := composeEnv(devFlags{Provider: providerReal, RealAPIKey: "sk-1"})
	if len(got) != 1 || got[0] != "DEV_PROVIDER_KEY=sk-1" {
		t.Errorf("real composeEnv = %v, want [DEV_PROVIDER_KEY=sk-1]", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertValidationErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error, got nil")
	}
	var xe *expandError
	if !isExpandError(err, &xe) {
		t.Fatalf("expected *expandError, got %T: %v", err, err)
	}
	if xe.code != exitValidation {
		t.Fatalf("exit code = %d, want %d (validation)", xe.code, exitValidation)
	}
}
