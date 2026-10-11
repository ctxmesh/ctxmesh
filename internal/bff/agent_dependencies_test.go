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
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

func cond(typ string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	c := readyCondition(status, reason, msg)
	c.Type = typ
	return c
}

// depsAgent references one of everything the Bindings panel lists.
func depsAgent() *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "img:1",
			Env:                []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "chat"}},
			GuardrailPolicyRef: "pii",
			KnowledgeBases: []agentsv1alpha1.KnowledgeBaseRef{
				{Name: "handbook", AutoInject: true},
			},
			SessionMemory:  &agentsv1alpha1.SessionMemorySpec{Scope: "session", PerUser: true},
			LongTermMemory: &agentsv1alpha1.LongTermMemorySpec{Enabled: true, EmbeddingRoute: "embed"},
			Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
				Default:   "allow",
				Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "refund", Rule: "require-approval"}, {Name: "search", Rule: "allow"}},
				Approvers: []agentsv1alpha1.Approver{{Kind: "Group", Name: "finance"}},
			}},
			Feedback: &agentsv1alpha1.FeedbackSpec{
				Human:    &agentsv1alpha1.HumanSource{Scores: []agentsv1alpha1.ScoreDecl{{Name: "thumbs"}, {Name: "accuracy"}}},
				External: []agentsv1alpha1.ExternalSource{{Name: "csat-webhook", Score: agentsv1alpha1.ScoreDecl{Name: "csat"}}},
			},
		},
	}
}

func servedRoute(secretRef string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: detailNS},
		Spec: agentsv1alpha1.ModelRouteSpec{Providers: []agentsv1alpha1.ProviderRef{
			{Provider: "openai", Model: "gpt", Priority: 1, SecretBindingRef: secretRef},
			{Provider: "openai", Model: "gpt-mini", Priority: 2, SecretBindingRef: secretRef},
		}},
		Status: agentsv1alpha1.ModelRouteStatus{Conditions: []metav1.Condition{
			readyCondition(metav1.ConditionTrue, "Served", "the gateway serves this route"),
		}},
	}
}

