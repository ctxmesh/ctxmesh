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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// What the console may say about one thing an agent references.
const (
	// depReady: the object exists and its own status says it works.
	depReady = "ready"
	// depNotReady: the object exists, and its status says it does not work yet. Reason and
	// Message carry the condition's own words.
	depNotReady = "notReady"
	// depMissing: nothing of that name exists where the agent looks for it.
	depMissing = "missing"
	// depUnknown: the caller may not read it, or the read failed. Nothing is claimed either way.
	depUnknown = "unknown"
	// depDeclared: a spec field with no object behind it, so there is nothing to resolve.
	depDeclared = "declared"
)

// The kinds of thing an agent can reference.
const (
	depKindModelRoute      = "modelRoute"
	depKindSecretBinding   = "secretBinding"
	depKindGuardrailPolicy = "guardrailPolicy"
	depKindToolBinding     = "toolBinding"
	depKindKnowledgeBase   = "knowledgeBase"
	depKindSessionMemory   = "sessionMemory"
	depKindLongTermMemory  = "longTermMemory"
	depKindApprovalRule    = "approvalRule"
	depKindApprover        = "approver"
	depKindFeedbackSource  = "feedbackSource"
)

const (
	conditionModelRouteReady = "ModelRouteReady"
	conditionKnowledgeReady  = "KnowledgeReady"
	conditionValidated       = "Validated"
	reasonNotReconciled      = "NotReconciled"
	reasonSecretResolved     = "Resolved"
	reasonSecretUnreadable   = "SecretUnreadable"
	reasonRouteNotFound      = "RouteNotFound"
	// reasonChecking: the controller's word on the route predates the agent's current spec.
	reasonChecking = "Checking"
	// reasonValueFrom: MODEL_ROUTE is read from a ConfigMap, Secret or field when the pod starts.
	reasonValueFrom = "ValueFrom"
	// reasonNeedsIngest: a knowledge base nothing has ingested, with no schedule that will.
	reasonNeedsIngest       = "NeedsIngest"
	kbPhasePending          = "Pending"
	toolRuleRequireApproval = "require-approval"
)

// AgentDependency is one thing an agent references, resolved as the caller (ADR 0011). A reference
// that does not resolve is listed as missing, never dropped: a dangling reference is the row a
// reader of the console's Bindings panel most needs. The kinds listed are those below; an agent's
// other references (an eval suite, the embedding route of its long-term memory) are not resolved.
type AgentDependency struct {
	// Kind is one of modelRoute, secretBinding, guardrailPolicy, toolBinding, knowledgeBase,
	// sessionMemory, longTermMemory, approvalRule, approver, feedbackSource.
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Detail is the human-facing subject: a tool name, a memory scope, an approver's kind.
	Detail string `json:"detail,omitempty"`
	// Via names what the reference is reached through: the model route a secret binding is used by,
	// the MCP server a tool binding belongs to.
	Via string `json:"via,omitempty"`
	// State is ready, notReady, missing, unknown or declared.
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// resolveAgentDependencies lists everything the agent references, each resolved with the caller's
// own client. toolBindings are the namespace's MCPToolBindings (already read caller-scoped);
// toolBindingsHidden says the caller may not list them, which is reported rather than shown as none.
func (s *Server) resolveAgentDependencies(
	ctx context.Context,
	caller client.Client,
	ad *agentsv1beta1.AgentDeployment,
	toolBindings []agentsv1beta1.MCPToolBinding,
	toolBindingsHidden bool,
) []AgentDependency {
	deps := make([]AgentDependency, 0, 8)
	deps = append(deps, s.modelRouteDependencies(ctx, caller, ad)...)
	if d, ok := s.guardrailDependency(ctx, caller, ad); ok {
		deps = append(deps, d)
	}
	deps = append(deps, toolBindingDependencies(ad, toolBindings, toolBindingsHidden)...)
	deps = append(deps, s.knowledgeBaseDependencies(ctx, caller, ad)...)
	deps = append(deps, memoryDependencies(ad)...)
	deps = append(deps, approvalDependencies(ad)...)
	deps = append(deps, feedbackDependencies(ad)...)
	return deps
}

// unreadable records that a reference could not be read. A denial is the caller's RBAC and says so;
// any other failure is logged and reported as a failed read, never as missing.
func (s *Server) unreadable(err error, d AgentDependency, what string) AgentDependency {
	d.State = depUnknown
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		d.Reason = "Forbidden"
		d.Message = fmt.Sprintf("Your account can't read this %s, so its state is not shown.", what)
		return d
	}
	s.log.Error(err, "agent detail: reading a referenced object failed",
		"kind", d.Kind, "namespace", d.Namespace, "name", d.Name)
	d.Reason = "ReadFailed"
	d.Message = fmt.Sprintf("The %s could not be read just now.", what)
	return d
}

