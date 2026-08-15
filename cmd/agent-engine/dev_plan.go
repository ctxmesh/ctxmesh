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

// dev_plan.go holds the PURE planning logic for `agent-engine dev`: it turns a
// parsed agent.yaml + the resolved dev flags into a devPlan (the set of Compose
// services, their env, and the rendered mock-gateway config). It performs no I/O
// and starts no containers, so the whole plan is unit-testable (dev_test.go).
//
// The runtime (docker compose up/down, /invoke smoke) lives in dev.go.
package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// ── Substrate choice (documented) ─────────────────────────────────────────────
//
// `agent-engine dev` renders a Docker Compose stack — the lightest inner loop
// (spec §5: "Compose or a local kind"). It brings up the SAME launcher + user
// agent that production runs, with the full localhost runtime contract:
//
//	agent     — the user's image (launcher = PID 1, execs the user entrypoint).
//	            The launcher's OWN in-process listeners come up from the injected
//	            env: memory :2998 (MEMORY_BACKEND_ADDR→valkey) and feedback :2995
//	            (LANGFUSE_HOST). The traced proxy owns AGENT_PORT.
//	gateway   — a mock LiteLLM (the SAME mock approach production uses:
//	            internal/gateway.MockResponse "MOCK_OK…" via LiteLLM mock_response).
//	            No new provider is built; --provider real repoints at a real base
//	            URL + key instead.
//	discovery — the tool-discovery sidecar (:2999) serving tools.json (§ contract).
//	memory    — Valkey, the backend the launcher's :2998 endpoint talks to.
//
// This is parity-by-construction: the container images and the contract env are
// exactly what the controller injects in-cluster, only the substrate (Compose vs
// Knative) differs. A full kind cluster is intentionally NOT used here — the
// point of `dev` is a fast inner loop.

const (
	// mockResponseText is the deterministic mock completion the gateway returns.
	// It MUST stay in sync with internal/gateway.MockResponse (the production
	// mock) — kept as a literal here so cmd/agent-engine has no dependency on the
	// controller package, with a unit test asserting they match.
	mockResponseText = "MOCK_OK deterministic response from agent-engine gateway"

	// litellmImage pins the mock-gateway image — the SAME LiteLLM the in-cluster
	// gateway uses (config/gateway/deployment.yaml), so the mock path is identical.
	litellmImage = "ghcr.io/berriai/litellm:v1.91.0"

	// valkeyImage is the memory backend for the launcher's :2998 endpoint.
	valkeyImage = "valkey/valkey:8-alpine"

	// gatewayInternalPort is the port LiteLLM listens on inside its container.
	gatewayInternalPort = 4000

	// discoveryPort is the tool-discovery sidecar port (contract: :2999).
	discoveryPort = 2999

	// memoryPort is the launcher's in-process memory endpoint (contract: :2998).
	memoryPort = 2998

	// feedbackPort is the launcher's in-process feedback ingest hook (contract: :2995).
	feedbackPort = 2995

	// defaultRoute is the model-route alias used when agent.yaml omits model.route.
	// It is the model_name the mock gateway serves, so /invoke resolves to MOCK_OK
	// even for an agent that did not declare a route.
	defaultRoute = "dev-mock"

	// projectName is the Compose project name — namespaces all containers/networks
	// so `dev` never collides with the user's other stacks and teardown is scoped.
	projectName = "agent-engine-dev"

	// replayFixtureMountPath is where `dev --replay` bind-mounts the operator's local
	// fixture (file or dir) INSIDE the swapped gateway container. replay-serve loads
	// the fixture from here.
	replayFixtureMountPath = "/fixture"

	// replayReportHostPort is the host port the swapped gateway (replay-serve) publishes
	// its internal :4000 on in replay mode, so the CLI on the host can GET
	// /replay/report and /replay/version after the agent run (ADR 0071 §3a). It is a
	// high, uncommon port to avoid clashing with the agent's published /invoke port.
	replayReportHostPort = 4010
)

// devVersion is the CLI version, mirrored to the replay image tag + checked against the
// replay-serve container's reported version at startup (ADR 0071 §3a parity gate). Overridable at
// build time via -ldflags "-X main.devVersion=<v>"; "m78-smoke" locally so the default
// Dockerfile.replay build + the CLI agree out of the box.
var devVersion = "m78-smoke"

