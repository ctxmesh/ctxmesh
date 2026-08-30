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

// ── GET /api/workflows/{ns}/{name} — the declared workflow DAG (M144-canvas, ADR 0115) ──
//
// Projects a Workflow CR's spec.steps into a node/edge graph for the console's
// shared delegation canvas: each step is a node (task | choice | map | loop), and
// its control-flow fields become LABELED edges — next, each conditional branch
// (labelled by its CEL predicate), the default fallthrough, onError, each ordered
// catch (labelled by its error classes), and the map fan-out/join / loop bodies.
// The first step is the start (a determinable start is a validation rule, ADR 0060).
// Caller-scoped (ADR 0011); read-only — this is the declared structure, not a run.

import (
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// WorkflowEdge is one labeled control-flow edge from a step to another step.
// Kind ∈ next | branch | default | catch | map | join | loop.
type WorkflowEdge struct {
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
}

// WorkflowNode is one step drawn on the canvas. Kind ∈ task | choice | map | loop.
type WorkflowNode struct {
	Name     string         `json:"name"`
	AgentRef string         `json:"agentRef"`
	Kind     string         `json:"kind"`
	Start    bool           `json:"start,omitempty"`
	Edges    []WorkflowEdge `json:"edges"`
}

// WorkflowDetailResponse is the declared workflow graph + its validation status.
type WorkflowDetailResponse struct {
	Name        string         `json:"name"`
	Namespace   string         `json:"namespace"`
	RegistryRef string         `json:"registryRef"`
	Validated   bool           `json:"validated"`
	Reason      string         `json:"reason,omitempty"`
	Nodes       []WorkflowNode `json:"nodes"`
}

// stepKind classifies a step by which control-flow construct it carries.
func stepKind(st *agentsv1beta1.WorkflowStep) string {
	switch {
	case st.Map != nil:
		return "map"
	case st.Loop != nil:
		return "loop"
	case len(st.Branches) > 0:
		return "choice"
	default:
		return "task"
	}
}

// truncateExpr keeps a CEL predicate short enough to be an edge label.
func truncateExpr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

// projectWorkflowDetail turns a Workflow CR into the canvas node/edge graph.
func projectWorkflowDetail(wf *agentsv1beta1.Workflow) WorkflowDetailResponse {
	validated, reason := workflowValidated(wf)
	nodes := make([]WorkflowNode, 0, len(wf.Spec.Steps))
	for i := range wf.Spec.Steps {
		st := &wf.Spec.Steps[i]
		edges := []WorkflowEdge{}
		if st.Next != "" {
			edges = append(edges, WorkflowEdge{To: st.Next, Kind: "next"})
		}
		for _, b := range st.Branches {
			edges = append(edges, WorkflowEdge{To: b.To, Kind: "branch", Label: truncateExpr(b.When)})
		}
		if st.Default != "" {
			edges = append(edges, WorkflowEdge{To: st.Default, Kind: "default", Label: "default"})
		}
		if st.OnError != "" {
			edges = append(edges, WorkflowEdge{To: st.OnError, Kind: "catch", Label: "on error"})
		}
		for _, c := range st.Catch {
			edges = append(edges, WorkflowEdge{To: c.Next, Kind: "catch", Label: "catch " + strings.Join(c.Errors, ",")})
		}
		if st.Map != nil {
			edges = append(edges, WorkflowEdge{To: st.Map.Do, Kind: "map", Label: "for each"})
			if st.Map.Join != "" {
				edges = append(edges, WorkflowEdge{To: st.Map.Join, Kind: "join", Label: "join"})
			}
		}
		if st.Loop != nil {
			edges = append(edges, WorkflowEdge{To: st.Loop.Do, Kind: "loop", Label: "loop"})
		}
		nodes = append(nodes, WorkflowNode{
			Name:     st.Name,
			AgentRef: st.AgentRef,
			Kind:     stepKind(st),
			Start:    i == 0,
			Edges:    edges,
		})
	}
	return WorkflowDetailResponse{
		Name:        wf.Name,
		Namespace:   wf.Namespace,
		RegistryRef: wf.Spec.RegistryRef,
		Validated:   validated,
		Reason:      reason,
		Nodes:       nodes,
	}
}

// handleGetWorkflow serves GET /api/workflows/{ns}/{name} — the declared DAG,
// caller-scoped (ADR 0011). A 403/404 is surfaced honestly via writeGetError.
func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
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
	var wf agentsv1beta1.Workflow
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &wf); err != nil {
		s.writeGetError(w, err, "workflow")
		return
	}
	writeJSON(w, http.StatusOK, projectWorkflowDetail(&wf))
}
