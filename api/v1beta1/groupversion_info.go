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

// Package v1beta1 contains the API Schema definitions for the agents.ctxmesh.ai group. It is the
// only served version and the storage version of every kind (ADR 0100).
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
	SchemeGroupVersion = schema.GroupVersion{Group: "agents.ctxmesh.ai", Version: "v1beta1"} //nolint:goconst // "v1beta1" is an API version string; extracting to a const adds no clarity

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