// replayImageRef is the replay-serve image `dev --replay` swaps the gateway for (built by
// Dockerfile.replay; the established per-binary image pattern — Dockerfile.{launcher,bff,
// egress-sidecar,…}). Tagged to the CLI's own version (devVersion) so a stale image is caught at
// startup by the /replay/version parity check (ADR 0071 §3a). No existing image ships the
// agent-engine CLI, so this is a new but pattern-consistent image.
func replayImageRef() string { return "agent-engine-replay:" + devVersion }

// providerMode selects the gateway backend.
type providerMode string

const (
	// providerMock (default) renders a LiteLLM mock_response gateway → MOCK_OK.
	providerMock providerMode = "mock"
	// providerReal points the gateway at a real upstream base URL + API key.
	providerReal providerMode = "real"
)

// devFlags holds the resolved, validated command-line configuration.
type devFlags struct {
	// File is the path to the user's agent.yaml (same schema `expand` consumes).
	File string
	// Port is the host port the agent's /invoke is published on.
	Port int
	// Provider selects mock (default) or real gateway backend.
	Provider providerMode
	// RealBaseURL / RealAPIKey are used ONLY when Provider == real. The key is
	// read from an env var name (never a committed literal); see resolveDevFlags.
	RealBaseURL string
	RealAPIKey  string
	// RealModel is the upstream model id for the real provider (e.g.
	// "openai/gpt-4o-mini"). Required with --provider real.
	RealModel string
	// ReplayFixture is the path to a recorded fixture (a single merged fixture JSON
	// file OR a directory of partial *.json blobs) to replay. When set, `dev` runs in
	// REPLAY mode (ADR 0071 §3a): the gateway service is swapped for the both-channel
	// replay mock (replay-serve) and the agent's tool endpoints are rewritten to hit
	// its /mcp channel, so both the model + tool channels come from the fixture — fully
	// deterministic, zero cluster deps. Mutually exclusive with --provider real.
	ReplayFixture string
}

// devPlan is the fully-resolved plan for a dev run: the parsed agent, the
// resolved model route, and the rendered Compose + gateway-config content. It is
// produced purely (no I/O) so it can be asserted in unit tests.
type devPlan struct {
	// AgentName is metadata.name from agent.yaml (also AGENT_NAME on the agent).
	AgentName string
	// Image is the agent container image from agent.yaml.
	Image string
	// Route is the resolved MODEL_ROUTE (agent.yaml model.route, or defaultRoute).
	Route string
	// HostPort is the published host port for /invoke.
	HostPort int
	// Provider is the resolved gateway mode.
	Provider providerMode
	// Replay, when non-empty, is the container path the replay-serve mock loads the
	// fixture from (the fixture is bind-mounted into the swapped gateway service). Its
	// presence flips renderCompose into REPLAY mode (ADR 0071 §3a).
	Replay string
	// ComposeYAML is the rendered docker-compose.yaml content.
	ComposeYAML string
	// GatewayConfigYAML is the rendered LiteLLM config.yaml (the mock or real route).
	GatewayConfigYAML string
	// ToolsJSON is the tools.json served by the discovery sidecar. In normal dev it is
	// the empty manifest; in replay mode it lists the fixture's recorded tools with
	// endpoints pointing at the replay mock's /mcp channel (so the agent's tool calls
	// are served from the fixture).
	ToolsJSON string
}

// IsReplay reports whether this plan runs in replay mode (ADR 0071 §3a).
func (p *devPlan) IsReplay() bool { return p.Replay != "" }

// InvokeURL is the local /invoke endpoint the agent answers on.
func (p *devPlan) InvokeURL() string {
	return fmt.Sprintf("http://localhost:%d/invoke", p.HostPort)
}

// devYAML is the subset of agent.yaml `dev` needs: the image to run and the
// model route to point at the gateway. It reuses the SAME simplified schema the
// CLI `expand` command consumes (name, image, model.route), validating only the
// fields `dev` acts on — unknown fields are tolerated here (a superset agent.yaml
// still runs) so the two commands never disagree on required shape.
type devYAML struct {
	Name  string     `yaml:"name"`
	Image string     `yaml:"image"`
	Model *modelYAML `yaml:"model"`
}