// fromCondition sets the state from a Ready-style condition: True is ready, anything else is not
// ready with the condition's reason and message. A nil condition means its controller has not
// reported yet, which is not ready.
func fromCondition(d AgentDependency, c *metav1.Condition, pendingReason, pendingMessage string) AgentDependency {
	if c == nil {
		d.State, d.Reason, d.Message = depNotReady, pendingReason, pendingMessage
		return d
	}
	d.Reason, d.Message = c.Reason, c.Message
	if c.Status == metav1.ConditionTrue {
		d.State = depReady
	} else {
		d.State = depNotReady
	}
	return d
}

// modelRouteEnv is the agent's MODEL_ROUTE variable, or nil when it sets none.
func modelRouteEnv(ad *agentsv1beta1.AgentDeployment) *corev1.EnvVar {
	for i := range ad.Spec.Env {
		if ad.Spec.Env[i].Name == envModelRoute {
			return &ad.Spec.Env[i]
		}
	}
	return nil
}

// valueFromDependency describes a MODEL_ROUTE read from elsewhere when the pod starts. The console
// cannot tell which route that is without reading the source, so it says where the name comes from.
func valueFromDependency(ad *agentsv1beta1.AgentDeployment, src *corev1.EnvVarSource) AgentDependency {
	from := "a field of the pod"
	switch {
	case src.ConfigMapKeyRef != nil:
		from = fmt.Sprintf("ConfigMap %s, key %s", src.ConfigMapKeyRef.Name, src.ConfigMapKeyRef.Key)
	case src.SecretKeyRef != nil:
		from = fmt.Sprintf("Secret %s, key %s", src.SecretKeyRef.Name, src.SecretKeyRef.Key)
	}
	return AgentDependency{
		Kind: depKindModelRoute, Name: envModelRoute, Namespace: ad.Namespace, Detail: "from " + from,
		State: depDeclared, Reason: reasonValueFrom,
		Message: fmt.Sprintf("MODEL_ROUTE is read from %s when the agent starts, so which route it names, "+
			"and whether that route is served, is not shown here.", from),
	}
}

// modelRouteDependencies resolves the agent's model route and the secret bindings its providers use.
func (s *Server) modelRouteDependencies(
	ctx context.Context,
	caller client.Client,
	ad *agentsv1beta1.AgentDeployment,
) []AgentDependency {
	env := modelRouteEnv(ad)
	if env == nil {
		return nil
	}
	if env.ValueFrom != nil {
		return []AgentDependency{valueFromDependency(ad, env.ValueFrom)}
	}
	name := strings.TrimSpace(env.Value)
	if name == "" {
		return nil
	}
	d := AgentDependency{Kind: depKindModelRoute, Name: name, Namespace: ad.Namespace}
	var mr agentsv1beta1.ModelRoute
	err := caller.Get(ctx, client.ObjectKey{Namespace: ad.Namespace, Name: name}, &mr)
	switch {
	case apierrors.IsNotFound(err):
		d.State, d.Reason = depMissing, reasonRouteNotFound
		d.Message = fmt.Sprintf("There is no ModelRoute named %q in %s, so the agent has no model to call.",
			name, ad.Namespace)
		return []AgentDependency{d}
	case apierrors.IsForbidden(err):
		// The controller reads the route with its own identity and mirrors it onto the agent
		// (ADR 0151). Its word is better than none for a caller who may read only the agent, but
		// only once it is about the agent's current spec: before that it may describe the route the
		// agent named before an edit.
		c := apimeta.FindStatusCondition(ad.Status.Conditions, conditionModelRouteReady)
		switch {
		case c == nil:
			return []AgentDependency{s.unreadable(err, d, "model route")}
		case c.ObservedGeneration < ad.Generation:
			d.State, d.Reason = depUnknown, reasonChecking
			d.Message = "Your account can't read this route, and the controller has not yet checked the route " +
				"the agent's current spec names."
			return []AgentDependency{d}
		case c.Reason == reasonRouteNotFound:
			d.State, d.Reason, d.Message = depMissing, c.Reason, c.Message
			return []AgentDependency{d}
		default:
			return []AgentDependency{fromCondition(d, c, "", "")}
		}
	case err != nil:
		return []AgentDependency{s.unreadable(err, d, "model route")}
	}
	deps := []AgentDependency{fromCondition(d, apimeta.FindStatusCondition(mr.Status.Conditions, conditionReadyType),
		"NotYetServed", "The controller has not reconciled this route yet.")}
	seen := map[string]bool{}
	for _, p := range mr.Spec.Providers {
		ref := strings.TrimSpace(p.SecretBindingRef)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		deps = append(deps, s.secretBindingDependency(ctx, caller, ad.Namespace, ref, name))
	}
	return deps
}

