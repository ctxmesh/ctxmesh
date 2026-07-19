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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// AgentVersionSpec is an immutable snapshot of an AgentDeploymentSpec captured
// at a specific point in time. Created by the controller whenever the
// AgentDeployment spec hash changes; named "<deployment>-<spec-hash-8>".
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AgentVersion spec is immutable"
type AgentVersionSpec struct {
	// deploymentName is the name of the parent AgentDeployment this version
	// was snapshotted from.
	// +kubebuilder:validation:MinLength=1
	DeploymentName string `json:"deploymentName"`

	// snapshot is a verbatim copy of the AgentDeploymentSpec at the time this
	// AgentVersion was created. It is used by the controller to drive the
	// Knative Service template and is never modified after creation.
	Snapshot AgentDeploymentSpec `json:"snapshot"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion

// AgentVersion is the Schema for the agentversions API.
// Each AgentVersion is an immutable, controller-created snapshot of an
// AgentDeployment's spec at a specific point in time. Immutability is enforced
// by a CRD-level CEL rule — no webhook is required. AgentVersions are
// owner-referenced to their parent AgentDeployment and garbage collected
// automatically when the deployment is deleted.
type AgentVersion struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the immutable snapshot of the AgentDeployment spec.
	// +required
	Spec AgentVersionSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// AgentVersionList contains a list of AgentVersion.
type AgentVersionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentVersion `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentVersion{}, &AgentVersionList{})
		return nil
	})
}
