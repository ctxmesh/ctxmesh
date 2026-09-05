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

package bff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file implements the RBAC-aware-chrome endpoints (ui-foundation §3,
// ADR 0012): /api/whoami, /api/capabilities, /api/namespaces. All three run
// through the CALLER-SCOPED client (ADR 0011) so every answer is the K8s API
// server's own decision about the CALLER — the BFF re-implements no authz and
// gates nothing server-side. whoami/capabilities are DISPLAY-ONLY: the UI hides
// or disables affordances the caller lacks; enforcement stays with K8s.

// annNamespaceDisplayName is the annotation key on a Namespace object that
// carries its human-readable display label (ADR 0068 §7). A friendly view over a
// namespace — metadata only, no policy. "workspace" is a UI-only concept; the
// wire stays namespace + display-name.
const annNamespaceDisplayName = "agents.ctxmesh.ai/display-name"

// The golden CRD resource names (plural, as the API server expects them in a
// SelfSubjectAccessReview's ResourceAttributes) the console probes capabilities
// for. Named so the string is defined once (the capabilities map keys off it and
// tests reference it).
const (
	resAgentDeployments   = "agentdeployments"
	resModelRoutes        = "modelroutes"
	resSecretBindings     = "secretbindings"
	resAgentRegistries    = "agentregistries"
	resToolRegistries     = "toolregistries"
	resMCPToolBindings    = "mcptoolbindings"
	resAgentScalingPolicy = "agentscalingpolicies"
	resEvalSuites         = "evalsuites"
	resPromptVersions     = "promptversions"
	// resAuditLogs is the virtual audit resource (ADR 0056, M63). Probed so the console can
	// gate the operator-only Audit surface on `list auditlogs` — only the operator persona's
	// ClusterRole grants it, so developer/viewer chrome hides the nav item (display-only; the
	// API still enforces on GET /api/audit).
	resAuditLogs = "auditlogs"
	// resKnowledgeBases is probed so the console can GATE the Knowledge Bases nav item on
	// `list knowledgebases` (M99 C2) — a persona that can't list KBs (e.g. developer) must not see a
	// nav item that then 403s. Display-only; the API still enforces on GET /api/knowledgebases.
	resKnowledgeBases = "knowledgebases"
	// resLogs is a SYNTHETIC capability key (NOT an agents.ctxmesh.ai CRD): it maps to the caller's
	// `get pods/log` permission in the target namespace — the CORE-group subresource the live-log
	// tail actually requires (handleAgentLogs, ADR 0011: the BFF SA has rules:[] and cannot read pod
	// logs; only the caller can). Probed so the console can GATE the agent-detail **Logs** tab (M100
	// UI99-logs): a persona who can't read pod logs must not see a tab that then 403s. Display-only;
	// the API still enforces on GET /api/agents/{ns}/{name}/logs.
	resLogs = "logs"
	// resPods / verbGet name the core-group pods/log SSAR the logs capability probes (M100).
	resPods = "pods"
	verbGet = "get"
	// resSecrets is the CORE-group Secret. Connecting a provider writes one (the key)
	// alongside the SecretBinding, and the console used to gate that flow on the
	// SecretBinding alone — so a caller who could create bindings but not Secrets was
	// invited to type an API key into a form the API would refuse, and only found out
	// once the credential was already in flight (M153). The synthetic `secrets.create`
	// capability exists so the console can ask about what the operation ACTUALLY needs.
	resSecrets = "secrets"
	verbCreate = "create"
	// verbUpdate names the write verb an upsert falls back to; the provider connect path
	// reports it in a denial message so a user is not told the wrong permission is missing.
	verbUpdate = "update"
	// verbList is the read verb the caller-scoped stop list probes with (ADR 0129).
	verbList = "list"
)

// agentsAPIGroup is the API group all the golden CRD kinds live in.
const agentsAPIGroup = "agents.ctxmesh.ai"

// golden{Resources,Verbs} are the capability matrix the console probes per
// namespace (ui-foundation §3). Resources are the plural CRD names in the
// agents.ctxmesh.ai group; verbs are the standard write/read set. The console
// keys off exactly these strings, so the response map is `resource -> verb ->
// allowed` over this cross product.
var (
	goldenResources = []string{
		resAgentDeployments,
		resModelRoutes,
		resSecretBindings,
		resAgentRegistries,
		resToolRegistries,
		resMCPToolBindings,
		resAgentScalingPolicy,
		resEvalSuites,
		resPromptVersions,
		resAuditLogs,
		resKnowledgeBases,
	}
	goldenVerbs = []string{verbGet, verbList, verbCreate, verbUpdate, "delete"}
)

// handleWhoAmI serves GET /api/whoami — the caller's identity (username +
// groups) as the API server reports it. It issues a SelfSubjectReview
// (authentication/v1) through the CALLER-SCOPED client, so the token being
// described is exactly the caller's own (ADR 0011). A missing token → 401 before
// any K8s call (the factory gate). An API-server rejection surfaces honestly as
// 401/403 (never a 500 masking an authn failure); anything else is a real 500.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	// A SelfSubjectReview asks the API server "who am I?" for the token that
	// authenticates THIS request. It is a create-only virtual resource: the
	// server fills Status.UserInfo on the create.
	review := &authnv1.SelfSubjectReview{}
	if err := caller.Create(r.Context(), review); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "SelfSubjectReview failed")
		writeError(w, http.StatusInternalServerError, "failed to resolve caller identity")
		return
	}

	groups := review.Status.UserInfo.Groups
	if groups == nil {
		// [] not null so the header never guards a null.
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, WhoAmIResponse{
		Username: review.Status.UserInfo.Username,
		Groups:   groups,
	})
}