// secretBindingDependency resolves one SecretBinding a model route's provider names. Whether its Secret
// holds the key is checked as the caller, exactly as the secret-bindings list does; the value is never read.
func (s *Server) secretBindingDependency(
	ctx context.Context,
	caller client.Client,
	ns, name, route string,
) AgentDependency {
	d := AgentDependency{Kind: depKindSecretBinding, Name: name, Namespace: ns, Via: route}
	var sb agentsv1beta1.SecretBinding
	if err := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			d.State, d.Reason = depMissing, "SecretBindingNotFound"
			d.Message = fmt.Sprintf("Model route %s names SecretBinding %q, and there is none in %s.", route, name, ns)
			return d
		}
		return s.unreadable(err, d, "secret binding")
	}
	ready, phase, readErr := s.secretBindingProbe(ctx, caller, &sb)
	switch {
	case ready:
		d.State, d.Reason = depReady, reasonSecretResolved
	case phase == phaseNotReady:
		d.State, d.Reason = depNotReady, "SecretUnresolved"
		d.Message = fmt.Sprintf("Secret %q does not exist, or has no key %q.", sb.Spec.SecretRef.Name, sb.Spec.SecretRef.Key)
	default:
		d.State, d.Reason = depUnknown, reasonSecretUnreadable
		d.Message = s.secretUnreadableMessage(&sb, readErr)
	}
	return d
}

// secretUnreadableMessage says why a binding's Secret could not be checked: the caller's RBAC, or a
// read that failed.
func (s *Server) secretUnreadableMessage(sb *agentsv1beta1.SecretBinding, readErr error) string {
	switch {
	case apierrors.IsForbidden(readErr) || apierrors.IsUnauthorized(readErr):
		return fmt.Sprintf("Your account can't read Secrets in %s, so whether Secret %q holds key %q is not shown.",
			sb.Namespace, sb.Spec.SecretRef.Name, sb.Spec.SecretRef.Key)
	case readErr != nil:
		s.log.Error(readErr, "agent detail: reading a secret binding's Secret failed",
			"namespace", sb.Namespace, "secretBinding", sb.Name)
		return fmt.Sprintf("Secret %q could not be read just now, so whether it holds key %q is not shown.",
			sb.Spec.SecretRef.Name, sb.Spec.SecretRef.Key)
	default:
		// Only a kubernetes-backed binding naming a Secret can be probed; the API admits no other today.
		return "This binding names no Secret the console can check."
	}
}

// guardrailDependency resolves spec.guardrailPolicyRef. The controller refuses to serve an agent whose
// policy is missing or invalid (ADR 0059), so this row explains a NotReady agent.
func (s *Server) guardrailDependency(
	ctx context.Context,
	caller client.Client,
	ad *agentsv1beta1.AgentDeployment,
) (AgentDependency, bool) {
	name := strings.TrimSpace(ad.Spec.GuardrailPolicyRef)
	if name == "" {
		return AgentDependency{}, false
	}
	d := AgentDependency{Kind: depKindGuardrailPolicy, Name: name, Namespace: ad.Namespace}
	var gp agentsv1beta1.GuardrailPolicy
	err := caller.Get(ctx, client.ObjectKey{Namespace: ad.Namespace, Name: name}, &gp)
	switch {
	case apierrors.IsNotFound(err):
		d.State, d.Reason = depMissing, "GuardrailPolicyNotFound"
		d.Message = fmt.Sprintf("There is no GuardrailPolicy named %q in %s. The agent does not serve unguarded, "+
			"so it stays down until the policy exists.", name, ad.Namespace)
	case err != nil:
		d = s.unreadable(err, d, "guardrail policy")
	default:
		d = fromCondition(d, apimeta.FindStatusCondition(gp.Status.Conditions, conditionValidated),
			"NotValidated", "The controller has not validated this policy yet.")
	}
	return d, true
}

