package bff

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// Console FLOWS — a flow is one console action and the COMPLETE set of writes it performs.
//
// This exists because the UI used to assemble the permission question itself, resource by
// resource, and got it wrong in the direction that matters. `canConnectProvider` asked for
// `secrets.create && secretbindings.create` while createProviderObjects upserts a THIRD object,
// a ModelRoute — and asked only about `create` while the path is an upsert. A caller holding
// the first two and not the third saw an enabled button, and because the Secret is written
// first and the sequence is not transactional, a denial mid-way left a live credential in the
// cluster with no route to use it.
//
// The fix is not a longer conjunction in the UI. It is moving the question to the only party
// that knows the answer: the handler that performs the writes. The UI asks "may I run this
// flow?" and never enumerates resources again, so the two cannot drift.
//
// The same drift already existed elsewhere and had gone unnoticed: goldenResources omits
// `workflows` and `alertpolicies` while nav gated on them, and an unprobed cell reads as
// optimistic-true — so those gates never gated anything at all.

// flowNeed is one resource the flow writes, and every verb it may need on it.
type flowNeed struct {
	// group is the API group; "" is core (Secrets live there).
	group string
	// resource is the plural resource name as RBAC spells it.
	resource string
	// verbs are ALL required — a flow is completable only when every verb on every need is
	// allowed. Listing a verb the flow does not use would hide a working control, so each
	// entry is the verb that specific path actually issues.
	verbs []string
}

// flowSpec is a named console action and its complete write set.
type flowSpec struct {
	name  string
	needs []flowNeed
}

// flowConnectProvider names the provider-connect flow; the handler authorizes it by name.
const flowConnectProvider = "connectProvider"

// consoleFlows is the registry the capability probe answers. Adding a write to a handler
// without adding it here is the drift this file exists to prevent, and hack/flow-completability.sh
// is the gate that catches it.
var consoleFlows = []flowSpec{
	{
		// Connect a model provider (internal/bff/providers.go, createProviderObjects).
		// Three objects, in this order: Secret (the key), SecretBinding (logical name →
		// Secret/key), ModelRoute (provider + models + the binding ref).
		name: flowConnectProvider,
		needs: []flowNeed{
			{group: "", resource: resSecrets, verbs: []string{verbCreate}},
			{group: agentsAPIGroup, resource: resSecretBindings, verbs: []string{verbCreate}},
			{group: agentsAPIGroup, resource: resModelRoutes, verbs: []string{verbCreate}},
		},
	},
	{
		// Rotate the key (handleRotateProviderKey). It reads the ModelRoute and SecretBinding to
		// recover what connect wrote, then UPDATES all three objects through connect's write path
		// and never creates one. A custom resource cannot be updated without its resourceVersion,
		// which the update reads first (get). The Secret is updated without a read: get on a
		// Secret returns its data, and no shipped persona may hold it (ADR 0136).
		// TestRotateProviderKey_IssuesExactlyTheFlowVerbs holds the handler to this list.
		name: "rotateProviderKey",
		needs: []flowNeed{
			{group: "", resource: resSecrets, verbs: []string{verbUpdate}},
			{group: agentsAPIGroup, resource: resSecretBindings, verbs: []string{verbGet, verbUpdate}},
			{group: agentsAPIGroup, resource: resModelRoutes, verbs: []string{verbGet, verbUpdate}},
		},
	},
}

// flowNeedsCoreSecretVerbs returns the core-group Secret verbs any flow depends on, so the
// probe knows which extra SSARs to run beyond the agents-group cross-product. Derived from the
// registry rather than hardcoded — a new flow needing a new Secret verb must not silently
// evaluate against an unprobed cell, which reads as optimistic-true.
func flowNeedsCoreSecretVerbs() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range consoleFlows {
		for _, n := range f.needs {
			if n.group != "" || n.resource != resSecrets {
				continue
			}
			for _, v := range n.verbs {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// evaluateFlows folds a probed capability map into flow → completable.
//
// A missing cell is treated as NOT allowed, deliberately inverting the UI's optimistic default.
// Optimism is right for hiding chrome and wrong for a write: an unprobed cell means "we did not
// ask", and offering a flow we did not verify is how the partial-write above happened.
func evaluateFlows(allowed map[string]map[string]bool) map[string]bool {
	out := make(map[string]bool, len(consoleFlows))
	for _, f := range consoleFlows {
		ok := true
		for _, n := range f.needs {
			verbs := allowed[n.resource]
			for _, v := range n.verbs {
				if !verbs[v] {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		out[f.name] = ok
	}
	return out
}

// authorizeFlow asks, as the caller, for every permission the named flow needs in namespace,
// and returns the first refusal (authz.ErrForbidden) or API error. A handler whose flow starts
// with an outbound request to a caller-supplied URL calls this first, so a caller who could not
// complete the writes never gets the BFF to contact anything on their behalf.
func (s *Server) authorizeFlow(ctx context.Context, caller client.Client, name, namespace string) error {
	for _, f := range consoleFlows {
		if f.name != name {
			continue
		}
		for _, n := range f.needs {
			for _, v := range n.verbs {
				if err := s.authorizer.Authorize(ctx, caller, authz.Action{
					Verb: v, Group: n.group, Resource: n.resource, Namespace: namespace,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return fmt.Errorf("authorize flow: no console flow named %q", name)
}