// handleCapabilities serves GET /api/capabilities?namespace=<ns> — a flat
// capability map for the golden kinds × verbs, computed by batched SelfSubject
// AccessReviews (authorization/v1) through the CALLER-SCOPED client. Each SSAR
// asks the API server "may the CALLER do <verb> on <resource> in <ns>?", so the
// answers are the server's own RBAC decisions (ADR 0011). This is DISPLAY-ONLY:
// the UI disables what is false; the API server still enforces on the real op.
//
// The reviews run in parallel (independent, read-only probes); a single review's
// failure (a true API error, not a plain allowed/denied) fails the whole request
// honestly rather than reporting a half-filled matrix as success. There is no
// server-side cache — the SPA caches the result per session.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := r.URL.Query().Get("namespace")

	allowed, err := probeCapabilities(r.Context(), caller, namespace)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "capabilities probe failed")
		writeError(w, http.StatusInternalServerError, "failed to probe capabilities")
		return
	}

	writeJSON(w, http.StatusOK, CapabilitiesResponse{
		Namespace: namespace,
		Allowed:   allowed,
		Flows:     evaluateFlows(allowed),
	})
}

// probeCapabilities runs one SelfSubjectAccessReview per golden resource × verb
// against the caller-scoped client and folds them into resource → verb →
// allowed. The SSARs run concurrently (each is an independent read-only probe);
// the first true API error (not an allowed/denied answer) is returned so the
// handler can surface it honestly. The result map is fully populated for every
// resource × verb (an errored request never yields a partial matrix).
func probeCapabilities(ctx context.Context, caller client.Client, namespace string) (map[string]map[string]bool, error) {
	// Pre-size and pre-seed the result so concurrent writers only ever touch
	// their own (resource, verb) cell — no map-growth race, no shared verb map
	// written by two goroutines.
	allowed := make(map[string]map[string]bool, len(goldenResources)+1)
	for _, res := range goldenResources {
		allowed[res] = make(map[string]bool, len(goldenVerbs))
	}
	// The synthetic `logs` capability is a SINGLE `get pods/log` probe (core group + subresource),
	// distinct from the agents-group golden cross-product — seed its own cell.
	allowed[resLogs] = make(map[string]bool, 1)
	// Same shape as `logs`: a single core-group probe outside the agents-group
	// cross-product, seeded with its own cell.
	allowed[resSecrets] = make(map[string]bool, 1)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, res := range goldenResources {
		for _, verb := range goldenVerbs {
			wg.Add(1)
			go func(res, verb string) {
				defer wg.Done()
				ok, err := reviewAccess(ctx, caller, namespace, res, verb)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					return
				}
				allowed[res][verb] = ok
			}(res, verb)
		}
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	// The synthetic `logs` capability (core-group `get pods/log`, M100 UI99-logs) — one extra SSAR,
	// run after the golden fan-out. A single serial round-trip keeps the concurrency simple; its own
	// probe so a nil answer never masks a golden deny.
	logsOK, err := reviewLogsAccess(ctx, caller, namespace)
	if err != nil {
		return nil, err
	}
	allowed[resLogs][verbGet] = logsOK

	// The core-group Secret probes. Which verbs to ask about is DERIVED from the flow registry
	// rather than hardcoded, so a flow that starts needing a new verb cannot end up evaluating
	// against an unprobed cell — which the UI's optimistic default would read as allowed.
	for _, verb := range flowNeedsCoreSecretVerbs() {
		ok, err := reviewCoreAccess(ctx, caller, namespace, resSecrets, "", verb)
		if err != nil {
			return nil, err
		}
		allowed[resSecrets][verb] = ok
	}
	return allowed, nil
}