// parseDevYAML parses agent.yaml bytes into the dev subset and validates the
// fields dev requires (name + image). It returns a typed *expandError so the
// command surfaces the same exit-code contract as `expand`.
func parseDevYAML(raw []byte) (*devYAML, error) {
	var dy devYAML
	if err := yaml.Unmarshal(raw, &dy); err != nil {
		return nil, parseErr("YAML parse error: %v", err)
	}
	if strings.TrimSpace(dy.Name) == "" {
		return nil, validationErr("required field missing: name")
	}
	if strings.TrimSpace(dy.Image) == "" {
		return nil, validationErr("required field missing: image")
	}
	return &dy, nil
}

// buildDevPlan turns a parsed agent.yaml + resolved flags into a devPlan. Pure:
// no filesystem, no Docker. It resolves the route, renders the gateway config
// (mock or real) and the Compose file, and returns the plan.
//
// replayToolNames is the set of tool names recorded in the fixture (empty unless
// flags.ReplayFixture is set). In replay mode the plan renders tools.json to list
// these tools with endpoints pointing at the replay mock's /mcp channel, so the
// discovery sidecar advertises exactly the fixture's tools and the agent's tool
// calls are served from the recording (ADR 0071 §3a). The caller (dev.go) loads
// the fixture and passes the names, keeping buildDevPlan pure.
func buildDevPlan(dy *devYAML, flags devFlags, replayToolNames []string) (*devPlan, error) {
	route := defaultRoute
	if dy.Model != nil && strings.TrimSpace(dy.Model.Route) != "" {
		route = strings.TrimSpace(dy.Model.Route)
	}

	plan := &devPlan{
		AgentName: dy.Name,
		Image:     dy.Image,
		Route:     route,
		HostPort:  flags.Port,
		Provider:  flags.Provider,
		ToolsJSON: emptyToolsJSON,
	}

	if flags.ReplayFixture != "" {
		// Replay mode: the gateway is swapped for replay-serve (both channels from the
		// fixture); no LiteLLM config is rendered, and tools.json lists the fixture's
		// tools pointing at the replay mock's /mcp channel.
		plan.Replay = replayFixtureMountPath
		plan.ToolsJSON = renderReplayToolsJSON(replayToolNames)
	} else {
		gatewayCfg, err := renderGatewayConfig(route, flags)
		if err != nil {
			return nil, err
		}
		plan.GatewayConfigYAML = gatewayCfg
	}

	plan.ComposeYAML = renderCompose(plan)
	return plan, nil
}

// emptyToolsJSON is the discovery sidecar's cold-start manifest. An agent that
// reads :2999 gets a valid (empty) manifest — the discovery contract is present
// locally exactly as it is in-cluster before any ToolBinding is reconciled.
const emptyToolsJSON = `{"version":"0","tools":[]}` + "\n"

// replayToolEndpoint is the MCP endpoint the discovery sidecar advertises for a replayed tool: the
// swapped `gateway` service's /mcp channel (the model channel stays on the same host at /v1, per
// ADR 0071 §3a "keep the model channel on the existing gateway hostname"). Reachable over the
// compose network by service name — never loopback, so no Linux-CI host-gateway trap.
func replayToolEndpoint() string {
	return fmt.Sprintf("http://gateway:%d/mcp", gatewayInternalPort)
}

// renderReplayToolsJSON renders the discovery sidecar's tools.json for replay mode: one remote,
// streamable-http tool per recorded tool name, all pointing at the replay mock's /mcp endpoint.
// The version is content-addressed via the toolmanifest normalizer so identical tool sets render
// identically (deterministic golden output). An empty name list renders the empty manifest (a
// fixture with no tool interactions).
func renderReplayToolsJSON(names []string) string {
	seen := map[string]bool{}
	tools := make([]toolmanifest.Tool, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		tools = append(tools, toolmanifest.Tool{
			Name:      n,
			Mode:      "remote",
			Endpoint:  replayToolEndpoint(),
			Transport: toolmanifest.Transport,
		})
	}
	m := toolmanifest.Normalize(toolmanifest.Manifest{Tools: tools})
	b, err := json.Marshal(m)
	if err != nil {
		// Tools carry only strings — Marshal cannot fail; fall back to the empty manifest.
		return emptyToolsJSON
	}
	return string(b) + "\n"
}

