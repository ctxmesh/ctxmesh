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

package audit

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// fixedTime is a deterministic clock for the entry-shape assertions.
var fixedTime = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// captureSink records every entry it receives for later assertion.
type captureSink struct {
	entries []AuditEntry
}

func (c *captureSink) Record(entry AuditEntry) { c.entries = append(c.entries, entry) }

// newTestAuditor returns an Auditor whose clock and sink are test-controlled;
// the cache is nil because these tests exercise emit() directly, not Start().
func newTestAuditor(sink Sink) *Auditor {
	return &Auditor{sink: sink, now: func() time.Time { return fixedTime }}
}

func TestEmit_EntryShape_CreateUpdateDelete(t *testing.T) {
	tests := []struct {
		name string
		verb Verb
	}{
		{name: "create", verb: VerbCreate},
		{name: "update", verb: VerbUpdate},
		{name: "delete", verb: VerbDelete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			a := newTestAuditor(sink)

			obj := &agentsv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "checkout-agent",
					Namespace: "team-a",
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: "kubectl", Time: &metav1.Time{Time: fixedTime}},
					},
				},
			}

			a.emit(tc.verb, "AgentDeployment", obj)

			if len(sink.entries) != 1 {
				t.Fatalf("expected 1 audit entry, got %d", len(sink.entries))
			}
			got := sink.entries[0]
			if got.Verb != tc.verb {
				t.Errorf("verb = %q, want %q", got.Verb, tc.verb)
			}
			if got.Kind != "AgentDeployment" {
				t.Errorf("kind = %q, want AgentDeployment", got.Kind)
			}
			if got.Name != "checkout-agent" {
				t.Errorf("name = %q, want checkout-agent", got.Name)
			}
			if got.Namespace != "team-a" {
				t.Errorf("namespace = %q, want team-a", got.Namespace)
			}
			if got.Subject != "kubectl" {
				t.Errorf("subject = %q, want kubectl", got.Subject)
			}
			if !got.Timestamp.Equal(fixedTime) {
				t.Errorf("timestamp = %v, want %v", got.Timestamp, fixedTime)
			}
		})
	}
}

func TestEmit_NonObject_Skipped(t *testing.T) {
	sink := &captureSink{}
	a := newTestAuditor(sink)

	a.emit(VerbCreate, "AgentDeployment", "not-a-k8s-object")

	if len(sink.entries) != 0 {
		t.Fatalf("expected non-object to be skipped, got %d entries", len(sink.entries))
	}
}

func TestSubjectFromObject(t *testing.T) {
	older := metav1.Time{Time: fixedTime.Add(-time.Hour)}
	newer := metav1.Time{Time: fixedTime}

	tests := []struct {
		name string
		obj  metav1.Object
		want string
	}{
		{
			name: "no managed fields",
			obj:  &agentsv1alpha1.AgentDeployment{},
			want: unknownSubject,
		},
		{
			name: "single field manager",
			obj: &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "argocd", Time: &newer}},
			}},
			want: "argocd",
		},
		{
			name: "most recent field manager wins",
			obj: &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
				ManagedFields: []metav1.ManagedFieldsEntry{
					{Manager: "old-manager", Time: &older},
					{Manager: "recent-manager", Time: &newer},
				},
			}},
			want: "recent-manager",
		},
		{
			name: "empty manager falls back to unknown",
			obj: &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "", Time: &newer}},
			}},
			want: unknownSubject,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := subjectFromObject(tc.obj); got != tc.want {
				t.Errorf("subjectFromObject = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSameResourceVersion(t *testing.T) {
	a := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}}
	b := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}}
	c := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "101"}}

	if !sameResourceVersion(a, b) {
		t.Error("equal resourceVersions should report same (resync)")
	}
	if sameResourceVersion(a, c) {
		t.Error("differing resourceVersions should report changed (real update)")
	}
}

func TestAuditedTypes_CoversEveryAgentCRD(t *testing.T) {
	// Guard against an audit gap: every agent CRD must be audited. This mirrors
	// the platform-persona RBAC roles — the two sets must not drift. Kinds are
	// resolved from the scheme, the same way the Auditor does at runtime.
	testScheme := k8sruntime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("adding agents scheme: %v", err)
	}

	// PromptVersion and ToolRegistry retired to Postgres (ADR 0044); MemoryBinding
	// folded into AgentDeployment.spec.sessionMemory + retired (ADR 0101) — no longer
	// CRDs, so not audited.
	wantKinds := map[string]bool{
		"AgentDeployment": true, "AgentVersion": true, "ModelRoute": true,
		"SecretBinding": true, "MCPToolBinding": true,
		"AgentRegistry": true, "AgentScalingPolicy": true,
		"EvalSuite": true,
	}
	got := map[string]bool{}
	for _, obj := range auditedTypes() {
		if obj == nil {
			t.Fatal("audited type list contains a nil object")
		}
		gvk, err := apiutil.GVKForObject(obj, testScheme)
		if err != nil {
			t.Errorf("resolving GVK for %T: %v", obj, err)
			continue
		}
		got[gvk.Kind] = true
	}
	for k := range wantKinds {
		if !got[k] {
			t.Errorf("agent CRD %q is not audited (audit gap)", k)
		}
	}
	if len(got) != len(wantKinds) {
		t.Errorf("audited kinds = %d, want %d: %v", len(got), len(wantKinds), got)
	}
}
