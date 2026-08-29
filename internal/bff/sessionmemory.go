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
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// sessionMemoryScopeSession is the private per-agent scope (the CRD default). perUser is only meaningful
// for this scope — the "shared" team scratchpad is per-conversation by design.
const sessionMemoryScopeSession = "session"

// SessionMemoryConfig is the console ENABLE/toggle surface for spec.sessionMemory (M137/EU1d, ADR 0080).
// M98 shipped the CRD + runtime for perUser session memory but no console affordance — an operator could
// only set it via YAML. Like longtermmemory, it patches the folded field directly (no agent.yaml round
// trip). It carries just what the toggle needs; the Backend (if configured) is preserved across a PUT.
type SessionMemoryConfig struct {
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope,omitempty"`
	// PerUser isolates each end-user's conversation memory into its own bucket. PRODUCT-grade, not
	// security-grade (launcher-stamped inside the pod boundary); only meaningful for the private
	// ("session") scope; breaks conversation handoff + share-links for the agent (ADR 0080). Default off.
	PerUser bool `json:"perUser"`
}

func sessionMemoryConfigOf(ad *agentsv1alpha1.AgentDeployment) SessionMemoryConfig {
	sm := ad.Spec.SessionMemory
	if sm == nil {
		return SessionMemoryConfig{}
	}
	scope := sm.Scope
	if scope == "" {
		scope = sessionMemoryScopeSession // the CRD default
	}
	return SessionMemoryConfig{Enabled: true, Scope: scope, PerUser: sm.PerUser}
}

// handleGetSessionMemoryConfig serves GET /api/agents/{ns}/{name}/sessionmemory — the current session-memory
// config (so the console can render the perUser toggle state). Caller-scoped (ADR 0011).
func (s *Server) handleGetSessionMemoryConfig(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(),
		client.ObjectKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, sessionMemoryConfigOf(&ad))
}

// handleUpdateSessionMemory serves PUT /api/agents/{ns}/{name}/sessionmemory — enable/configure session
// memory by patching spec.sessionMemory. Disabled clears the folded field entirely. The existing Backend
// (if any) is preserved; perUser is only kept for the private ("session") scope (the CRD ignores it for
// the shared scratchpad — we normalize it off to keep the persisted spec honest).
func (s *Server) handleUpdateSessionMemory(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req SessionMemoryConfig
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(),
		client.ObjectKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	if !req.Enabled {
		ad.Spec.SessionMemory = nil
	} else {
		scope := strings.TrimSpace(req.Scope)
		if scope == "" {
			scope = sessionMemoryScopeSession
		}
		// Preserve a configured backend across the toggle (never clobber it).
		var backend *agentsv1alpha1.MemoryBackend
		if sm := ad.Spec.SessionMemory; sm != nil {
			backend = sm.Backend
		}
		// perUser is meaningful only for the private session scope (the CRD ignores it for "shared").
		perUser := req.PerUser && scope == sessionMemoryScopeSession
		ad.Spec.SessionMemory = &agentsv1alpha1.SessionMemorySpec{
			Scope:   scope,
			PerUser: perUser,
			Backend: backend,
		}
	}

	if err := caller.Update(r.Context(), &ad); err != nil {
		ce := classifyCreateError(err, agentDeploymentKind, ad.Name)
		writeError(w, ce.status, ce.msg)
		return
	}
	writeJSON(w, http.StatusOK, sessionMemoryConfigOf(&ad))
}
