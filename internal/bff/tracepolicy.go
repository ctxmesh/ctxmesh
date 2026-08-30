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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// The redaction-policy editor (m18.13, ADR 0019). An agent's spec.tracePolicy is
// the per-agent EXTENSION of the always-on trace-redaction policy — extra named
// regex detectors applied at the collector before persistence. This is the ONE
// governance surface the console can edit today: the built-in detectors
// (emails/keys/SSNs) are always on and not listed here.
//
// NOTE (audit viewer, deferred): the OTHER ADR-0019 surface — an audit-log viewer —
// is NOT implemented here. The audit trail is emitted to the controller-manager's
// logs (internal/audit LogSink), and the BFF is a SEPARATE process with an empty-
// rules SA (ADR 0011), so it cannot read a cross-process, log-only trail without a
// queryable audit store. That store is a follow-up (ADR 0019 records this).

// CustomDetectorDTO is one custom redaction rule: a name (in the marker) + an RE2
// pattern. No secret material.
type CustomDetectorDTO struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// TracePolicyResponse is the agent's per-agent redaction extension. CustomDetectors
// is non-nil on the wire ([] not null).
type TracePolicyResponse struct {
	CustomDetectors []CustomDetectorDTO `json:"customDetectors"`
}

// handleGetTracePolicy serves GET /api/agents/{ns}/{name}/tracepolicy — the agent's
// custom redaction detectors, read CALLER-SCOPED (ADR 0011).
func (s *Server) handleGetTracePolicy(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, tracePolicyResponse(&ad))
}

// handleUpdateTracePolicy serves PUT /api/agents/{ns}/{name}/tracepolicy — replace
// the agent's custom redaction detectors, CALLER-SCOPED (a caller without update is
// denied by the API server → 403). An invalid detector (bad name/regex) is rejected
// by the CRD's own validation and surfaced as a 422. It is a read-modify-write of
// the live object, so no other spec field is touched.
func (s *Server) handleUpdateTracePolicy(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req TracePolicyResponse
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

	// nil when empty so clearing the last detector removes the block entirely.
	detectors := make([]agentsv1alpha1.CustomDetector, 0, len(req.CustomDetectors))
	for _, d := range req.CustomDetectors {
		detectors = append(detectors, agentsv1alpha1.CustomDetector{
			Name:    strings.TrimSpace(d.Name),
			Pattern: d.Pattern,
		})
	}
	if len(detectors) == 0 {
		ad.Spec.TracePolicy = nil
	} else {
		ad.Spec.TracePolicy = &agentsv1alpha1.TracePolicy{CustomDetectors: detectors}
	}

	if err := caller.Update(r.Context(), &ad); err != nil {
		ce := classifyCreateError(err, agentDeploymentKind, ad.Name)
		writeError(w, ce.status, ce.msg)
		return
	}
	writeJSON(w, http.StatusOK, tracePolicyResponse(&ad))
}

// tracePolicyResponse projects an agent's spec.tracePolicy onto the DTO.
func tracePolicyResponse(ad *agentsv1alpha1.AgentDeployment) TracePolicyResponse {
	out := TracePolicyResponse{CustomDetectors: make([]CustomDetectorDTO, 0)}
	if ad.Spec.TracePolicy != nil {
		for _, d := range ad.Spec.TracePolicy.CustomDetectors {
			out.CustomDetectors = append(out.CustomDetectors, CustomDetectorDTO{Name: d.Name, Pattern: d.Pattern})
		}
	}
	return out
}