func depsByKind(t *testing.T, got []AgentDependency, kind string) []AgentDependency {
	t.Helper()
	var out []AgentDependency
	for _, d := range got {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func detailDeps(t *testing.T, c client.Client) []AgentDependency {
	t.Helper()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.Dependencies)
	return got.Dependencies
}

// TestAgentDependenciesResolved proves every kind the panel lists is resolved as the caller: the route
// and its (deduplicated) secret binding, the guardrail, the tool binding, the knowledge base, and the
// spec-only memory, approval and feedback declarations.
func TestAgentDependenciesResolved(t *testing.T) {
	sb := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "openai-key", Namespace: detailNS},
		Spec:       agentsv1alpha1.SecretBindingSpec{SecretRef: agentsv1alpha1.SecretKeyRef{Name: "openai", Key: "api-key"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: detailNS},
		Data:       map[string][]byte{"api-key": []byte("x")},
	}
	gp := &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pii", Namespace: detailNS},
		Status: agentsv1beta1.GuardrailPolicyStatus{Conditions: []metav1.Condition{
			cond("Validated", metav1.ConditionTrue, "AllPatternsCompile", ""),
		}},
	}
	tb := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-search", Namespace: detailNS},
		Spec:       agentsv1alpha1.MCPToolBindingSpec{AgentRef: "echo", ToolName: "search", RegistryRef: "crm", Mode: "remote"},
		Status:     agentsv1alpha1.MCPToolBindingStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionTrue, "Bound", "ok")}},
	}
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "handbook", Namespace: detailNS},
		Status:     agentsv1beta1.KnowledgeBaseStatus{Phase: "Ready"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(depsAgent(), servedRoute("openai-key"), sb, secret, gp, tb, kb).Build()

	got := detailDeps(t, c)

	route := depsByKind(t, got, depKindModelRoute)
	require.Len(t, route, 1)
	assert.Equal(t, "chat", route[0].Name)
	assert.Equal(t, depReady, route[0].State)
	assert.Equal(t, "Served", route[0].Reason)

	secrets := depsByKind(t, got, depKindSecretBinding)
	require.Len(t, secrets, 1, "two providers naming one binding is one dependency")
	assert.Equal(t, depReady, secrets[0].State)
	assert.Equal(t, "chat", secrets[0].Via)

	guard := depsByKind(t, got, depKindGuardrailPolicy)
	require.Len(t, guard, 1)
	assert.Equal(t, depReady, guard[0].State)

	tools := depsByKind(t, got, depKindToolBinding)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo-search", tools[0].Name)
	assert.Equal(t, "search", tools[0].Detail)
	assert.Equal(t, "crm", tools[0].Via)
	assert.Equal(t, depReady, tools[0].State)

	kbs := depsByKind(t, got, depKindKnowledgeBase)
	require.Len(t, kbs, 1)
	assert.Equal(t, depReady, kbs[0].State)
	assert.Equal(t, "auto-inject", kbs[0].Detail)

	session := depsByKind(t, got, depKindSessionMemory)
	require.Len(t, session, 1)
	assert.Equal(t, "session", session[0].Name)
	assert.Equal(t, "per user", session[0].Detail)
	assert.Equal(t, depDeclared, session[0].State)
	longTerm := depsByKind(t, got, depKindLongTermMemory)
	require.Len(t, longTerm, 1)
	assert.Contains(t, longTerm[0].Detail, "embed")

	rules := depsByKind(t, got, depKindApprovalRule)
	require.Len(t, rules, 1, "only require-approval rules are approval rules")
	assert.Equal(t, "refund", rules[0].Name)
	approvers := depsByKind(t, got, depKindApprover)
	require.Len(t, approvers, 1)
	assert.Equal(t, "finance", approvers[0].Name)
	assert.Equal(t, "Group", approvers[0].Detail)

	feedback := depsByKind(t, got, depKindFeedbackSource)
	require.Len(t, feedback, 2)
	assert.Equal(t, "human", feedback[0].Name)
	assert.Equal(t, "thumbs, accuracy", feedback[0].Detail)
	assert.Equal(t, "csat-webhook", feedback[1].Name)
	assert.Equal(t, "csat", feedback[1].Detail)
	assert.Contains(t, feedback[0].Message, "Enforce")
}

// TestAgentDependenciesMissingAreListed proves a reference that does not resolve is listed as missing,
// with the reason, rather than dropped.
func TestAgentDependenciesMissingAreListed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(depsAgent()).Build()

	got := detailDeps(t, c)

	for _, kind := range []string{depKindModelRoute, depKindGuardrailPolicy, depKindKnowledgeBase} {
		ds := depsByKind(t, got, kind)
		require.Len(t, ds, 1, kind)
		assert.Equal(t, depMissing, ds[0].State, kind)
		assert.NotEmpty(t, ds[0].Reason, kind)
		assert.NotEmpty(t, ds[0].Message, kind)
	}
	assert.Equal(t, "GuardrailPolicyNotFound", depsByKind(t, got, depKindGuardrailPolicy)[0].Reason)
	assert.Empty(t, depsByKind(t, got, depKindSecretBinding), "an unreadable route names no secret bindings")
	assert.Empty(t, depsByKind(t, got, depKindToolBinding), "no binding targets the agent")
}