// reviewAccess issues one SelfSubjectAccessReview for (namespace, resource, verb)
// in the agents API group and returns the API server's allow decision. A
// transport/API error (not the allow/deny answer itself) is returned so the
// caller can fail honestly.
func reviewAccess(ctx context.Context, caller client.Client, namespace, resource, verb string) (bool, error) {
	ssar := &authzv1.SelfSubjectAccessReview{
		Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Group:     agentsAPIGroup,
				Resource:  resource,
				Verb:      verb,
			},
		},
	}
	if err := caller.Create(ctx, ssar); err != nil {
		return false, err
	}
	return ssar.Status.Allowed, nil
}

// reviewLogsAccess issues one SelfSubjectAccessReview for `get pods/log` in namespace — the
// CORE-group subresource the live pod-log tail requires (handleAgentLogs). It is separate from
// reviewAccess (which is agents-group only) because pod logs live in the core API group ("") with
// the `log` subresource, not in agents.ctxmesh.ai. A transport/API error (not the allow/deny
// answer) is returned so the caller can fail honestly.
func reviewLogsAccess(ctx context.Context, caller client.Client, namespace string) (bool, error) {
	return reviewCoreAccess(ctx, caller, namespace, resPods, "log", verbGet)
}

// reviewCoreAccess issues one SelfSubjectAccessReview against the CORE API group
// (the empty group) for (namespace, resource[/subresource], verb). The agents-group
// probes go through reviewAccess; this is its core-group sibling, shared by the
// pods/log and secrets probes so the two cannot drift apart.
func reviewCoreAccess(ctx context.Context, caller client.Client, namespace, resource, subresource, verb string) (bool, error) {
	ssar := &authzv1.SelfSubjectAccessReview{
		Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   namespace,
				Group:       "", // core API group
				Resource:    resource,
				Subresource: subresource,
				Verb:        verb,
			},
		},
	}
	if err := caller.Create(ctx, ssar); err != nil {
		return false, err
	}
	return ssar.Status.Allowed, nil
}

// handleNamespaces serves GET /api/namespaces — the namespaces the CALLER can
// list, through the CALLER-SCOPED client (ADR 0011). A caller whose RBAC does
// not permit listing namespaces gets an honest 403 (via classifyReadError), NEVER
// a silent empty list that would masquerade as "no namespaces exist". An
// authentically empty cluster still yields {"namespaces":[]}.
func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	var list corev1.NamespaceList
	if err := caller.List(r.Context(), &list); err != nil {
		if _, _, isRBAC := classifyReadError(err); isRBAC {
			// The caller cannot `list namespaces` cluster-wide. That is the NORMAL state for
			// every persona bound per-namespace — which is the binding shape ADR 0136 and
			// catalog.go's tenant-isolation precondition both require. Returning 403 here left
			// the picker empty and the console with no namespace to work in, so it fell back to
			// "" — a cluster-scoped capability probe that no namespaced RoleBinding can satisfy.
			//
			// The projects pattern instead (what the OpenShift console does): enumerate
			// CANDIDATES with the platform's own view, then filter to what THIS caller may use.
			// Only SSAR-approved names reach the wire, so this is not a namespace-existence
			// oracle. The BFF gains no Kubernetes privilege — the mirror is its own Postgres
			// projection, and every allow decision is still the API server's, made as the caller.
			allowed, learned, ferr := s.callerVisibleNamespaces(r.Context(), caller)
			if ferr != nil {
				s.log.Error(ferr, "namespace discovery fallback failed")
				writeError(w, http.StatusInternalServerError, "failed to determine your namespaces")
				return
			}
			if !learned {
				// The fallback had nothing to enumerate (no mirror, or no namespace in it), so it
				// learned NOTHING about this caller. Returning [] here would assert "you have no
				// namespaces" — a fact we do not have. The honest answer is still the denial.
				status, msg, _ := classifyReadError(err)
				writeError(w, status, msg)
				return
			}
			writeJSON(w, http.StatusOK, NamespaceListResponse{Namespaces: allowed})
			return
		}
		s.log.Error(err, "list namespaces failed")
		writeError(w, http.StatusInternalServerError, "failed to list namespaces")
		return
	}

	summaries := make([]NamespaceSummary, 0, len(list.Items))
	for i := range list.Items {
		ns := &list.Items[i]
		s := NamespaceSummary{Name: ns.Name}
		if dn := ns.Annotations[annNamespaceDisplayName]; dn != "" {
			s.DisplayName = dn
		}
		summaries = append(summaries, s)
	}
	// Stable order so the SPA's namespace picker is deterministic.
	slices.SortFunc(summaries, func(a, b NamespaceSummary) int { return strings.Compare(a.Name, b.Name) })

	writeJSON(w, http.StatusOK, NamespaceListResponse{Namespaces: summaries})
}