// renderGatewayConfig renders the LiteLLM config.yaml for the gateway service.
//
//   - mock: a single route named `route` with mock_response → MOCK_OK. No key.
//   - real: a single route named `route` pointing at the user's upstream model +
//     api_key. The key is passed as an env reference (os.environ/DEV_PROVIDER_KEY)
//     so it is NEVER written into the rendered file — the value is injected into
//     the gateway container's environment at up time.
func renderGatewayConfig(route string, flags devFlags) (string, error) {
	var b strings.Builder
	b.WriteString("model_list:\n")
	b.WriteString("  - model_name: " + route + "\n")
	b.WriteString("    litellm_params:\n")

	switch flags.Provider {
	case providerReal:
		if strings.TrimSpace(flags.RealModel) == "" {
			return "", validationErr("--provider real requires --model (the upstream model id, e.g. openai/gpt-4o-mini)")
		}
		fmt.Fprintf(&b, "      model: %s\n", flags.RealModel)
		// Key is read from the gateway container env — never rendered into the file.
		b.WriteString("      api_key: os.environ/DEV_PROVIDER_KEY\n")
		if strings.TrimSpace(flags.RealBaseURL) != "" {
			fmt.Fprintf(&b, "      api_base: %s\n", flags.RealBaseURL)
		}
	case providerMock:
		// Mock: any valid provider prefix works because mock_response short-circuits
		// the call. openai/gpt-4o-mini mirrors internal/gateway.Render's mock entry.
		b.WriteString("      model: openai/gpt-4o-mini\n")
		b.WriteString("      api_key: DUMMY_MOCK_KEY\n")
		fmt.Fprintf(&b, "      mock_response: %q\n", mockResponseText)
	default:
		return "", validationErr("unknown provider %q — use mock or real", flags.Provider)
	}
	return b.String(), nil
}

// renderCompose renders the docker-compose.yaml for the plan. The env wired onto
// the agent service is exactly the runtime contract the controller injects
// in-cluster (with localhost → the Compose service DNS names).
func renderCompose(p *devPlan) string {
	// Agent env — the full runtime contract, keys sorted for deterministic output.
	agentEnv := map[string]string{
		"AGENT_NAME": p.AgentName,
		// The launcher owns AGENT_PORT (proxy) and reverse-proxies to the child
		// on AGENT_UPSTREAM_PORT — same as in-cluster.
		"AGENT_PORT": "8080",
		// Model gateway → the mock (or real) LiteLLM service. The agent stamps
		// MODEL_ROUTE as the model name; the gateway resolves it to MOCK_OK.
		"MODEL_GATEWAY_URL": fmt.Sprintf("http://gateway:%d/v1", gatewayInternalPort),
		"MODEL_ROUTE":       p.Route,
		// Memory endpoint (:2998) — the launcher starts it because a backend is set.
		"MEMORY_BACKEND_ADDR":  "memory:6379",
		"MEMORY_PORT":          fmt.Sprintf("%d", memoryPort),
		"MEMORY_KEY_NAMESPACE": "dev",
		// Feedback ingest hook (:2995) — the launcher starts it because a Langfuse
		// host is set. In dev this points at a stub host; the endpoint exists so the
		// contract is present (feedback POSTs get a connection, not a missing route).
		"LANGFUSE_HOST": "http://localhost:3000",
		"FEEDBACK_PORT": fmt.Sprintf("%d", feedbackPort),
		// Discovery sidecar (:2999) — a separate container the agent reads for tools.
		"DISCOVERY_HOST": "discovery",
		"DISCOVERY_PORT": fmt.Sprintf("%d", discoveryPort),
		// Tracing: no collector in the dev loop; the launcher's OTel init is
		// best-effort and no-ops when the endpoint is unreachable.
		"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317",
	}

	var b strings.Builder
	// A schema-versionless compose file (Compose v2 ignores the `version` key and
	// warns on it, so it is intentionally omitted).
	b.WriteString("# Rendered by `agent-engine dev` — the local inner loop (spec §22).\n")
	b.WriteString("# Ephemeral: regenerated on every run; safe to delete.\n")
	b.WriteString("name: " + projectName + "\n")
	b.WriteString("services:\n")

	// ── agent ────────────────────────────────────────────────────────────────
	b.WriteString("  agent:\n")
	fmt.Fprintf(&b, "    image: %s\n", p.Image)
	fmt.Fprintf(&b, "    ports:\n      - \"%d:8080\"\n", p.HostPort)
	b.WriteString("    environment:\n")
	writeSortedEnv(&b, agentEnv, "      ")
	b.WriteString("    depends_on:\n")
	b.WriteString("      - gateway\n")
	b.WriteString("      - memory\n")
	b.WriteString("      - discovery\n")
	b.WriteString("    restart: \"no\"\n")

	// ── gateway (mock/real LiteLLM — or the replay mock in --replay mode) ────────
	if p.IsReplay() {
		renderReplayGatewayService(&b, p)
	} else {
		b.WriteString("  gateway:\n")
		fmt.Fprintf(&b, "    image: %s\n", litellmImage)
		b.WriteString("    command: [\"--config\", \"/etc/litellm/config.yaml\", \"--port\", \"4000\"]\n")
		b.WriteString("    volumes:\n")
		b.WriteString("      - ./gateway-config.yaml:/etc/litellm/config.yaml:ro\n")
		if p.Provider == providerReal {
			// The real key is injected from the host env into the gateway container
			// ONLY (never the agent), matching the in-cluster invariant that provider
			// keys live only on the gateway pod. Compose reads DEV_PROVIDER_KEY from the
			// host environment at `up` time.
			b.WriteString("    environment:\n")
			b.WriteString("      DEV_PROVIDER_KEY: ${DEV_PROVIDER_KEY:?set DEV_PROVIDER_KEY for --provider real}\n")
		}
		b.WriteString("    restart: \"no\"\n")
	}

	// ── memory (Valkey) ────────────────────────────────────────────────────────
	b.WriteString("  memory:\n")
	fmt.Fprintf(&b, "    image: %s\n", valkeyImage)
	b.WriteString("    restart: \"no\"\n")

	// ── discovery sidecar ──────────────────────────────────────────────────────
	b.WriteString("  discovery:\n")
	fmt.Fprintf(&b, "    image: %s\n", discoveryImageRef)
	b.WriteString("    environment:\n")
	fmt.Fprintf(&b, "      DISCOVERY_PORT: \"%d\"\n", discoveryPort)
	b.WriteString("      TOOLS_JSON_PATH: /etc/agent/tools.json\n")
	b.WriteString("    volumes:\n")
	b.WriteString("      - ./tools.json:/etc/agent/tools.json:ro\n")
	b.WriteString("    restart: \"no\"\n")

	return b.String()
}

