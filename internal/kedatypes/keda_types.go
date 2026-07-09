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

// Package kedatypes contains minimal Go types for the KEDA ScaledObject API
// (keda.sh/v1alpha1). We define only the fields consumed by the
// AgentScalingPolicy controller (m7.4) rather than importing the full
// github.com/kedacore/keda/v2/apis/keda/v1alpha1 package, which uses
// controller-runtime v0.19 webhook APIs incompatible with our v0.24
// (ctrl.NewWebhookManagedBy signature change, webhook.Validator removal).
//
// These types are wire-compatible with the KEDA v2.16.1 CRD
// (keda.sh/v1alpha1/scaledobjects) installed in the cluster. The schema is
// pinned to the fields actually used; unknown fields round-trip via
// controller-runtime's unstructured path.
//
// Source of truth for the field layout:
// https://github.com/kedacore/keda/blob/v2.16.1/apis/keda/v1alpha1/scaledobject_types.go
package kedatypes

import (
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the KEDA API group and version.
var GroupVersion = schema.GroupVersion{Group: "keda.sh", Version: "v1alpha1"}

// SchemeBuilder registers the KEDA types.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the KEDA types to the scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(
		GroupVersion,
		&ScaledObject{},
		&ScaledObjectList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// ScaledObject is the KEDA ScaledObject resource (keda.sh/v1alpha1).
// Only the fields consumed by the AgentScalingPolicy controller are declared;
// additional fields are preserved by controller-runtime's unstructured merge.
// NOTE: no +kubebuilder:object:root marker here — we do not generate the CRD
// from this package; the CRD comes from the KEDA release (keda-crds.yaml).
type ScaledObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ScaledObjectSpec   `json:"spec"`
	Status            ScaledObjectStatus `json:"status,omitempty"`
}

// DeepCopyObject implements runtime.Object.
func (s *ScaledObject) DeepCopyObject() runtime.Object {
	out := s.DeepCopy()
	return out
}

// DeepCopy returns a deep copy of the ScaledObject.
func (s *ScaledObject) DeepCopy() *ScaledObject {
	if s == nil {
		return nil
	}
	out := new(ScaledObject)
	s.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all fields into out.
func (s *ScaledObject) DeepCopyInto(out *ScaledObject) {
	*out = *s
	out.TypeMeta = s.TypeMeta
	s.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	s.Spec.DeepCopyInto(&out.Spec)
	out.Status = s.Status
}

// ScaledObjectList is a list of ScaledObject resources.
// NOTE: no +kubebuilder:object:root marker here — CRD is from KEDA release.
type ScaledObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScaledObject `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (s *ScaledObjectList) DeepCopyObject() runtime.Object {
	out := s.DeepCopy()
	return out
}

// DeepCopy returns a deep copy.
func (s *ScaledObjectList) DeepCopy() *ScaledObjectList {
	if s == nil {
		return nil
	}
	out := new(ScaledObjectList)
	s.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all fields into out.
func (s *ScaledObjectList) DeepCopyInto(out *ScaledObjectList) {
	*out = *s
	out.TypeMeta = s.TypeMeta
	s.ListMeta.DeepCopyInto(&out.ListMeta)
	if s.Items != nil {
		in, out2 := &s.Items, &out.Items
		*out2 = make([]ScaledObject, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out2)[i])
		}
	}
}

// ScaledObjectSpec is the spec for a ScaledObject.
type ScaledObjectSpec struct {
	// scaleTargetRef identifies the target workload to scale.
	ScaleTargetRef *ScaleTarget `json:"scaleTargetRef"`
	// minReplicaCount is the minimum replica count (0 enables scale-to-zero).
	// +optional
	MinReplicaCount *int32 `json:"minReplicaCount,omitempty"`
	// maxReplicaCount is the maximum replica count.
	// +optional
	MaxReplicaCount *int32 `json:"maxReplicaCount,omitempty"`
	// cooldownPeriod is the cooldown in seconds after a scale event.
	// +optional
	CooldownPeriod *int32 `json:"cooldownPeriod,omitempty"`
	// triggers is the list of scalers.
	Triggers []ScaleTriggers `json:"triggers"`
}

// DeepCopyInto copies all fields into out.
func (s *ScaledObjectSpec) DeepCopyInto(out *ScaledObjectSpec) {
	*out = *s
	if s.ScaleTargetRef != nil {
		in, out2 := &s.ScaleTargetRef, &out.ScaleTargetRef
		*out2 = new(ScaleTarget)
		**out2 = **in
	}
	if s.MinReplicaCount != nil {
		v := *s.MinReplicaCount
		out.MinReplicaCount = &v
	}
	if s.MaxReplicaCount != nil {
		v := *s.MaxReplicaCount
		out.MaxReplicaCount = &v
	}
	if s.CooldownPeriod != nil {
		v := *s.CooldownPeriod
		out.CooldownPeriod = &v
	}
	if s.Triggers != nil {
		in, out2 := &s.Triggers, &out.Triggers
		*out2 = make([]ScaleTriggers, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out2)[i])
		}
	}
}

// ScaleTarget identifies the target workload to scale.
type ScaleTarget struct {
	// name of the workload resource.
	Name string `json:"name"`
	// apiVersion of the workload resource. Defaults to "apps/v1".
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// kind of the workload resource. Defaults to "Deployment".
	// +optional
	Kind string `json:"kind,omitempty"`
}

// ScaleTriggers references a KEDA scaler.
type ScaleTriggers struct {
	// type is the scaler type (e.g. "metrics-api", "prometheus").
	Type string `json:"type"`
	// name is an optional unique name for this trigger.
	// +optional
	Name string `json:"name,omitempty"`
	// metadata is the scaler-specific configuration.
	Metadata map[string]string `json:"metadata"`
}

// DeepCopyInto copies all fields into out.
func (s *ScaleTriggers) DeepCopyInto(out *ScaleTriggers) {
	*out = *s
	if s.Metadata != nil {
		out.Metadata = make(map[string]string, len(s.Metadata))
		maps.Copy(out.Metadata, s.Metadata)
	}
}

// ScaledObjectStatus is the observed state of a ScaledObject.
type ScaledObjectStatus struct {
	// scaleTargetKind is the Kind of the scale target.
	// +optional
	ScaleTargetKind string `json:"scaleTargetKind,omitempty"`
}
