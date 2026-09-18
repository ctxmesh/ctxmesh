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
	return eventingAvailableVia(mgr.GetRESTMapper())
}

// eventingAvailableVia is the testable half. EventingAvailable takes a ctrl.Manager, which cannot be
// constructed in a unit test without a cluster, so the function deciding whether the manager can
// START had no test at all -- only its downstream guards did. The seam gives it one.
//
// The detection itself is correct, and was measured to be: close gate (d) found the PUBLISHED
// v0.1.0-beta.6 controller in CrashLoopBackOff on a cold cluster carrying the documented
// prerequisites, exiting on
//
//	failed to wait for agentdeployment caches to sync kind source: *v1.Trigger
//
// and the cause was not this logic but its absence from that artifact -- the guard landed after the
// tag. Probing the same cold cluster with the same RESTMapper returns NoMatch correctly. The lesson
// belongs to the release, not the code: a repair on a branch repairs nothing.
func eventingAvailableVia(rm meta.RESTMapper) bool {
	for _, kind := range []string{"Trigger", "Broker"} {
		gk := eventingv1.SchemeGroupVersion.WithKind(kind).GroupKind()
		if _, err := rm.RESTMapping(gk, eventingv1.SchemeGroupVersion.Version); err != nil {
			if meta.IsNoMatchError(err) {
				return false
			}
			// We could not ASK, which is not the same as "absent" -- but the two guesses are not
			// symmetric. Guessing ABSENT disables one execution model until a restart. Guessing
			// PRESENT registers a watch on a kind that may not exist, controller-runtime then fails
			// the cache sync, and the manager EXITS: nothing reconciles at all. The old code guessed
			// present and said so ("let the watch fail loudly instead"), which trades one degraded
			// feature for the entire control plane. So an unanswerable question resolves to absent.
			ctrl.Log.WithName("eventing").Error(err,
				"could not determine whether Knative Eventing is installed; treating it as ABSENT so the "+
					"manager can still start — install Knative Eventing and restart the controller to enable "+
					"the eventing execution model")
			return false
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
	return kedaAvailableVia(mgr.GetRESTMapper())
}

// kedaAvailableVia is the testable half, same seam and same asymmetry as eventingAvailableVia.
func kedaAvailableVia(rm meta.RESTMapper) bool {
	gk := schema.GroupKind{Group: kedatypes.GroupVersion.Group, Kind: "ScaledObject"}
	if _, err := rm.RESTMapping(gk, kedatypes.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false
		}
		ctrl.Log.WithName("keda").Error(err,
			"could not determine whether KEDA is installed; treating it as ABSENT so the manager can still "+
				"start — install KEDA and restart the controller to enable autoscaling")
		return false
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
