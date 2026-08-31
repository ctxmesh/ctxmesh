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
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// feedbackUnattributed labels a score whose name is not declared by the agent's bound FeedbackStore — its
// raw data still returns (never hidden), just without a source (ADR 0112 §4).
const feedbackUnattributed = "unattributed"

// handleSubmitFeedback serves POST /api/feedback — the console/external WRITE path for the phase-2
// FeedbackStore (M139, ADR 0112). Caller-scoped (ADR 0011): a caller may only submit feedback on a trace
// whose agent they can read — authorizeRunAccess resolves trace→run→agent, which also defeats trace-id
// forgery (access to agent A cannot poison agent B). When the agent binds a FeedbackStore, the submitted
// score name is gated by the declared model per its mode (Enforce rejects an undeclared name; Monitor
// accepts). The score is RELAYED to Langfuse — the store of record (ADR 0008); the post-hoc online-score
// fold reads Langfuse, so a submitted score is folded on the next window (no bypass, verified).
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

	// Gate the score name by the agent's declared feedback model (fail-closed on an unreadable ref).
	store, err := s.resolveFeedbackStore(r, caller, rn.Namespace, rn.Agent)
	if err != nil {
		writeError(w, http.StatusForbidden, "cannot read the feedback store governing this agent")
		return
	}
	if store != nil && store.Spec.Mode != agentsv1beta1.FeedbackMonitor {
		if _, declared := feedbackSourceByName(store)[name]; !declared {
			writeError(w, http.StatusUnprocessableEntity,
				"score name \""+name+"\" is not declared by the agent's FeedbackStore (mode Enforce)")
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

// resolveFeedbackStore fetches the FeedbackStore bound to an agent via feedbackStoreRef (caller-scoped,
// ADR 0011). Returns (nil, nil) when the agent (or its ref) is absent — today's open relay, unchanged. An
// unreadable/dangling ref returns an error so the WRITE path can fail closed (we cannot know the declared
// model); the READ path treats that as best-effort (no attribution).
func (s *Server) resolveFeedbackStore(r *http.Request, caller client.Client, ns, agentName string) (*agentsv1beta1.FeedbackStore, error) {
	var agent agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: agentName}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // agent gone ⇒ nothing to enforce
		}
		return nil, err
	}
	ref := strings.TrimSpace(agent.Spec.FeedbackStoreRef)
	if ref == "" {
		return nil, nil // no bound store ⇒ open relay (unchanged)
	}
	var store agentsv1beta1.FeedbackStore
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: ref}, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// feedbackSourceByName maps each declared score name → its source label ("human" or "external:<channel>")
// for the FeedbackStore (ADR 0112 §4). The reconciler guarantees names are unique across sources, so the
// map is unambiguous.
func feedbackSourceByName(store *agentsv1beta1.FeedbackStore) map[string]string {
	m := map[string]string{}
	if store == nil {
		return m
	}
	if store.Spec.Human != nil {
		for i := range store.Spec.Human.Scores {
			m[store.Spec.Human.Scores[i].Name] = "human"
		}
	}
	for i := range store.Spec.External {
		m[store.Spec.External[i].Score.Name] = "external:" + store.Spec.External[i].Name
	}
	return m
}

// attributeFeedback tags each score with the source declared by the agent's FeedbackStore (ADR 0112 §4):
// Langfuse stamps every API-written score Source=API, so it cannot itself distinguish human from external —
// the CRD's name→source map IS the attribution. Best-effort: no bound store / an unreadable ref leaves
// scores unattributed (the raw data still returns). A declared name → its source; an undeclared name under
// a bound store → "unattributed".
func (s *Server) attributeFeedback(r *http.Request, caller client.Client, rn *run.Run, scores []FeedbackScore) {
	store, err := s.resolveFeedbackStore(r, caller, rn.Namespace, rn.Agent)
	if err != nil || store == nil {
		return
	}
	byName := feedbackSourceByName(store)
	for i := range scores {
		if src, ok := byName[scores[i].Name]; ok {
			scores[i].AttributedSource = src
		} else {
			scores[i].AttributedSource = feedbackUnattributed
		}
	}
}
