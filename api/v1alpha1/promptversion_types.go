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

// GitPromptSource locates a prompt artifact in a git repository.
// The ref must be an immutable pin (a tag or full SHA, not a branch name)
// so that a resolved prompt is fully reproducible.
type GitPromptSource struct {
	// repo is the URL of the git repository containing the prompt, e.g.
	// "https://github.com/example/prompts.git". Required.
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// ref is the immutable git ref that pins the prompt version — a full commit
	// SHA or an annotated tag. Branch names are discouraged because they are
	// mutable; the controller resolves ref → content at reconcile time and the
	// result must be reproducible. Required.
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`

	// path is the path within the repository to the prompt file, e.g.
	// "prompts/my-agent/system.txt". Required.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
}

// PromptVersionSpec defines the desired state of a PromptVersion.
type PromptVersionSpec struct {
	// git points to the prompt artifact in a git repository.
	// The platform never becomes the prompt store — git remains the source of
	// truth (ADR 0008, PRD §7).
	// +required
	Git GitPromptSource `json:"git"`
}

// PromptVersionStatus defines the observed state of a PromptVersion.
type PromptVersionStatus struct {
	// conditions reflect the current reconciliation state of the PromptVersion.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pv
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.git.repo"
// +kubebuilder:printcolumn:name="Ref",type="string",JSONPath=".spec.git.ref"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PromptVersion is the Schema for the promptversions API.
// A PromptVersion is a git-backed pointer to a prompt artifact. An
// AgentDeployment references it via spec.promptRef; swapping promptRef (or its
// ref) changes the active prompt without an image rebuild — the container image
// digest is unchanged but a new Knative revision is rolled out carrying the new
// prompt material. Absent promptRef means the image-bundled prompt is used
// (unchanged deploy). (PRD §7, ADR 0008).
type PromptVersion struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of the PromptVersion.
	// +required
	Spec PromptVersionSpec `json:"spec"`

	// status defines the observed state of the PromptVersion.
	// +optional
	Status PromptVersionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PromptVersionList contains a list of PromptVersion.
type PromptVersionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PromptVersion `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PromptVersion{}, &PromptVersionList{})
		return nil
	})
}
