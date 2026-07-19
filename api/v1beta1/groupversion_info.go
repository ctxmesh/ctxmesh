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

// Package v1beta1 is the graduated API version of the agents.ctxmesh.ai group (ADR 0037, M34). It
// is served alongside v1alpha1 during the deprecation window; v1alpha1 is the conversion HUB (this
// version is a spoke). No fields change at graduation — the only schema evolution (folding
// MemoryBinding into AgentDeployment.spec.sessionMemory, m34.2) is already a field present in both
// versions — so each root type REUSES the v1alpha1 spec/status types and conversion is a direct
// assignment (no duplication, no import cycle: v1beta1 imports v1alpha1, never the reverse).
// +kubebuilder:object:generate=true
// +groupName=agents.ctxmesh.ai
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: "agents.ctxmesh.ai", Version: "v1beta1"}

	// GroupVersion is an alias for SchemeGroupVersion.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder registers the v1beta1 types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds the v1beta1 types to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
