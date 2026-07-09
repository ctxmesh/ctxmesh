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

// Package main implements the agent-engine launcher — PID 1 for agent containers.
//
// M3 evolution: the launcher no longer exec-replaces the user process. It
// spawns the user process as a child (os/exec), stays alive, and operates as a
// reverse proxy. This lets it emit a language-agnostic agent.invoke boundary
// span on every /invoke request and propagate W3C trace context so downstream
// instrumentation nests beneath it.
//
// Port model:
//
//	$AGENT_PORT (default 8080)          ← Knative routes here → launcher proxy
//	$AGENT_UPSTREAM_PORT (default 8081) ← child process listens here (internal)
//
// Signal / exit contract (M1 semantics preserved):
//
//	SIGTERM/SIGINT are forwarded to the child.
//	Orphaned children (reparented to PID 1) are reaped via Wait4(-1).
//	The launcher exits with the child's exit code.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// defaultOTLPEndpoint is the default gRPC address for the OTel collector
// sidecar (see observability.md — collector runs at localhost:4317 in the pod).
const defaultOTLPEndpoint = "localhost:4317"

// Config holds the parsed launcher configuration.
type Config struct {
	// Argv is the argument vector for the child process: Argv[0] is the agent
	// binary path; remaining elements are its command-line arguments.
	Argv []string

	// ProxyPort is the TCP port the launcher listens on (what Knative routes
	// to). Env: AGENT_PORT, default 8080.
	ProxyPort int

	// UpstreamPort is the TCP port the child process listens on (internal).
	// The launcher overrides AGENT_PORT in the child's environment to this
	// value so the child binds here rather than on the proxy port.
	// Env: AGENT_UPSTREAM_PORT, default 8081.
	UpstreamPort int

	// OTLPEndpoint is the gRPC address of the OTel collector.
	// Env: OTEL_EXPORTER_OTLP_ENDPOINT, default "localhost:4317".
	OTLPEndpoint string

	// AgentName, AgentVersion, AgentRoute are attached as span attributes on
	// every agent.invoke span. Envs: AGENT_NAME, AGENT_VERSION, AGENT_ROUTE.
	AgentName    string
	AgentVersion string
	AgentRoute   string

	// Memory holds the :2998 memory-endpoint configuration. The listener is
	// started ONLY when Memory.BackendAddr is non-empty (i.e. MEMORY_BACKEND_ADDR
	// is injected by the controller for an agent with a MemoryBinding).
	Memory memoryConfig

	// A2A holds the agent-to-agent mesh configuration. The outbound /a2a
	// listener is started ONLY when A2A.RegistryID is non-empty (i.e.
	// AGENT_REGISTRY_ID is injected because the agent is a resolved
	// AgentRegistry member); inbound access control is likewise a no-op without
	// it.
	A2A a2aConfig

	// ObjectStore holds the blob-offload configuration for the async path. The
	// offloader is constructed ONLY when ObjectStore.Addr is non-empty (i.e.
	// OBJECT_STORE_ADDR is injected because the agent participates in the
	// eventing path); otherwise offload is disabled and async payloads pass
	// through capped.
	ObjectStore objectStoreConfig

	// Gateway holds the outbound cost-budget gateway-proxy configuration (M8).
	// The proxy listener is started ONLY when Gateway.UpstreamURL is non-empty
	// AND a cap is set (i.e. the controller injected spec.budget). Otherwise the
	// agent's MODEL_GATEWAY_URL points straight at LiteLLM and there is zero
	// budget overhead.
	Gateway gatewayConfig
}

