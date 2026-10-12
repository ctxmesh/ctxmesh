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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// Agent version history lives in the control-plane store (ADR 0042), so the API server no longer
// authorizes reading it: each read is authorized with a caller-scoped SelfSubjectAccessReview on the
// `agentversions` resource (ADR 0044), which the persona roles grant.
const resourceAgentVersions = "agentversions"

// msgVersionStoreRequired is the honest 501 when the install has no control-plane database.
const msgVersionStoreRequired = "agent version history requires the control-plane database; this install has none"

// AgentVersionDiffResponse is returned by GET /api/agents/{ns}/{name}/versions/diff (V3, m101.7).
// It is a TEXTUAL line diff of two versions' DEPLOYED-SPEC snapshots, rendered as canonical
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
// a correctness guard (both versions must belong to {name}); symmetric from/to. The agent is read
// through the caller's own client first (an honest 404/403, and the identity its history is keyed
// by); each version read is authorized by a caller-scoped SSAR.
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
	if s.agentVersionStore == nil {
		writeError(w, http.StatusNotImplemented, msgVersionStoreRequired)
		return
	}

	var ad agentsv1beta1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	fromYAML, ok := s.versionSnapshotYAML(w, r, caller, &ad, fromName, "from")
	if !ok {
		return
	}
	toYAML, ok := s.versionSnapshotYAML(w, r, caller, &ad, toName, "to")
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

// versionSnapshotYAML authorizes and reads one version of the agent, and marshals its snapshot to
// canonical YAML (sigs.k8s.io/yaml — honors the json tags + omitempty, deterministic key order). On
// any failure it writes the error and returns ("", false): a caller without `get agentversions` →
// 403, a missing version → 404 (labeled from/to), a version of another agent → 400, a store or
// marshal failure → 500.
func (s *Server) versionSnapshotYAML(
	w http.ResponseWriter, r *http.Request, caller client.Client,
	ad *agentsv1beta1.AgentDeployment, versionName, label string,
) (string, bool) {
	ctx := r.Context()
	if err := s.authorizeStore(ctx, caller, authz.VerbGet, resourceAgentVersions, ad.Namespace, versionName); err != nil {
		s.writeAuthzError(w, err, "read the "+label+" version")
		return "", false
	}
	v, found, err := s.agentVersionStore.Get(ctx, ad.Namespace, string(ad.UID), versionName)
	if err != nil {
		s.log.Error(err, "agent version diff: read failed", "namespace", ad.Namespace, "version", versionName)
		writeError(w, http.StatusInternalServerError, "failed to read the "+label+" version")
		return "", false
	}
	if !found {
		owners, err := s.agentVersionStore.Owners(ctx, ad.Namespace, versionName)
		if err != nil {
			s.log.Error(err, "agent version diff: read failed", "namespace", ad.Namespace, "version", versionName)
			writeError(w, http.StatusInternalServerError, "failed to read the "+label+" version")
			return "", false
		}
		// Path-anchored correctness guard: a diff across DIFFERENT agents is meaningless.
		for _, o := range owners {
			if o.Agent != ad.Name {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("version %q does not belong to agent %q", versionName, ad.Name))
				return "", false
			}
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf("the %s version not found", label))
		return "", false
	}
	raw, err := yaml.Marshal(v.Snapshot)
	if err != nil {
		s.log.Error(err, "agent version diff: marshal snapshot failed", "version", versionName)
		writeError(w, http.StatusInternalServerError, "failed to render the "+label+" version")
		return "", false
	}
	return string(raw), true
}

// agentVersionNames returns the agent's version names, newest first, for the detail page. A caller
// without `list agentversions` in the namespace gets an empty list (the detail stays readable, as for
// bindings), as does an install without the store; any other failure writes a 500 and returns false.
func (s *Server) agentVersionNames(
	w http.ResponseWriter, r *http.Request, caller client.Client, ad *agentsv1beta1.AgentDeployment,
) ([]string, bool) {
	names := []string{}
	if s.agentVersionStore == nil {
		return names, true
	}
	ctx := r.Context()
	if err := s.authorizeStore(ctx, caller, authz.VerbList, resourceAgentVersions, ad.Namespace, ""); err != nil {
		if errors.Is(err, authz.ErrForbidden) {
			return names, true
		}
		s.writeAuthzError(w, err, "list agent versions")
		return nil, false
	}
	entries, err := s.agentVersionStore.List(ctx, ad.Namespace, string(ad.UID))
	if err != nil {
		s.log.Error(err, "list agent versions for detail failed", "namespace", ad.Namespace, "agent", ad.Name)
		writeError(w, http.StatusInternalServerError, "failed to read agent versions")
		return nil, false
	}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, true
}