// toolBindingDependencies lists the MCPToolBindings that target the agent, each with its Ready condition.
func toolBindingDependencies(
	ad *agentsv1beta1.AgentDeployment,
	bindings []agentsv1beta1.MCPToolBinding,
	hidden bool,
) []AgentDependency {
	if hidden {
		return []AgentDependency{{
			Kind:      depKindToolBinding,
			Namespace: ad.Namespace,
			State:     depUnknown,
			Reason:    "Forbidden",
			Message: fmt.Sprintf("Your account can't list MCPToolBindings in %s, so the tools bound to this agent "+
				"are not shown.", ad.Namespace),
		}}
	}
	var deps []AgentDependency
	for i := range bindings {
		b := &bindings[i]
		if b.Spec.AgentRef != ad.Name {
			continue
		}
		d := AgentDependency{
			Kind:      depKindToolBinding,
			Name:      b.Name,
			Namespace: b.Namespace,
			Detail:    b.Spec.ToolName,
			Via:       b.Spec.RegistryRef,
		}
		deps = append(deps, fromCondition(d, apimeta.FindStatusCondition(b.Status.Conditions, conditionReadyType),
			reasonNotReconciled, "The controller has not resolved this binding yet."))
	}
	return deps
}

// knowledgeBaseDependencies resolves each spec.knowledgeBases ref. A KnowledgeBase that is ready still
// returns nothing when the agent's knowledge proxy is off (KnowledgeReady=False), so that is shown too.
func (s *Server) knowledgeBaseDependencies(
	ctx context.Context,
	caller client.Client,
	ad *agentsv1beta1.AgentDeployment,
) []AgentDependency {
	if len(ad.Spec.KnowledgeBases) == 0 {
		return nil
	}
	proxy := apimeta.FindStatusCondition(ad.Status.Conditions, conditionKnowledgeReady)
	deps := make([]AgentDependency, 0, len(ad.Spec.KnowledgeBases))
	for _, ref := range ad.Spec.KnowledgeBases {
		ns := ref.Namespace
		if ns == "" {
			ns = ad.Namespace
		}
		d := AgentDependency{Kind: depKindKnowledgeBase, Name: ref.Name, Namespace: ns, Detail: "tool-only"}
		if ref.AutoInject {
			d.Detail = "auto-inject"
		}
		var kb agentsv1beta1.KnowledgeBase
		err := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &kb)
		switch {
		case apierrors.IsNotFound(err):
			d.State, d.Reason = depMissing, "KnowledgeBaseNotFound"
			d.Message = fmt.Sprintf("There is no KnowledgeBase named %q in %s. The agent runs without it.", ref.Name, ns)
		case err != nil:
			d = s.unreadable(err, d, "knowledge base")
		default:
			d = knowledgeBaseState(d, &kb)
			if d.State == depReady && proxy != nil && proxy.Status != metav1.ConditionTrue {
				d.State, d.Reason, d.Message = depNotReady, proxy.Reason, proxy.Message
			}
		}
		deps = append(deps, d)
	}
	return deps
}