// TestAgentDependenciesNotReadyCarryTheConditionsWords proves a found-but-not-ready reference carries
// its own condition's reason and message.
func TestAgentDependenciesNotReadyCarryTheConditionsWords(t *testing.T) {
	route := servedRoute("missing-binding")
	route.Status.Conditions = []metav1.Condition{
		readyCondition(metav1.ConditionFalse, "NotYetServed", "route rendered; the gateway has not started serving it yet"),
	}
	gp := &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pii", Namespace: detailNS},
		Status: agentsv1beta1.GuardrailPolicyStatus{Conditions: []metav1.Condition{
			cond("Validated", metav1.ConditionFalse, "InvalidPattern", "pattern 0 does not compile"),
		}},
	}
	tb := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-search", Namespace: detailNS},
		Spec:       agentsv1alpha1.MCPToolBindingSpec{AgentRef: "echo", ToolName: "search", RegistryRef: "crm", Mode: "remote"},
		Status: agentsv1alpha1.MCPToolBindingStatus{Conditions: []metav1.Condition{
			readyCondition(metav1.ConditionFalse, "RegistryNotFound", "ToolRegistry crm not found"),
		}},
	}
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "handbook", Namespace: detailNS},
		Status:     agentsv1beta1.KnowledgeBaseStatus{Phase: "Ready"},
	}
	ad := depsAgent()
	ad.Status.Conditions = []metav1.Condition{
		cond("KnowledgeReady", metav1.ConditionFalse, "TokenServiceUnavailable", "the knowledge proxy will be disabled"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad, route, gp, tb, kb).Build()

	got := detailDeps(t, c)

	r := depsByKind(t, got, depKindModelRoute)[0]
	assert.Equal(t, depNotReady, r.State)
	assert.Equal(t, "NotYetServed", r.Reason)
	assert.Contains(t, r.Message, "has not started serving")

	sb := depsByKind(t, got, depKindSecretBinding)
	require.Len(t, sb, 1)
	assert.Equal(t, depMissing, sb[0].State)
	assert.Equal(t, "SecretBindingNotFound", sb[0].Reason)

	g := depsByKind(t, got, depKindGuardrailPolicy)[0]
	assert.Equal(t, depNotReady, g.State)
	assert.Equal(t, "InvalidPattern", g.Reason)

	tool := depsByKind(t, got, depKindToolBinding)[0]
	assert.Equal(t, depNotReady, tool.State)
	assert.Equal(t, "RegistryNotFound", tool.Reason)

	k := depsByKind(t, got, depKindKnowledgeBase)[0]
	assert.Equal(t, depNotReady, k.State, "a ready KB is not reachable while the agent's knowledge proxy is off")
	assert.Equal(t, "TokenServiceUnavailable", k.Reason)
}

// TestAgentDependenciesForbiddenReadsAreUnknown proves a reference the caller may not read is reported
// as unknown, never as missing, and that the model route falls back to the controller's condition.
func TestAgentDependenciesForbiddenReadsAreUnknown(t *testing.T) {
	ad := depsAgent()
	ad.Status.Conditions = []metav1.Condition{
		cond("ModelRouteReady", metav1.ConditionFalse, "NotYetServed", "ModelRoute chat: not served yet"),
	}
	denied := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: resource}, "", errors.New("denied"))
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *agentsv1alpha1.ModelRoute:
					return denied("modelroutes")
				case *agentsv1beta1.GuardrailPolicy:
					return denied("guardrailpolicies")
				case *agentsv1beta1.KnowledgeBase:
					return errors.New("etcd timeout")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*agentsv1alpha1.MCPToolBindingList); ok {
					return denied("mcptoolbindings")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	got := detailDeps(t, c)

	r := depsByKind(t, got, depKindModelRoute)[0]
	assert.Equal(t, depNotReady, r.State, "the controller's ModelRouteReady stands in for an unreadable route")
	assert.Equal(t, "NotYetServed", r.Reason)

	g := depsByKind(t, got, depKindGuardrailPolicy)[0]
	assert.Equal(t, depUnknown, g.State)
	assert.Equal(t, "Forbidden", g.Reason)

	k := depsByKind(t, got, depKindKnowledgeBase)[0]
	assert.Equal(t, depUnknown, k.State, "a failed read is not evidence of absence")
	assert.Equal(t, "ReadFailed", k.Reason)

	tools := depsByKind(t, got, depKindToolBinding)
	require.Len(t, tools, 1)
	assert.Equal(t, depUnknown, tools[0].State, "a denied binding list is reported, not shown as no tools")
}

// TestAgentDependenciesNoneIsEmptyList proves an agent that references nothing gets [] (not null).
func TestAgentDependenciesNoneIsEmptyList(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	assert.Empty(t, detailDeps(t, c))
}
