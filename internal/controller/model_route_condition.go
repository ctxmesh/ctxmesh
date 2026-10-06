package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// conditionModelRouteReady says whether the model route an agent names in MODEL_ROUTE can be called
// now (ADR 0151). A side condition like KnowledgeReady, never folded into Ready: an edited route is
// still served by the old gateway pods, and an agent must not go NotReady while its calls work.
const conditionModelRouteReady = "ModelRouteReady"

const envModelRoute = "MODEL_ROUTE"

// modelRouteName is the route an agent names, or "" when it names none (or names it indirectly).
func modelRouteName(deploy *agentsv1alpha1.AgentDeployment) string {
	for _, e := range deploy.Spec.Env {
		if e.Name == envModelRoute && e.ValueFrom == nil {
			return strings.TrimSpace(e.Value)
		}
	}
	return ""
}

// syncModelRouteCondition mirrors the named route's Ready onto the agent. The route is looked up in the
// agent's own namespace; a route of that name elsewhere is not one this agent may rely on.
func (r *AgentDeploymentReconciler) syncModelRouteCondition(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment) error {
	route := modelRouteName(deploy)
	if route == "" {
		if apimeta.RemoveStatusCondition(&deploy.Status.Conditions, conditionModelRouteReady) {
			if err := r.Status().Update(ctx, deploy); err != nil {
				return fmt.Errorf("clearing %s: %w", conditionModelRouteReady, err)
			}
		}
		return nil
	}
	cond := metav1.Condition{Type: conditionModelRouteReady, ObservedGeneration: deploy.Generation}
	var mr agentsv1alpha1.ModelRoute
	err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: route}, &mr)
	switch {
	case apierrors.IsNotFound(err):
		cond.Status, cond.Reason = metav1.ConditionFalse, "RouteNotFound"
		cond.Message = fmt.Sprintf("MODEL_ROUTE names %q, and there is no ModelRoute of that name in %s", route, deploy.Namespace)
	case err != nil:
		return fmt.Errorf("getting ModelRoute %s/%s: %w", deploy.Namespace, route, err)
	default:
		if rc := apimeta.FindStatusCondition(mr.Status.Conditions, conditionReady); rc != nil {
			cond.Status, cond.Reason = rc.Status, rc.Reason
			cond.Message = fmt.Sprintf("ModelRoute %s: %s", route, rc.Message)
		} else {
			cond.Status, cond.Reason = metav1.ConditionFalse, reasonNotYetServed
			cond.Message = fmt.Sprintf("ModelRoute %s has not been reconciled yet", route)
		}
	}
	if apimeta.SetStatusCondition(&deploy.Status.Conditions, cond) {
		if err := r.Status().Update(ctx, deploy); err != nil {
			return fmt.Errorf("updating %s: %w", conditionModelRouteReady, err)
		}
	}
	return nil
}
