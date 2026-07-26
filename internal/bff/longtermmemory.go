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

// LongTermMemoryConfig is the agent's long-term-memory capability (M46's folded AgentDeployment field). This
// is the ENABLE surface (m49.3) — the console could VIEW an agent's long-term memories but had no way to turn
// the capability ON (the m49.1 capability pocket). Like tracepolicy, it patches the folded field directly, so
// it works for any agent without an agent.yaml/expand round-trip.
type LongTermMemoryConfig struct {
	Enabled        bool   `json:"enabled"`
	PerUser        bool   `json:"perUser"`
	EmbeddingRoute string `json:"embeddingRoute,omitempty"`
}

func longTermMemoryConfigOf(ad *agentsv1alpha1.AgentDeployment) LongTermMemoryConfig {
	if lt := ad.Spec.LongTermMemory; lt != nil {
		return LongTermMemoryConfig{Enabled: lt.Enabled, PerUser: lt.PerUser, EmbeddingRoute: lt.EmbeddingRoute}
	}
	return LongTermMemoryConfig{}
}

// handleGetLongTermMemoryConfig serves GET /api/agents/{ns}/{name}/longtermmemory — the current capability
// (so the console can show the toggle state). Caller-scoped (ADR 0011).
func (s *Server) handleGetLongTermMemoryConfig(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, longTermMemoryConfigOf(&ad))
}

// handleUpdateLongTermMemory serves PUT /api/agents/{ns}/{name}/longtermmemory — enable/configure the
// capability by patching spec.longTermMemory. Disabled clears the folded field entirely.
func (s *Server) handleUpdateLongTermMemory(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req LongTermMemoryConfig
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

	// Off ⇒ clear the block (the folded field is nil when disabled).
	if !req.Enabled {
		ad.Spec.LongTermMemory = nil
	} else {
		ad.Spec.LongTermMemory = &agentsv1alpha1.LongTermMemorySpec{
			Enabled:        true,
			PerUser:        req.PerUser,
			EmbeddingRoute: strings.TrimSpace(req.EmbeddingRoute),
		}
	}

	if err := caller.Update(r.Context(), &ad); err != nil {
		ce := classifyCreateError(err, agentDeploymentKind, ad.Name)
		writeError(w, ce.status, ce.msg)
		return
	}
	writeJSON(w, http.StatusOK, longTermMemoryConfigOf(&ad))
}
