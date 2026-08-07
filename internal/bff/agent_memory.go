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
	"net/http"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/agentmemory"
)

// AgentMemoryDTO is one long-term memory projected for the console (ADR 0045). The raw embedding vector is
// never included, and per-user memories are NOT listed — only agent-wide knowledge — so no user's private
// remembered facts (nor their hashed identity) are exposed in the admin console.
type AgentMemoryDTO struct {
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
	// TraceID is the originating run's trace id (m54.3), lifted from the internal
	// `traceId` tag the launcher stamps — the console back-links each remembered
	// fact to the trace that produced it (M49 UX review A2). Empty when the memory
	// was written outside a traced run. It is removed from Tags so it does not also
	// render as a user-facing tag chip.
	TraceID   string `json:"traceId,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// memoryTraceTagKey is the tag key the launcher stamps the originating trace id
// under (cmd/launcher/memory_longterm.go). Lifted into the typed DTO field.
const memoryTraceTagKey = "traceId"

// splitTraceTag lifts the traceId tag out of a memory's tags into a typed value,
// returning the trace id and the tags WITHOUT it (so it isn't shown as a chip). The
// input map is not mutated.
func splitTraceTag(tags map[string]string) (traceID string, rest map[string]string) {
	traceID = tags[memoryTraceTagKey]
	if traceID == "" {
		return "", tags
	}
	rest = make(map[string]string, len(tags))
	for k, v := range tags {
		if k != memoryTraceTagKey {
			rest[k] = v
		}
	}
	return traceID, rest
}

// AgentMemoryResponse is returned by GET /api/agents/{ns}/{name}/memory.
type AgentMemoryResponse struct {
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Items     []AgentMemoryDTO `json:"items"`
}

const (
	defaultAgentMemoryLimit = 50
	maxAgentMemoryLimit     = 200
)

// handleAgentMemory serves GET /api/agents/{ns}/{name}/memory — an agent's AGENT-WIDE long-term memories
// (ADR 0045, m46.6), newest-first. Caller-scoped (ADR 0011): the caller must be able to `get` the agent
// through their own client, exactly like the detail route — a denial is 403, a missing agent 404. The
// per-user (agent_user) memories are deliberately excluded (they are users' private data + hashed identity).
func (s *Server) handleAgentMemory(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}
	if s.agentMemoryStore == nil {
		writeError(w, http.StatusNotImplemented, "long-term memory requires the control-plane store (CONTROLPLANE_DSN)")
		return
	}

	// Caller-scoped existence + authz gate: verify the caller can `get` the agent BEFORE any store read,
	// so no memory is returned for an agent the caller may not see.
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	limit := parseAgentMemoryLimit(r.URL.Query().Get("limit"))
	mems, err := s.agentMemoryStore.List(r.Context(), agentmemory.ListOptions{
		Namespace: ns, AgentName: name, Scope: agentmemory.ScopeAgent, Limit: limit,
	})
	if err != nil {
		s.log.Error(err, "list agent memories failed", "namespace", ns, "agent", name)
		writeError(w, http.StatusInternalServerError, "failed to list agent memories")
		return
	}
	items := make([]AgentMemoryDTO, 0, len(mems))
	for i := range mems {
		traceID, tags := splitTraceTag(mems[i].Tags)
		items = append(items, AgentMemoryDTO{
			Content:   mems[i].Content,
			Tags:      tags,
			TraceID:   traceID,
			CreatedAt: mems[i].CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, AgentMemoryResponse{Namespace: ns, Name: name, Items: items})
}

// parseAgentMemoryLimit resolves ?limit to a bounded page size (default 50, capped at 200).
func parseAgentMemoryLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return defaultAgentMemoryLimit
	}
	if n > maxAgentMemoryLimit {
		return maxAgentMemoryLimit
	}
	return n
}
