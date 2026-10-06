package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
)

// The gateway serves a config only once no pod of an older template is left; each case below is a
// state `kubectl rollout status` would still be waiting in.
func TestGatewayServes(t *testing.T) {
	one := int32(1)
	zero := int32(0)
	done := func() *appsv1.Deployment {
		d := &appsv1.Deployment{}
		d.Generation = 4
		d.Spec.Replicas = &one
		d.Spec.Template.Annotations = map[string]string{configHashAnnotation: "h2"}
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
		return d
	}
	assert.True(t, gatewayServes(done(), "h2"))

	for name, mutate := range map[string]func(*appsv1.Deployment){
		"another config":            func(d *appsv1.Deployment) { d.Spec.Template.Annotations[configHashAnnotation] = "h1" },
		"spec not yet observed":     func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 3 },
		"new pod not up":            func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 },
		"old pod still running":     func(d *appsv1.Deployment) { d.Status.Replicas = 2 },
		"new pod not yet available": func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 0 },
		"scaled to zero": func(d *appsv1.Deployment) {
			d.Spec.Replicas = &zero
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 4}
		},
	} {
		d := done()
		mutate(d)
		assert.False(t, gatewayServes(d, "h2"), name)
	}
}