// loadConfig reads launcher configuration from environment variables.
//
// Environment variables:
//
//	AGENT_ENTRYPOINT (required): absolute path to the agent binary.
//	AGENT_ENTRYPOINT_ARGS (optional): extra arguments, split on whitespace.
//	AGENT_PORT (optional): proxy listen port (default 8080).
//	AGENT_UPSTREAM_PORT (optional): child listen port (default 8081).
//	OTEL_EXPORTER_OTLP_ENDPOINT (optional): OTLP gRPC target (default localhost:4317).
//	AGENT_NAME, AGENT_VERSION, AGENT_ROUTE (optional): span attributes.
//
// The lookup parameter is a function that resolves env vars by name
// (typically os.Getenv); it is a parameter so the pure parsing logic can be
// exercised in unit tests without mutating process state.
func loadConfig(lookup func(string) string) (Config, error) {
	ep := lookup("AGENT_ENTRYPOINT")
	if ep == "" {
		return Config{}, fmt.Errorf(
			"AGENT_ENTRYPOINT is not set: set it to the absolute path of the agent binary",
		)
	}

	argv := []string{ep}
	if args := lookup("AGENT_ENTRYPOINT_ARGS"); args != "" {
		argv = append(argv, strings.Fields(args)...)
	}

	proxyPort, err := parsePort(lookup("AGENT_PORT"), 8080)
	if err != nil {
		return Config{}, fmt.Errorf("AGENT_PORT: %w", err)
	}

	upstreamPort, err := parsePort(lookup("AGENT_UPSTREAM_PORT"), 8081)
	if err != nil {
		return Config{}, fmt.Errorf("AGENT_UPSTREAM_PORT: %w", err)
	}

	otlpEndpoint := lookup("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otlpEndpoint == "" {
		otlpEndpoint = defaultOTLPEndpoint
	}

	agentName := lookup("AGENT_NAME")

	mem, err := loadMemoryConfig(lookup, agentName)
	if err != nil {
		return Config{}, err
	}

	a2a, err := loadA2AConfig(lookup, agentName)
	if err != nil {
		return Config{}, err
	}

	objStore := loadObjectStoreConfig(lookup)

	gw, err := loadGatewayConfig(lookup, agentName)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Argv:         argv,
		ProxyPort:    proxyPort,
		UpstreamPort: upstreamPort,
		OTLPEndpoint: otlpEndpoint,
		AgentName:    agentName,
		AgentVersion: lookup("AGENT_VERSION"),
		AgentRoute:   lookup("AGENT_ROUTE"),
		Memory:       mem,
		A2A:          a2a,
		ObjectStore:  objStore,
		Gateway:      gw,
	}, nil
}

// objectStoreConfig is the blob-offload configuration for the async path
// (m7.6b). Parsed from env alongside the launcher Config.
type objectStoreConfig struct {
	// Addr is OBJECT_STORE_ADDR — the MinIO host:port. Empty ⇒ offload is
	// DISABLED (no offloader constructed): oversize async payloads are not
	// offloaded and pass through capped, exactly as before this feature.
	Addr string
	// AccessKey / SecretKey are the dev object-store credentials
	// (OBJECT_STORE_ACCESS_KEY / OBJECT_STORE_SECRET_KEY). Deterministic dev-only
	// fixed values injected by the controller as STATIC env (no valueFrom — the
	// Knative ksvc constraint); NEVER a real credential.
	AccessKey string
	SecretKey string
}

// loadObjectStoreConfig parses the blob-offload configuration from env.
//
// Environment variables:
//
//	OBJECT_STORE_ADDR (gate): the MinIO host:port. Empty ⇒ offload is disabled
//	  (no offloader is built); every other object-store env is then irrelevant.
//	OBJECT_STORE_ACCESS_KEY / OBJECT_STORE_SECRET_KEY: the dev credentials.
//
// Like loadMemoryConfig / loadA2AConfig, it does NOT hard-fail on missing
// credentials when the gate is set — an empty credential is a
// visible-but-non-fatal misconfiguration (the first PUT/GET surfaces the auth
// error) rather than a crash on a best-effort path.
func loadObjectStoreConfig(lookup func(string) string) objectStoreConfig {
	addr := lookup("OBJECT_STORE_ADDR")
	if addr == "" {
		return objectStoreConfig{}
	}
	return objectStoreConfig{
		Addr:      addr,
		AccessKey: lookup("OBJECT_STORE_ACCESS_KEY"),
		SecretKey: lookup("OBJECT_STORE_SECRET_KEY"),
	}
}

// ObjectStoreEnabled reports whether blob offload should be wired — true iff an
// object-store address was injected.
func (c Config) ObjectStoreEnabled() bool {
	return c.ObjectStore.Addr != ""
}

