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

package controller

// Routes hot-reload delivery (J7) — the controller side of the M81-K3 mounted-ConfigMap + fsnotify
// pattern, mirroring reconcileToolPolicyConfigMap. The egress sidecar's remote-tool URL table was
// delivered as a STATIC EGRESS_ROUTES env; a URL edit is excluded from the pod-template digest (so it
// never rolls the revision), which means — with the routes in the pod spec — the edit is never
// re-applied to the running sidecar either (the CreateOrUpdate guard skips a same-name revision). So a
// remote-URL edit silently did not take effect until something else rolled the pod.
//
// The fix: deliver the routes as a per-agent, STABLE-named <agent>-egress-routes ConfigMap mounted
// read-only on the sidecar, with the static EGRESS_ROUTES_FILE path env. The ConfigMap NAME (the only
// routes reference in the pod spec) is constant, so a URL edit updates the ConfigMap CONTENT — not the
// pod template — and the sidecar's fsnotify watch reloads it live: no revision roll, and the edit
// actually takes effect. The sidecar still accepts the legacy static EGRESS_ROUTES env (dev/tests).

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

const (
	// envEgressRoutesFile is the STATIC env var carrying the in-container PATH to the mounted routes
	// file on the egress sidecar (the source the sidecar reads + fsnotify-watches). NEVER valueFrom
	// (the m5.7 Knative ksvc landmine) — the VALUE is a static path.
	envEgressRoutesFile = "EGRESS_ROUTES_FILE"

	// egressRoutesConfigMapSuffix names the per-agent, STABLE-named ConfigMap that materialises the
	// route table JSON (<agent>-egress-routes). STABLE (not content-addressed) is the point of the K3
	// pattern: a remote-URL edit UPDATES this same ConfigMap IN PLACE, so the mounted file changes and
	// the sidecar reloads (fsnotify) WITHOUT a revision roll.
	egressRoutesConfigMapSuffix = "-egress-routes"

	// egressRoutesConfigMapKey is the data key inside the <agent>-egress-routes ConfigMap.
	egressRoutesConfigMapKey = "routes.json"

	// egressRoutesMountPath is where the routes file is mounted in the EGRESS SIDECAR container. The
	// sidecar reads EGRESS_ROUTES_FILE (this path + key) and watches the directory.
	egressRoutesMountPath = "/etc/egress/routes"

	// egressRoutesVolumeName is the pod volume name for the mounted routes ConfigMap.
	egressRoutesVolumeName = "egress-routes"
)

// egressRoutesConfigMapName returns the STABLE per-agent routes ConfigMap name (<agent>-egress-routes).
func egressRoutesConfigMapName(agentName string) string {
	return agentName + egressRoutesConfigMapSuffix
}

// reconcileEgressRoutesConfigMap materialises the sidecar route table JSON into the per-agent,
// STABLE-named <agent>-egress-routes ConfigMap (owner-ref'd so it GCs with the AgentDeployment,
// mirroring reconcileToolPolicyConfigMap) and returns the volume + mount + static EGRESS_ROUTES_FILE
// env the sidecar needs (J7). The routes are delivered as a WATCHED, mounted file (not the static
// EGRESS_ROUTES env) so a remote-tool-URL edit takes effect on the running sidecar without a roll.
//
// SECURITY: the controller OWNS + reconciles this ConfigMap (owner-ref + CreateOrUpdate reverts drift),
// so a namespace-level edit cannot silently repoint a tool's real upstream out from under the CR — the
// same posture as the tool-policy ConfigMap (ADR 0074 §6a). routesJSON is the SAME table the sidecar
// used to get via EGRESS_ROUTES.
func (r *AgentDeploymentReconciler) reconcileEgressRoutesConfigMap(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	routesJSON string,
) (vol *corev1.Volume, mount *corev1.VolumeMount, env []corev1.EnvVar, err error) {
	cmName := egressRoutesConfigMapName(deploy.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err = ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[egressRoutesConfigMapKey] = routesJSON
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("upserting egress-routes ConfigMap: %w", err)
	}

	v := corev1.Volume{
		Name: egressRoutesVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			},
		},
	}
	m := corev1.VolumeMount{Name: egressRoutesVolumeName, MountPath: egressRoutesMountPath, ReadOnly: true}
	e := []corev1.EnvVar{
		{Name: envEgressRoutesFile, Value: egressRoutesMountPath + "/" + egressRoutesConfigMapKey},
	}
	return &v, &m, e, nil
}
