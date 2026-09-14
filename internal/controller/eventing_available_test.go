package controller

import (
	"context"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// Knative Eventing is an OPTIONAL capability (ADR 0141). Only one of three execution models needs
// it, yet the controller used to Own() Trigger and Broker unconditionally — and controller-runtime
// starts an informer per owned kind, so a cluster without those CRDs could not start the manager at
// all. A user deploying nothing but serving agents, which is the entire quickstart, still had to
// install a second Knative component before the product would run.

func TestEventingUnavailableErrorNamesThePrerequisiteAndTheWayOut(t *testing.T) {
	// The message is the whole point. A no-op reconcile that leaves the resource looking accepted
	// is the failure mode this project keeps paying for: a console reporting read-only when it
	// could not enumerate, a runbook step that 404s, an image tag nobody publishes. Whoever hits
	// this must learn what is missing AND what to do, from the error alone.
	err := eventingUnavailableErr("executionModel 'eventing'")
	if err == nil {
		t.Fatal("an eventing-dependent reconcile on a cluster without eventing must return an error, not nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"executionModel 'eventing'", // what the user asked for
		"Knative Eventing",          // what is missing
		"eventing.knative.dev/v1",   // the exact API group, so it is greppable
		"restart",                   // detection is at startup, so installing it later is not enough
		"'serving' or 'job'",        // the way out without installing anything
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name %q so the operator can act on it alone; got: %s", want, msg)
		}
	}
}

func TestEventingDependentReconcilesRefuseWhenUnavailable(t *testing.T) {
	// Both eventing entry points must refuse, not skip. reconcileTrigger and reconcileBroker each
	// guard on EventingAvailable; a false here that returned nil would leave an agent bound to a
	// registry whose broker does not exist, reporting success, receiving nothing, forever.
	if err := eventingUnavailableErr("an AgentRegistry broker"); err == nil ||
		!strings.Contains(err.Error(), "AgentRegistry broker") {
		t.Fatalf("the broker path must fail with its own specific message; got %v", err)
	}
}

func TestReconcileTriggerRefusesWithoutEventing(t *testing.T) {
	// The GUARD, not the message. With EventingAvailable false the reconcile must return before it
	// touches the API at all -- which is why a zero-value reconciler with no client is a valid
	// fixture here: if the guard is removed, this panics or errors on a nil client instead of
	// returning the specific error, and either way the test goes red.
	r := &AgentDeploymentReconciler{EventingAvailable: false}
	err := r.reconcileTrigger(context.Background(),
		&agentsv1alpha1.AgentDeployment{}, registryMembership{RegistryName: "r"})
	if err == nil {
		t.Fatal("reconcileTrigger must refuse on a cluster without Knative Eventing, not silently skip")
	}
	if !strings.Contains(err.Error(), "Knative Eventing") {
		t.Errorf("the refusal must name the missing prerequisite; got: %v", err)
	}
}

func TestReconcileBrokerRefusesWithoutEventing(t *testing.T) {
	r := &AgentRegistryReconciler{EventingAvailable: false}
	err := r.reconcileBroker(context.Background(), &agentsv1alpha1.AgentRegistry{})
	if err == nil {
		t.Fatal("reconcileBroker must refuse on a cluster without Knative Eventing, not silently skip")
	}
	if !strings.Contains(err.Error(), "Knative Eventing") {
		t.Errorf("the refusal must name the missing prerequisite; got: %v", err)
	}
}