// knowledgeBaseState reads a KnowledgeBase's validation and ingestion phase.
func knowledgeBaseState(d AgentDependency, kb *agentsv1beta1.KnowledgeBase) AgentDependency {
	if c := apimeta.FindStatusCondition(kb.Status.Conditions, conditionValidated); c != nil &&
		c.Status != metav1.ConditionTrue {
		d.State, d.Reason, d.Message = depNotReady, c.Reason, c.Message
		return d
	}
	d.Reason = kb.Status.Phase
	switch kb.Status.Phase {
	case kbPhaseReady:
		d.State = depReady
	case kbPhasePartiallyIngested:
		d.State = depReady
		d.Message = "Queryable, but some documents were flagged during ingestion."
	case kbPhaseIngesting:
		// A re-ingest replaces a corpus that is still there, so it answers from the last completed run.
		if kb.Status.LastIngestedAt != nil {
			d.State, d.Message = depReady, "Re-ingesting now. Queries use the last completed ingestion until it finishes."
		} else {
			d.State, d.Message = depNotReady, "The first ingestion run is in progress."
		}
	case kbPhaseFailed:
		d.State, d.Message = depNotReady, "The last ingestion run failed."
	case kbPhaseBudgetExceeded:
		d.State, d.Message = depNotReady, "The last ingestion run was halted by the tenant budget."
	case kbPhasePending:
		// A refresh schedule starts the first ingestion on its own; without one, someone has to.
		if kb.Spec.RefreshInterval != nil {
			d.State, d.Message = depNotReady, "Nothing has been ingested yet. Its refresh schedule starts the first ingestion."
		} else {
			d.State, d.Reason = depNotReady, reasonNeedsIngest
			d.Message = "Nothing has been ingested yet, and no refresh schedule will start an ingestion, " +
				"so retrieval returns nothing until one is run."
		}
	default:
		d.State, d.Reason = depNotReady, reasonNotReconciled
		d.Message = "The controller has not reconciled this knowledge base yet."
	}
	return d
}

// memoryDependencies lists the agent's memory. Both are spec fields with no object to resolve.
func memoryDependencies(ad *agentsv1beta1.AgentDeployment) []AgentDependency {
	var deps []AgentDependency
	if sm := ad.Spec.SessionMemory; sm != nil {
		scope := sm.Scope
		if scope == "" {
			scope = sessionMemoryScopeSession
		}
		d := AgentDependency{Kind: depKindSessionMemory, Name: scope, State: depDeclared}
		if sm.PerUser {
			d.Detail = "per user"
		}
		deps = append(deps, d)
	}
	if lt := ad.Spec.LongTermMemory; lt != nil && lt.Enabled {
		d := AgentDependency{Kind: depKindLongTermMemory, Name: "long-term", State: depDeclared}
		parts := []string{}
		if lt.PerUser {
			parts = append(parts, "per user")
		}
		if lt.EmbeddingRoute != "" {
			parts = append(parts, "embeds with "+lt.EmbeddingRoute)
		}
		d.Detail = strings.Join(parts, ", ")
		deps = append(deps, d)
	}
	return deps
}

// approvalDependencies lists the tool-policy rules that pause a run for approval and who may approve
// (spec.runtime.toolPolicy, ADR 0152).
func approvalDependencies(ad *agentsv1beta1.AgentDeployment) []AgentDependency {
	if ad.Spec.Runtime == nil || ad.Spec.Runtime.ToolPolicy == nil {
		return nil
	}
	tp := ad.Spec.Runtime.ToolPolicy
	var deps []AgentDependency
	if tp.Default == toolRuleRequireApproval {
		deps = append(deps, AgentDependency{
			Kind: depKindApprovalRule, Name: "*", Detail: "every tool without its own rule", State: depDeclared,
		})
	}
	for _, o := range tp.Overrides {
		if o.Rule == toolRuleRequireApproval {
			deps = append(deps, AgentDependency{Kind: depKindApprovalRule, Name: o.Name, State: depDeclared})
		}
	}
	for _, a := range tp.Approvers {
		deps = append(deps, AgentDependency{Kind: depKindApprover, Name: a.Name, Detail: a.Kind, State: depDeclared})
	}
	return deps
}

// feedbackDependencies lists the agent's declared feedback sources (spec.feedback, ADR 0152).
func feedbackDependencies(ad *agentsv1beta1.AgentDeployment) []AgentDependency {
	fb := ad.Spec.Feedback
	if fb == nil {
		return nil
	}
	gate := "Enforce: a score whose name no source declares is rejected."
	if fb.Mode == agentsv1beta1.FeedbackMonitor {
		gate = "Monitor: a score whose name no source declares is still accepted."
	}
	var deps []AgentDependency
	if fb.Human != nil && len(fb.Human.Scores) > 0 {
		names := make([]string, 0, len(fb.Human.Scores))
		for _, sc := range fb.Human.Scores {
			names = append(names, sc.Name)
		}
		deps = append(deps, AgentDependency{
			Kind: depKindFeedbackSource, Name: "human", Detail: strings.Join(names, ", "), State: depDeclared, Message: gate,
		})
	}
	for _, e := range fb.External {
		deps = append(deps, AgentDependency{
			Kind: depKindFeedbackSource, Name: e.Name, Detail: e.Score.Name, State: depDeclared, Message: gate,
		})
	}
	return deps
}
