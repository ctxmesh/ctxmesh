package controller

import (
	"fmt"

	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
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
