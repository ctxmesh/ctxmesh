package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

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

// ── the DETECTION, which had no test until close gate (d) found the crash ────────────────────────
//
// Both functions decide whether the manager may register a watch. Registering one for a kind the
// cluster does not serve does not degrade a feature -- controller-runtime fails the cache sync and
// the manager EXITS, so nothing reconciles at all. That is what a cold cluster with only Knative
// Serving (the documented prerequisite) got from the PUBLISHED v0.1.0-beta.6: CrashLoopBackOff on
// "failed to wait for agentdeployment caches to sync kind source: *v1.Trigger". The guard that
// prevents it landed after that tag, so the defect is in the artifact, not in this logic -- which
// is why the tests below cover the decision AND both of its failure directions, rather than
// asserting the bug that was already fixed.

// absentMapper answers NoMatch for everything, like a cluster without the optional CRDs.
type absentMapper struct{ meta.RESTMapper }

func (absentMapper) RESTMapping(gk schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	return nil, &meta.NoKindMatchError{GroupKind: gk}
}

// unanswerableMapper fails with something that is NOT NoMatch -- a discovery endpoint we could not
// reach. "Could not ask" is not "absent", but the two failures are not symmetric, and the safe
// resolution is the one that still lets the manager start.
type unanswerableMapper struct{ meta.RESTMapper }

func (unanswerableMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("discovery unreachable")
}

// presentMapper answers successfully, like a cluster that has the CRDs.
type presentMapper struct{ meta.RESTMapper }

func (presentMapper) RESTMapping(gk schema.GroupKind, vs ...string) (*meta.RESTMapping, error) {
	v := "v1"
	if len(vs) > 0 {
		v = vs[0]
	}
	return &meta.RESTMapping{GroupVersionKind: gk.WithVersion(v)}, nil
}

func TestEventingDetectionSaysAbsentWhenTheKindsAreAbsent(t *testing.T) {
	if eventingAvailableVia(absentMapper{}) {
		t.Fatal("eventing reported AVAILABLE on a cluster serving no eventing kinds — the manager would " +
			"register a Trigger watch, fail its cache sync, and exit")
	}
	if kedaAvailableVia(absentMapper{}) {
		t.Fatal("KEDA reported AVAILABLE on a cluster serving no keda.sh kinds")
	}
}

func TestOptionalDetectionFailsSAFEWhenDiscoveryCannotBeAsked(t *testing.T) {
	// The old code assumed PRESENT here and said so: "let the watch fail loudly instead". The watch
	// failing loudly means the manager exits and the whole control plane is down, which is strictly
	// worse than one execution model being unavailable until a restart.
	if eventingAvailableVia(unanswerableMapper{}) {
		t.Fatal("an unanswerable discovery question resolved to AVAILABLE — that takes the manager down " +
			"rather than degrading one execution model")
	}
	if kedaAvailableVia(unanswerableMapper{}) {
		t.Fatal("an unanswerable discovery question resolved to AVAILABLE for KEDA")
	}
}

func TestOptionalDetectionSaysPresentWhenTheKindsResolve(t *testing.T) {
	// The other direction, so the fail-safe above cannot be satisfied by always answering absent.
	if !eventingAvailableVia(presentMapper{}) {
		t.Fatal("eventing reported ABSENT on a cluster that serves the kinds — the eventing execution " +
			"model would be silently disabled")
	}
	if !kedaAvailableVia(presentMapper{}) {
		t.Fatal("KEDA reported ABSENT on a cluster that serves ScaledObject")
	}
}
