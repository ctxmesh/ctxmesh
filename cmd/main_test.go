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

package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// TestSchemeRegistersManagerKinds guards the M64 manager-scheme gap: a controller cannot be constructed
// for a kind the manager scheme doesn't recognize (the manager crash-loops on "no kind is registered").
// The controller ENVTEST builds its own scheme, so a missing AddToScheme in main's init() slips past it —
// only a live deploy (or this test) catches it. AgentTeam is v1beta1-only, so v1beta1 MUST be registered.
func TestSchemeRegistersManagerKinds(t *testing.T) {
	for _, gvk := range []schema.GroupVersionKind{
		agentsv1beta1.GroupVersion.WithKind("AgentTeam"),
		agentsv1alpha1.GroupVersion.WithKind("AgentDeployment"),
		agentsv1alpha1.GroupVersion.WithKind("AgentRegistry"),
	} {
		if !scheme.Recognizes(gvk) {
			t.Errorf("manager scheme does not recognize %s — its controller will fail to construct", gvk)
		}
	}
}
