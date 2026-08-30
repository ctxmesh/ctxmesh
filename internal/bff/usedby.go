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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// UsedByRef is one resource that references the queried object.
type UsedByRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// UsedByResponse is returned by GET /api/usedby — the resources referencing the
// queried object. Items is non-nil on the wire ([] not null).
type UsedByResponse struct {
	Items []UsedByRef `json:"items"`
}

// handleUsedBy serves GET /api/usedby?kind=&namespace=&name= — the reverse-lookup
// powering the console's "Used by" sections (m18.9). All reads are CALLER-SCOPED
// (ADR 0011); a Forbidden surfaces honestly, never a swallowed empty list.
//
// Supported kinds:
//   - modelroute / promptversion → the AgentDeployments that reference it
//     (the MODEL_ROUTE env / spec.promptRef).
//   - secretbinding → the ModelRoutes whose providers reference it.
func (s *Server) handleUsedBy(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultCreateNamespace
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	items := make([]UsedByRef, 0)
	switch kind {
	case "modelroute", "promptversion":
		var agents agentsv1alpha1.AgentDeploymentList
		if err := caller.List(r.Context(), &agents, client.InNamespace(ns)); err != nil {
			if status, msg, isRBAC := classifyReadError(err); isRBAC {
				writeError(w, status, msg)
				return
			}
			s.log.Error(err, "used-by: list agents failed")
			writeError(w, http.StatusInternalServerError, "failed to list agents")
			return
		}
		for i := range agents.Items {
			ad := &agents.Items[i]
			match := (kind == "promptversion" && ad.Spec.PromptRef == name) ||
				(kind == "modelroute" && agentModelRoute(ad) == name)
			if match {
				items = append(items, UsedByRef{Kind: "AgentDeployment", Name: ad.Name, Namespace: ad.Namespace})
			}
		}
	case "secretbinding":
		var routes agentsv1alpha1.ModelRouteList
		if err := caller.List(r.Context(), &routes, client.InNamespace(ns)); err != nil {
			if status, msg, isRBAC := classifyReadError(err); isRBAC {
				writeError(w, status, msg)
				return
			}
			s.log.Error(err, "used-by: list model routes failed")
			writeError(w, http.StatusInternalServerError, "failed to list model routes")
			return
		}
		for i := range routes.Items {
			mr := &routes.Items[i]
			for _, p := range mr.Spec.Providers {
				if p.SecretBindingRef == name {
					items = append(items, UsedByRef{Kind: "ModelRoute", Name: mr.Name, Namespace: mr.Namespace})
					break
				}
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "kind must be one of: modelroute, promptversion, secretbinding")
		return
	}

	writeJSON(w, http.StatusOK, UsedByResponse{Items: items})
}

// agentModelRoute reads the agent's MODEL_ROUTE env (expand: model.route → env).
func agentModelRoute(ad *agentsv1alpha1.AgentDeployment) string {
	for _, e := range ad.Spec.Env {
		if e.Name == envModelRoute {
			return e.Value
		}
	}
	return ""
}
