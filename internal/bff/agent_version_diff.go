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
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// AgentVersionDiffResponse is returned by GET /api/agents/{ns}/{name}/versions/diff (V3, m101.7).
// It is a TEXTUAL line diff of the two AgentVersions' DEPLOYED-SPEC snapshots, rendered as canonical
// YAML (the shape a user sees in `kubectl get -o yaml`). Its shape mirrors PromptVersionDiffResponse
// so the console's "never fabricate a diff" DiffState machine + renderer port over unchanged.
//
// It is a snapshot diff of the deployed spec — NOT the human agent.yaml source-spec (which is not
// stored per-version). The UI captions it as such. resolveMode is always "textual".
type AgentVersionDiffResponse struct {
	ResolveMode string `json:"resolveMode"`
	FromName    string `json:"fromName"`
	ToName      string `json:"toName"`
	// Diff is the unified line diff ("+"/"-"/" " prefixed lines) of the two snapshots' canonical YAML.
	// Empty when the two versions are identical (see Identical).
	Diff string `json:"diff"`
	// Identical is true when the two snapshots are byte-identical — the UI renders a calm "no changes"
	// state rather than an empty box.
	Identical bool `json:"identical"`
}

// handleAgentVersionDiff serves GET /api/agents/{ns}/{name}/versions/diff?from=<v>&to=<v> (V3) — a
// read-only diff of two of the agent's version snapshots. Nested under the agent so the path anchors
// a correctness guard (both versions must belong to {name}); symmetric from/to. Caller-scoped (ADR
// 0011): both AgentVersion reads go through the caller's own client. No optional dependency → no 501.
func (s *Server) handleAgentVersionDiff(w http.ResponseWriter, r *http.Request) {
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
	fromName := strings.TrimSpace(r.URL.Query().Get("from"))
	toName := strings.TrimSpace(r.URL.Query().Get("to"))
	if fromName == "" {
		writeError(w, http.StatusBadRequest, "the 'from' version is required")
		return
	}
	if toName == "" {
		writeError(w, http.StatusBadRequest, "the 'to' version is required")
		return
	}

	fromYAML, ok := s.versionSnapshotYAML(w, r, caller, ns, name, fromName, "from")
	if !ok {
		return
	}
	toYAML, ok := s.versionSnapshotYAML(w, r, caller, ns, name, toName, "to")
	if !ok {
		return
	}

	diff := computeTextualLineDiff(fromYAML, toYAML)
	writeJSON(w, http.StatusOK, AgentVersionDiffResponse{
		ResolveMode: "textual",
		FromName:    fromName,
		ToName:      toName,
		Diff:        diff,
		Identical:   diff == "",
	})
}

// versionSnapshotYAML gets one AgentVersion (caller-scoped), verifies it belongs to the agent named
// in the path, and marshals its Snapshot to canonical YAML (sigs.k8s.io/yaml — honors the json tags +
// omitempty, deterministic key order). On any failure it writes the error and returns ("", false):
// a missing version → 404 (labeled from/to), an RBAC denial → the caller's real 403, a cross-agent
// version → 400, a marshal failure → 500.
func (s *Server) versionSnapshotYAML(w http.ResponseWriter, r *http.Request, caller client.Client, ns, agent, versionName, label string) (string, bool) {
	var av agentsv1alpha1.AgentVersion
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: versionName}, &av); err != nil {
		s.writeGetError(w, err, "the "+label+" version")
		return "", false
	}
	// Path-anchored correctness guard: a diff across DIFFERENT agents is meaningless.
	if av.Spec.DeploymentName != agent {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("version %q does not belong to agent %q", versionName, agent))
		return "", false
	}
	raw, err := yaml.Marshal(av.Spec.Snapshot)
	if err != nil {
		s.log.Error(err, "agent version diff: marshal snapshot failed", "version", versionName)
		writeError(w, http.StatusInternalServerError, "failed to render the "+label+" version")
		return "", false
	}
	return string(raw), true
}
