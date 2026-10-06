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
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// feedbackUnattributed labels a score whose name the agent's spec.feedback does not declare. Its raw
// data still returns (never hidden), just without a source (ADR 0152 §3).
const feedbackUnattributed = "unattributed"

// handleSubmitFeedback serves POST /api/feedback, the console/external WRITE path. Caller-scoped (ADR
// 0011): a caller may only submit feedback on a trace whose agent they can read. authorizeRunAccess
// resolves trace→run→agent, which also defeats trace-id forgery (access to agent A cannot poison agent
// B). When the agent declares spec.feedback, the submitted score name is gated by it per its mode
// (Enforce rejects an undeclared name; Monitor accepts). The score is RELAYED to Langfuse, the store of
// record (ADR 0008).
func (s *Server) handleSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var req SubmitFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	traceID := strings.TrimSpace(req.TraceID)
	name := strings.TrimSpace(req.Name)
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "traceId is required")
		return
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (the score dimension)")
		return
	}

	// Caller-scoped authz + anti-forgery: resolve trace→run→agent and prove the caller can read that agent.
	rn, ok := s.authorizeRunAccess(w, r, caller, traceID, true)
	if !ok {
		return
	}

	// Gate the score name by the agent's declaration. An agent we cannot read fails closed: we cannot
	// know what it declares.
	spec, err := s.agentFeedbackSpec(r, caller, rn.Namespace, rn.Agent)
	if err != nil {
		writeError(w, http.StatusForbidden, "cannot read the feedback declaration governing this agent")
		return
	}
	if spec != nil && spec.Mode != agentsv1alpha1.FeedbackMonitor {
		if _, declared := feedbackSourceByName(spec)[name]; !declared {
			writeError(w, http.StatusUnprocessableEntity,
				"score name \""+name+"\" is not declared by the agent's spec.feedback (mode Enforce)")
			return
		}
	}

	if err := s.adapters.Langfuse.CreateScore(r.Context(), traceID, name, req.Value, req.Comment); err != nil {
		s.log.Error(err, "relay feedback to langfuse failed", "traceID", traceID, "name", name)
		writeError(w, http.StatusBadGateway, "failed to record feedback")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// agentFeedbackSpec returns the agent's spec.feedback, read caller-scoped (ADR 0011). (nil, nil) when
// the agent is gone or declares nothing: the open relay. Any other read error is returned so the WRITE
// path can fail closed; the READ path treats it as best-effort (no attribution).
func (s *Server) agentFeedbackSpec(r *http.Request, caller client.Client, ns, agentName string) (*agentsv1alpha1.FeedbackSpec, error) {
	var agent agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: agentName}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return agent.Spec.Feedback, nil
}

// feedbackSourceByName maps each declared score name to its source label ("human" or
// "external:<channel>"). The CRD's CEL rules keep names unique across sources, so the map is
// unambiguous.
func feedbackSourceByName(spec *agentsv1alpha1.FeedbackSpec) map[string]string {
	m := map[string]string{}
	if spec == nil {
		return m
	}
	if spec.Human != nil {
		for i := range spec.Human.Scores {
			m[spec.Human.Scores[i].Name] = "human"
		}
	}
	for i := range spec.External {
		m[spec.External[i].Score.Name] = "external:" + spec.External[i].Name
	}
	return m
}

// attributeFeedback tags each score with the source the agent's spec.feedback declares for its name.
// Langfuse stamps every API-written score Source=API, so it cannot itself tell human from external; the
// declaration's name→source map IS the attribution. Best-effort: no declaration, or an unreadable agent,
// leaves scores unattributed (the raw data still returns). An undeclared name under a declaration is
// labelled "unattributed".
func (s *Server) attributeFeedback(r *http.Request, caller client.Client, rn *run.Run, scores []FeedbackScore) {
	spec, err := s.agentFeedbackSpec(r, caller, rn.Namespace, rn.Agent)
	if err != nil || spec == nil {
		return
	}
	byName := feedbackSourceByName(spec)
	for i := range scores {
		if src, ok := byName[scores[i].Name]; ok {
			scores[i].AttributedSource = src
		} else {
			scores[i].AttributedSource = feedbackUnattributed
		}
	}
}
