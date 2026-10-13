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

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

const (
	// asyncDeliveryPolicySuffix names the registry's second NetworkPolicy, which lets the JetStream
	// dispatcher reach the registry's eventing agents.
	asyncDeliveryPolicySuffix = "-async-delivery"

	// bffPodLabelKey / bffPodLabelValue select the BFF's pods, which run the JetStream dispatcher. Every
	// platform component carries a control-plane label and no agent pod does.
	bffPodLabelKey   = "control-plane"
	bffPodLabelValue = "bff"

	// defaultAgentPort is the port an agent listens on when spec.port is unset (the CRD default).
	defaultAgentPort int32 = 8080
)

// jetStream reports whether the BFF runs the JetStream backend, and so the dispatcher that delivers hops
// to agents over HTTP.
func (b AsyncBackend) jetStream() bool {
	return b.Kind == asyncBackendJetStream && b.NATSURL != ""
}

// reconcileAsyncDeliveryPolicy admits the JetStream dispatcher to the registry's eventing agents.
//
// The dispatcher runs in the BFF, in the platform namespace, and POSTs each hop to the agent's
// <agent>-eventing Service. The registry's policy admits a member's ingress only from its own registry and
// the Knative data plane, which carries hops on the Knative backend, so on JetStream nothing would reach
// an eventing agent. This policy adds one ingress rule and nothing else: from the BFF's pods, selected by
// the platform namespace and the BFF's own label together, to the eventing members of this registry, on
// the ports their agents listen on. Policies add up, so the members' other rules are untouched. Serving
// agents receive through the Knative route, which the registry policy already admits, and jobs receive
// nothing. Without the JetStream backend, or with no eventing member, the policy is deleted.
//
// One policy covers every eventing member of the registry, so its ports are the union of theirs: an
// agent can be reached on another eventing member's port, where nothing of its own listens.
func (r *AgentRegistryReconciler) reconcileAsyncDeliveryPolicy(
	ctx context.Context,
	registry *agentsv1beta1.AgentRegistry,
	members []agentsv1beta1.AgentDeployment,
) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registry.Name + asyncDeliveryPolicySuffix,
			Namespace: registry.Namespace,
		},
	}
	ports := eventingDeliveryPorts(members)
	if !r.AsyncBackend.jetStream() || len(ports) == 0 {
		return r.deleteOwnedPolicy(ctx, registry, np)
	}

	npPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		npPorts = append(npPorts, networkingv1.NetworkPolicyPort{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(int(p))})
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{registryIDLabel: registry.Spec.RegistryId},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: eventingWorkloadLabel, Operator: metav1.LabelSelectorOpExists},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					// Both selectors in one peer: a BFF pod in the platform namespace, never any pod there.
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{namespaceNameLabel: newPlatformServices(r.PlatformNamespace).namespace},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{bffPodLabelKey: bffPodLabelValue},
					},
				}},
				Ports: npPorts,
			}},
		}
		return ctrl.SetControllerReference(registry, np, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting NetworkPolicy %s: %w", np.Name, err)
	}
	return nil
}

// deleteOwnedPolicy deletes np if it exists and the registry controls it.
func (r *AgentRegistryReconciler) deleteOwnedPolicy(
	ctx context.Context,
	registry *agentsv1beta1.AgentRegistry,
	np *networkingv1.NetworkPolicy,
) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading NetworkPolicy %s: %w", np.Name, err)
	}
	if !metav1.IsControlledBy(np, registry) {
		return nil
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting NetworkPolicy %s: %w", np.Name, err)
	}
	return nil
}

// eventingDeliveryPorts are the distinct ports the eventing members listen on, sorted.
func eventingDeliveryPorts(members []agentsv1beta1.AgentDeployment) []int32 {
	var ports []int32
	for i := range members {
		if members[i].Spec.ExecutionModel != execModelEventing {
			continue
		}
		port := members[i].Spec.Port
		if port == 0 {
			port = defaultAgentPort
		}
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return ports
}
