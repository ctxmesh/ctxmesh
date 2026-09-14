package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

// EventingAvailable reports whether this cluster serves the Knative Eventing kinds the
// eventing execution model needs (ADR 0141).
//
// Only one of the three execution models — serving (the default), eventing, job — needs Knative
// Eventing, but the controller used to Own() Trigger and Broker unconditionally. controller-runtime
// starts an informer per owned kind, so on a cluster without those CRDs the manager could not start
// AT ALL: a user deploying nothing but serving agents, which is the entire quickstart, still had to
// install a second Knative component before the product would run.
//
// Detection is at STARTUP, deliberately, not per reconcile. Installing Eventing later needs a
// controller restart, which is stated in the log line below and in the install docs. Discovering
// per-reconcile would hide a capability change inside an unrelated code path and cost an API call
// every time.
func EventingAvailable(mgr ctrl.Manager) bool {
	rm := mgr.GetRESTMapper()
	for _, kind := range []string{"Trigger", "Broker"} {
		gk := eventingv1.SchemeGroupVersion.WithKind(kind).GroupKind()
		if _, err := rm.RESTMapping(gk, eventingv1.SchemeGroupVersion.Version); err != nil {
			if meta.IsNoMatchError(err) {
				return false
			}
			// Any other error means we could not ASK, which is not the same as "absent". Treating an
			// unreachable discovery endpoint as "no eventing" would silently disable a capability the
			// cluster has, so assume present and let the watch fail loudly instead.
			ctrl.Log.WithName("eventing").Error(err, "could not determine whether Knative Eventing is installed; assuming it is")
			return true
		}
	}
	return true
}

// deleteStaleTrigger drops a Trigger left over from an earlier eventing configuration, and is a
// NO-OP when the cluster serves no eventing CRDs.
//
// The guard is not cosmetic. Making only the WATCH conditional left this cleanup unconditional, and
// on a cluster without Knative Eventing every serving-model reconcile failed with
//
//	deleting stale *v1.Trigger hello-agent: no matches for kind "Trigger" in version "eventing.knative.dev/v1"
//
// so the agent got an AgentVersion and never a Knative Service. Found by the M179 cold-install gate
// on a real cluster — the unit tests could not see it, because they exercise the eventing entry
// points rather than the serving path that cleans up after them.
//
// There cannot be a stale Trigger on a cluster that has never been able to hold one.
func (r *AgentDeploymentReconciler) deleteStaleTrigger(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment) error {
	if !r.EventingAvailable {
		return nil
	}
	return r.deleteWorkload(ctx, deploy, &eventingv1.Trigger{})
}

// eventingUnavailableErr is the error an eventing-dependent reconcile returns on a cluster without
// Knative Eventing. It names the missing prerequisite and how to fix it, because the alternative --
// a no-op reconcile that leaves the resource looking accepted -- is the failure mode this project
// has repeatedly paid for: a console that reported read-only when it could not enumerate, a runbook
// step that 404s, an image tag nobody publishes.
func eventingUnavailableErr(what string) error {
	return fmt.Errorf(
		"%s needs Knative Eventing and this cluster does not serve eventing.knative.dev/v1: "+
			"install Knative Eventing and restart the ctxmesh controller, or use executionModel "+
			"'serving' or 'job'", what)
}

// KEDAAvailable reports whether this cluster serves KEDA's ScaledObject.
//
// Same shape and same reason as EventingAvailable. The M179 cold-install gate found the controller
// logging
//
//	if kind is a CRD, it should be installed before calling Start  kind=ScaledObject.keda.sh
//
// on a cluster without KEDA. The manager still starts -- that much was measured -- but the
// controller owning that kind never reconciles anything, which is a subtler and worse failure than
// refusing to boot: everything looks healthy and nothing happens.
func KEDAAvailable(mgr ctrl.Manager) bool {
	gk := schema.GroupKind{Group: kedatypes.GroupVersion.Group, Kind: "ScaledObject"}
	if _, err := mgr.GetRESTMapper().RESTMapping(gk, kedatypes.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false
		}
		ctrl.Log.WithName("keda").Error(err, "could not determine whether KEDA is installed; assuming it is")
		return true
	}
	return true
}

// kedaUnavailableErr is what an autoscaling reconcile returns when KEDA is absent.
func kedaUnavailableErr() error {
	return fmt.Errorf(
		"AgentScalingPolicy needs KEDA and this cluster does not serve keda.sh: install KEDA and " +
			"restart the ctxmesh controller, or remove the AgentScalingPolicy — agents run without " +
			"autoscaling")
}