// callerVisibleNamespaces is the fallback for a caller who cannot `list namespaces`: the
// namespaces the mirror knows about, filtered to those where the caller may actually work.
//
// Membership is probed with `list agentdeployments` — the one verb every shipped persona
// grants, so it means "this namespace is usable by you" rather than accidentally selecting for
// some narrower permission. Fail-closed: a namespace whose probe errors is omitted, never
// included on the benefit of the doubt.
//
// The second return says whether the fallback LEARNED anything: false when there was nothing to
// enumerate (no mirror configured, or an empty one). That distinction is load-bearing. An empty
// result with learned=true means "candidates existed and none are yours" — a real, honest state
// the SPA should render as "ask an operator for a workspace". An empty result with learned=false
// means "we could not tell", and answering [] there would assert a fact we do not have; the
// caller's original denial is the honest answer instead.
func (s *Server) callerVisibleNamespaces(ctx context.Context, caller client.Client) ([]NamespaceSummary, bool, error) {
	if s.namespaceTenantStore == nil {
		return nil, false, nil
	}
	candidates, err := s.namespaceTenantStore.AllNamespaces(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	out := make([]NamespaceSummary, 0, len(candidates))
	for _, ns := range candidates {
		ok, rerr := reviewAccess(ctx, caller, ns, resAgentDeployments, verbList)
		if rerr != nil {
			s.log.Error(rerr, "namespace membership probe failed (omitting)", "namespace", ns)
			continue
		}
		if ok {
			out = append(out, NamespaceSummary{Name: ns})
		}
	}
	return out, true, nil
}

// handleSetNamespaceDisplayName serves PUT /api/namespaces/{name}/display-name
// (ADR 0068 §7). It sets or clears the agents.ctxmesh.ai/display-name annotation
// on the named Namespace through the CALLER-SCOPED client — the caller needs
// "update namespaces" in their own RBAC; an honest 403 is returned if not. This
// is a pure annotation write — no CRD, no new type, no policy change. An empty
// displayName removes the annotation (reverts to showing the raw namespace name
// in the UI). "workspace" never appears in this path — that word is UI-only.
func (s *Server) handleSetNamespaceDisplayName(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing namespace name")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req SetNamespaceDisplayNameRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	// Fetch the current namespace so we can patch annotations in place.
	var ns corev1.Namespace
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name}, &ns); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read namespace")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, fmt.Sprintf("namespace %q not found", name))
		default:
			s.log.Error(err, "get namespace failed", "namespace", name)
			writeError(w, http.StatusInternalServerError, "failed to read namespace")
		}
		return
	}

	// Set or clear the display-name annotation.
	if req.DisplayName == "" {
		delete(ns.Annotations, annNamespaceDisplayName)
	} else {
		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		ns.Annotations[annNamespaceDisplayName] = req.DisplayName
	}

	if err := caller.Update(r.Context(), &ns); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to update namespace")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		default:
			s.log.Error(err, "update namespace annotations failed", "namespace", name)
			writeError(w, http.StatusInternalServerError, "failed to update namespace")
		}
		return
	}

	summary := NamespaceSummary{Name: ns.Name, DisplayName: ns.Annotations[annNamespaceDisplayName]}
	writeJSON(w, http.StatusOK, summary)
}
