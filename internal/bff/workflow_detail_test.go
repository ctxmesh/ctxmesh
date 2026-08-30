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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

func TestProjectWorkflowDetail(t *testing.T) {
	wf := &agentsv1beta1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf1", Namespace: "team-a"},
		Spec: agentsv1beta1.WorkflowSpec{
			RegistryRef: "reg1",
			Steps: []agentsv1beta1.WorkflowStep{
				{
					Name:     "triage",
					AgentRef: "triage-agent",
					Branches: []agentsv1beta1.WorkflowBranch{
						{When: "steps.triage.output.urgent", To: "escalate"},
					},
					Default: "resolve",
				},
				{
					Name:     "escalate",
					AgentRef: "oncall-agent",
					Next:     "resolve",
					Catch: []agentsv1beta1.WorkflowCatch{
						{Errors: []string{"timeout", "agent_error"}, Next: "resolve"},
					},
				},
				{Name: "resolve", AgentRef: "resolver-agent"},
			},
		},
	}
	got := projectWorkflowDetail(wf)

	if got.Name != "wf1" || got.Namespace != "team-a" || got.RegistryRef != "reg1" {
		t.Fatalf("meta wrong: %+v", got)
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(got.Nodes))
	}

	// triage: the start; a choice with a labeled branch + a default edge.
	triage := got.Nodes[0]
	if !triage.Start {
		t.Error("triage should be the start step")
	}
	if triage.Kind != "choice" {
		t.Errorf("triage kind = %q, want choice", triage.Kind)
	}
	if len(triage.Edges) != 2 {
		t.Fatalf("triage edges = %d, want 2 (branch + default)", len(triage.Edges))
	}
	if triage.Edges[0].Kind != "branch" || triage.Edges[0].To != "escalate" ||
		triage.Edges[0].Label != "steps.triage.output.urgent" {
		t.Errorf("branch edge wrong: %+v", triage.Edges[0])
	}
	if triage.Edges[1].Kind != "default" || triage.Edges[1].To != "resolve" {
		t.Errorf("default edge wrong: %+v", triage.Edges[1])
	}

	// escalate: a task with a next edge + a labeled catch edge.
	esc := got.Nodes[1]
	if esc.Start {
		t.Error("escalate should not be the start")
	}
	if esc.Kind != "task" {
		t.Errorf("escalate kind = %q, want task", esc.Kind)
	}
	if len(esc.Edges) != 2 {
		t.Fatalf("escalate edges = %d, want 2 (next + catch)", len(esc.Edges))
	}
	if esc.Edges[0].Kind != "next" || esc.Edges[0].To != "resolve" {
		t.Errorf("next edge wrong: %+v", esc.Edges[0])
	}
	if esc.Edges[1].Kind != "catch" || esc.Edges[1].To != "resolve" ||
		esc.Edges[1].Label != "catch timeout,agent_error" {
		t.Errorf("catch edge wrong: %+v", esc.Edges[1])
	}

	// resolve: a terminal task — no outgoing edges.
	if len(got.Nodes[2].Edges) != 0 {
		t.Errorf("resolve should be terminal, got edges %+v", got.Nodes[2].Edges)
	}
}

func TestProjectWorkflowDetail_MapAndLoop(t *testing.T) {
	wf := &agentsv1beta1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf2", Namespace: "team-b"},
		Spec: agentsv1beta1.WorkflowSpec{
			RegistryRef: "reg2",
			Steps: []agentsv1beta1.WorkflowStep{
				{
					Name: "fanout",
					Map:  &agentsv1beta1.WorkflowMap{Over: "steps.x.output.items", As: "item", Do: "work", Join: "reduce"},
				},
				{Name: "work", AgentRef: "worker"},
				{Name: "reduce", AgentRef: "reducer"},
			},
		},
	}
	got := projectWorkflowDetail(wf)

	fan := got.Nodes[0]
	if fan.Kind != "map" {
		t.Errorf("fanout kind = %q, want map", fan.Kind)
	}
	if len(fan.Edges) != 2 {
		t.Fatalf("fanout edges = %d, want 2 (map + join)", len(fan.Edges))
	}
	if fan.Edges[0].Kind != "map" || fan.Edges[0].To != "work" {
		t.Errorf("map edge wrong: %+v", fan.Edges[0])
	}
	if fan.Edges[1].Kind != "join" || fan.Edges[1].To != "reduce" {
		t.Errorf("join edge wrong: %+v", fan.Edges[1])
	}
}