// discoveryImageRef is the tool-discovery sidecar image tag (matches the
// Makefile docker-build-discovery target). `dev` requires it to be built locally
// (documented in the command help + the preflight in dev.go).
const discoveryImageRef = "dev.local/agent-discovery:0.1.0"

// renderReplayGatewayService renders the swapped `gateway` service for replay mode (ADR 0071
// §3a): the replay-serve container under the SAME service name + internal port 4000, so the
// agent's MODEL_GATEWAY_URL (http://gateway:4000/v1) is unchanged and its tool endpoints resolve
// to http://gateway:4000/mcp — BOTH channels served from the fixture by one process. The fixture
// (file or dir) is bind-mounted at /fixture (replayFixtureMountPath) from the work dir, where
// writePlanAssets stages it. restart: "no" so a replay-serve crash surfaces rather than looping.
func renderReplayGatewayService(b *strings.Builder, _ *devPlan) {
	b.WriteString("  gateway:\n")
	fmt.Fprintf(b, "    image: %s\n", replayImageRef())
	fmt.Fprintf(b, "    command: [\"replay-serve\", %q, \"--port\", \"%d\"]\n",
		replayFixtureMountPath, gatewayInternalPort)
	// Publish :4000 so the host CLI can GET /replay/report + /replay/version after the run.
	fmt.Fprintf(b, "    ports:\n      - \"%d:%d\"\n", replayReportHostPort, gatewayInternalPort)
	b.WriteString("    volumes:\n")
	fmt.Fprintf(b, "      - ./fixture:%s:ro\n", replayFixtureMountPath)
	b.WriteString("    restart: \"no\"\n")
}

// writeSortedEnv writes an "environment:" map as sorted "KEY: \"value\"" lines
// (deterministic output for golden tests).
func writeSortedEnv(b *strings.Builder, env map[string]string, indent string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s%s: %q\n", indent, k, env[k])
	}
}