// loadMemoryConfig parses the :2998 memory-endpoint configuration from env.
//
// Environment variables:
//
//	MEMORY_BACKEND_ADDR (gate): Valkey host:port. Empty ⇒ the listener is NOT
//	  started at all (the agent has no MemoryBinding); every other memory env is
//	  then irrelevant.
//	MEMORY_PORT (optional): memory listener port (default 2998).
//	MEMORY_KEY_NAMESPACE (optional): key-prefix namespace; falls back to
//	  POD_NAMESPACE (the pod's namespace, injected via the downward API by the
//	  controller). The agent portion of the key reuses AGENT_NAME.
//
// The key layout is mem:{namespace}/{agent}:{conversationId}. When
// MEMORY_BACKEND_ADDR is set, the function does NOT hard-fail on a missing
// namespace/agent (it degrades to empty segments) — the controller is
// responsible for injecting them; an empty segment is a visible-but-non-fatal
// misconfiguration rather than a crash on a best-effort path.
func loadMemoryConfig(lookup func(string) string, agentName string) (memoryConfig, error) {
	addr := lookup("MEMORY_BACKEND_ADDR")
	if addr == "" {
		// Not gated on: the listener is skipped entirely.
		return memoryConfig{}, nil
	}

	port, err := parsePort(lookup("MEMORY_PORT"), defaultMemoryPort)
	if err != nil {
		return memoryConfig{}, fmt.Errorf("MEMORY_PORT: %w", err)
	}

	ns := lookup("MEMORY_KEY_NAMESPACE")
	if ns == "" {
		ns = lookup("POD_NAMESPACE")
	}

	return memoryConfig{
		BackendAddr: addr,
		Port:        port,
		Namespace:   ns,
		Agent:       agentName,
	}, nil
}

// MemoryEnabled reports whether the :2998 memory listener should be started —
// true iff a backend address was injected.
func (c Config) MemoryEnabled() bool {
	return c.Memory.BackendAddr != ""
}

// parsePort parses a port string (may be empty) and returns the result.
// If val is empty, defaultPort is returned. Returns an error if val is not a
// valid TCP port (1–65535).
func parsePort(val string, defaultPort int) (int, error) {
	if val == "" {
		return defaultPort, nil
	}
	p, err := strconv.Atoi(val)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q: must be an integer in 1–65535", val)
	}
	return p, nil
}

// validateEntrypoint checks that the binary named in cfg.Argv[0] exists on the
// filesystem and has at least one executable bit set. It does not attempt to
// exec the binary — the caller is responsible for starting the child process.
func validateEntrypoint(cfg Config) error {
	ep := cfg.Argv[0]

	info, err := os.Stat(ep)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("entrypoint %q: file not found", ep)
		}
		return fmt.Errorf("entrypoint %q: %w", ep, err)
	}
	if info.IsDir() {
		return fmt.Errorf("entrypoint %q: is a directory, not an executable binary", ep)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("entrypoint %q: not executable (mode %s)", ep, info.Mode())
	}

	return nil
}

// shouldSpan reports whether an HTTP request path should be wrapped in an
// agent.invoke tracer span.
//
// /healthz and /readyz are health probes: they pass through the proxy
// unwrapped to avoid polluting traces with probe traffic. All other paths
// (primarily /invoke) get a boundary span.
func shouldSpan(path string) bool {
	switch path {
	case "/healthz", "/readyz":
		return false
	default:
		return true
	}
}

// buildChildEnv returns the environment slice for the child process.
// All current env vars are inherited; AGENT_PORT is replaced with the
// upstream port so the child listens on the internal port, not the proxy port.
func buildChildEnv(cfg Config, environ []string) []string {
	upstreamVal := "AGENT_PORT=" + strconv.Itoa(cfg.UpstreamPort)
	out := make([]string, 0, len(environ)+1)
	found := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, "AGENT_PORT=") {
			out = append(out, upstreamVal)
			found = true
		} else {
			out = append(out, kv)
		}
	}
	if !found {
		out = append(out, upstreamVal)
	}
	return out
}
