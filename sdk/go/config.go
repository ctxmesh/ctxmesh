// Package ctxmesh is the Go SDK for agents running on ctxmesh.
//
// An agent runs in a pod beside the platform's sidecars, and this package is the typed way to
// reach them over localhost: conversation memory, long-term memory, tools, knowledge bases,
// feedback, agent-to-agent calls, delegation and handoff.
//
// It holds no credentials. Endpoints and identity arrive in the environment the platform
// injects, so there is no API key to manage and no base URL to configure.
//
// Conformance tier: plane-client (ADR 0139). Every launcher route is reachable here; the
// managed agent loop, tool dispatch and model client are authoring-tier and live in the Python
// and TypeScript SDKs.
package ctxmesh

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Ports the launcher binds. Their absence is how the SDK knows a capability is not wired for
// this agent.
const (
	defaultMemoryPort   = 2998
	defaultFeedbackPort = 2995
	defaultAMPPort      = 2997
)

// Config is the resolved plane for one agent process.
type Config struct {
	MemoryPort   int
	FeedbackPort int
	AMPPort      int

	AgentName       string
	AgentVersion    string
	AgentRole       string
	AgentRegistryID string
	ConversationID  string
	PromptVersion   string

	// MemoryWired is false when MEMORY_PORT was absent. Calling a memory method then returns
	// ErrNotWired rather than dialling a port nothing is listening on — the platform's way of
	// saying this agent was not granted memory.
	MemoryWired   bool
	FeedbackWired bool

	LongTermEnabled  bool
	KnowledgeEnabled bool
}

// FromEnv reads the configuration the launcher injects.
//
// It returns ErrNotInPod when none of the launcher's markers are present, which is the case
// that matters in practice: running an agent binary on a laptop should say so plainly, not
// fail later with a connection-refused to localhost:2998.
func FromEnv() (*Config, error) {
	return fromEnv(os.LookupEnv)
}

// FromConfig builds a Config explicitly, for tests and offline work.
func FromConfig(c Config) *Config { return &c }

func fromEnv(look func(string) (string, bool)) (*Config, error) {
	get := func(k string) string { v, _ := look(k); return strings.TrimSpace(v) }

	// A launcher-injected environment always carries at least one of these. Without any, the
	// process is not in a ctxmesh pod and every port we could pick would be a guess.
	inPod := false
	for _, marker := range []string{"MEMORY_PORT", "FEEDBACK_PORT", "AGENT_NAME", "MODEL_GATEWAY_URL"} {
		if _, ok := look(marker); ok {
			inPod = true
			break
		}
	}
	if !inPod {
		return nil, ErrNotInPod
	}

	mem, memSet, err := port(look, "MEMORY_PORT", defaultMemoryPort)
	if err != nil {
		return nil, err
	}
	fb, fbSet, err := port(look, "FEEDBACK_PORT", defaultFeedbackPort)
	if err != nil {
		return nil, err
	}
	amp, _, err := port(look, "AMP_PORT", defaultAMPPort)
	if err != nil {
		return nil, err
	}

	return &Config{
		MemoryPort:       mem,
		FeedbackPort:     fb,
		AMPPort:          amp,
		AgentName:        get("AGENT_NAME"),
		AgentVersion:     get("AGENT_VERSION"),
		AgentRole:        get("AGENT_ROLE"),
		AgentRegistryID:  get("AGENT_REGISTRY_ID"),
		ConversationID:   get("CONVERSATION_ID"),
		PromptVersion:    get("PROMPT_VERSION"),
		MemoryWired:      memSet,
		FeedbackWired:    fbSet,
		LongTermEnabled:  get("MEMORY_LONGTERM_ENABLED") == "true",
		KnowledgeEnabled: get("KNOWLEDGE_BASE_ENABLED") == "true",
	}, nil
}

// port reports the value, and whether it was explicitly SET — the caller needs the difference,
// because an unset port means the capability is not wired, not that it is on the default.
func port(look func(string) (string, bool), name string, def int) (int, bool, error) {
	raw, ok := look(name)
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		return def, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s=%q is not a port", ErrNotInPod, name, raw)
	}
	if n < 1 || n > 65535 {
		return 0, false, fmt.Errorf("%w: %s=%d out of range (1..65535)", ErrNotInPod, name, n)
	}
	return n, true, nil
}

func (c *Config) memoryBase() string   { return fmt.Sprintf("http://127.0.0.1:%d", c.MemoryPort) }
func (c *Config) feedbackBase() string { return fmt.Sprintf("http://127.0.0.1:%d", c.FeedbackPort) }
func (c *Config) ampBase() string      { return fmt.Sprintf("http://127.0.0.1:%d", c.AMPPort) }
