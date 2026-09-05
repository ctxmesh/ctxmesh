package bff

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

// consoleFlows is the registry the capability probe answers. Adding a write to a handler
// without adding it here is the drift this file exists to prevent, and hack/flow-completability.sh
// is the gate that catches it.
var consoleFlows = []flowSpec{
	{
		// Connect a model provider (internal/bff/providers.go, createProviderObjects).
		// Three objects, in this order: Secret (the key), SecretBinding (logical name →
		// Secret/key), ModelRoute (provider + models + the binding ref).
		name: "connectProvider",
		needs: []flowNeed{
			{group: "", resource: resSecrets, verbs: []string{verbCreate}},
			{group: agentsAPIGroup, resource: resSecretBindings, verbs: []string{verbCreate}},
			{group: agentsAPIGroup, resource: resModelRoutes, verbs: []string{verbCreate}},
		},
	},
	{
		// Re-connect / rotate the key. upsertObject creates, then UPDATES when the object
		// already exists, so the rotate path needs update on the same three objects — not
		// `secretbindings.update` alone, which is what the console checked while the write
		// that actually matters is the core Secret.
		name: "rotateProviderKey",
		needs: []flowNeed{
			{group: "", resource: resSecrets, verbs: []string{verbUpdate}},
			{group: agentsAPIGroup, resource: resSecretBindings, verbs: []string{verbUpdate}},
			{group: agentsAPIGroup, resource: resModelRoutes, verbs: []string{verbUpdate}},
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
