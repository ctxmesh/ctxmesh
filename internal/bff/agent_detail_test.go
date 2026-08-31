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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/publishedartifact"
)

// readyCondition is a terse "Ready" condition fixture for the detail tests.
func readyCondition(status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)),
	}
}

// detailNS is the namespace every detail/logs fixture lives in.
const detailNS = "team-a"

// getDetail drives GET /api/agents/team-a/{name} against a caller-scoped server
// and returns the recorder.
func getDetail(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+detailNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAgentDetailHappyPath proves the detail DTO carries the conditions, the
// Knative URL, readiness/phase, the bindings referencing THIS agent (tool +
// memory), and the version history — with []-not-null slices — read caller-scoped.
func TestAgentDetailHappyPath(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "img:1",
			ExecutionModel: "serving",
			Role:           "worker",
			PromptRef:      "system-prompt-v2",
			Env:            []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "anthropic"}},
			Scaling:        &agentsv1alpha1.ScalingSpec{Min: 1, Max: 5},
			// Session memory is the folded spec field (ADR 0101 — MemoryBinding retired):
			// the detail projection surfaces it as a "memory" binding row.
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{Scope: "session"},
		},
		Status: agentsv1alpha1.AgentDeploymentStatus{
			Conditions:    []metav1.Condition{readyCondition(metav1.ConditionTrue, "Deployed", "serving")},
			URL:           "http://echo.team-a.example",
			LatestVersion: "echo-abc123",
		},
	}
	// A tool binding referencing echo, plus a decoy binding for a different agent
	// that must NOT appear. (Memory comes from ad.Spec.SessionMemory above.)
	toolBinding := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-search", Namespace: "team-a"},
		Spec:       agentsv1alpha1.MCPToolBindingSpec{AgentRef: "echo", ToolName: "search", RegistryRef: "reg", Mode: "remote", Server: agentsv1alpha1.ToolServer{URL: "http://x"}},
		Status:     agentsv1alpha1.MCPToolBindingStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionTrue, "Bound", "ok")}},
	}
	decoyBinding := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "other-tool", Namespace: "team-a"},
		Spec:       agentsv1alpha1.MCPToolBindingSpec{AgentRef: "other", ToolName: "nope", RegistryRef: "reg", Mode: "remote", Server: agentsv1alpha1.ToolServer{URL: "http://y"}},
	}
	version := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-abc123", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentVersionSpec{DeploymentName: "echo", Snapshot: agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"}},
	}
	decoyVersion := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "other-v1", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentVersionSpec{DeploymentName: "other", Snapshot: agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"}},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(ad, toolBinding, decoyBinding, version, decoyVersion).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	assert.Equal(t, "echo", got.Name)
	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "img:1", got.Image)
	assert.Equal(t, "serving", got.ExecutionModel)
	assert.Equal(t, "worker", got.Role)
	assert.Equal(t, "system-prompt-v2", got.PromptRef, "promptRef surfaced for the used-by link")
	assert.Equal(t, "anthropic", got.ModelRoute, "modelRoute surfaced from the MODEL_ROUTE env")
	assert.Equal(t, int32(1), got.Scaling.Min)
	assert.Equal(t, int32(5), got.Scaling.Max)
	assert.True(t, got.Ready)
	assert.Equal(t, phaseReady, got.Phase)
	assert.Equal(t, "http://echo.team-a.example", got.URL)
	assert.Equal(t, "echo-abc123", got.LatestVersion)

	require.Len(t, got.Conditions, 1)
	assert.Equal(t, "Ready", got.Conditions[0].Type)
	assert.Equal(t, "True", got.Conditions[0].Status)
	assert.Equal(t, "Deployed", got.Conditions[0].Reason)

	// Bindings: echo's tool binding + folded session memory, not the decoy.
	require.Len(t, got.Bindings, 2)
	kinds := map[string]AgentBinding{}
	for _, b := range got.Bindings {
		kinds[b.Kind] = b
	}
	require.Contains(t, kinds, "tool")
	require.Contains(t, kinds, "memory")
	assert.Equal(t, "search", kinds["tool"].Detail)
	assert.True(t, kinds["tool"].Ready)
	assert.Equal(t, "session", kinds["memory"].Detail)
	for _, b := range got.Bindings {
		assert.NotEqual(t, "other-tool", b.Name, "decoy binding for a different agent leaked")
	}

	// Versions: only echo's version.
	assert.Equal(t, []string{"echo-abc123"}, got.Versions)
}

// TestAgentDetailForkNeedsRebinding proves the fork needs-rebinding state is forwarded as explicit
// fields (U14, m101.4) — the banner previously keyed on a `labels` map the BFF never sent (dead in
// prod). NeedsRebinding comes from the label; ForkUnresolvedRefs is parsed from the annotation.
func TestAgentDetailForkNeedsRebinding(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "forked",
			Namespace: detailNS,
			Labels:    map[string]string{labelForkNeedsRebinding: labelValueTrue},
			Annotations: map[string]string{
				annForkUnresolvedRefs: `["model route: gpt4","prompt: greeting"]`,
			},
		},
		Spec:   agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
		Status: agentsv1alpha1.AgentDeploymentStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionFalse, "NotReady", "pending")}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "forked")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.NeedsRebinding, "the fork label must project to NeedsRebinding=true")
	assert.Equal(t, []string{"model route: gpt4", "prompt: greeting"}, got.ForkUnresolvedRefs,
		"the annotation must project to the itemized ref list")
}

// TestAgentDetailNoForkStateIsClean proves a normal agent carries no needs-rebinding state.
func TestAgentDetailNoForkStateIsClean(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "clean", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionTrue, "Deployed", "ok")}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "clean")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.NeedsRebinding)
	assert.Empty(t, got.ForkUnresolvedRefs)
}

// TestAgentDetailPublishedState proves the durable published-template state is projected onto the
// detail (U13, m101.4) — so the badge survives a reload (it was in-session only).
func TestAgentDetailPublishedState(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionTrue, "Deployed", "ok")}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "echo",
		SpecJSON: []byte(`{"name":"echo"}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	s.publishedArtifactStore = store

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.Published, "a published agent must carry durable published state")
	assert.Equal(t, "org", got.Published.Visibility)
	assert.Equal(t, 1, got.Published.Version)
}

// TestAgentDetailNotPublishedIsNil proves an unpublished agent carries no published state.
func TestAgentDetailNotPublishedIsNil(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: []metav1.Condition{readyCondition(metav1.ConditionTrue, "Deployed", "ok")}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.publishedArtifactStore = publishedartifact.NewMemStore() // empty

	rec := getDetail(t, s, "solo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Nil(t, got.Published)
}

// TestAgentDetailNotFoundIs404 proves a missing agent surfaces as 404.
func TestAgentDetailNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAgentDetailForbiddenAgentIs403 proves a Forbidden on the AGENT read is fatal
// (403) — a caller who cannot read the agent sees an honest denial, not a body.
func TestAgentDetailForbiddenAgentIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, key.Name, errors.New("viewer denied"),
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAgentDetailForbiddenBindingsIsDegraded proves a Forbidden on the BINDINGS
// list is degraded (200 with empty bindings), not fatal — the caller can read the
// agent, so hiding it behind a binding-RBAC 403 would be worse than a partial view.
func TestAgentDetailForbiddenBindingsIsDegraded(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).
		WithInterceptorFuncs(interceptor.Funcs{
			// The agent Get succeeds; the binding/version LISTs are forbidden.
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "mcptoolbindings"}, "", errors.New("viewer denied"),
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code, "a binding-list forbidden must not hide the agent")

	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// []-not-null even when the lists were denied.
	assert.NotNil(t, got.Bindings)
	assert.NotNil(t, got.Versions)
	assert.Empty(t, got.Bindings)
	assert.Empty(t, got.Versions)
}

// TestAgentDetailScalingDefault proves a nil spec.Scaling projects the CRD default
// (min=0, max=3) so the page never shows a bare 0/0.
func TestAgentDetailScalingDefault(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, int32(0), got.Scaling.Min)
	assert.Equal(t, int32(3), got.Scaling.Max)
	// A pending agent (no Ready condition) → not ready, Pending phase.
	assert.False(t, got.Ready)
	assert.Equal(t, phasePending, got.Phase)
	assert.Empty(t, got.Conditions)
	assert.NotNil(t, got.Conditions)
}

// TestAgentDetailRuntimeProjected proves that spec.runtime is projected onto the
// detail DTO when present: outputSchema, toolPolicy (default + overrides +
// parallelLimit), and resilience (modelCall + toolCall + circuitBreaker).
func TestAgentDetailRuntimeProjected(t *testing.T) {
	schemaRaw := []byte(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "img:1",
			Runtime: &agentsv1alpha1.RuntimeSpec{
				OutputSchema: &k8sruntime.RawExtension{Raw: schemaRaw},
				ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
					Default: "allow",
					Overrides: []agentsv1alpha1.ToolPolicyOverride{
						{Name: "send_email", Rule: "require-approval", Retryable: false},
					},
					ForcedChoice:  "auto",
					ParallelLimit: 4,
				},
				Resilience: &agentsv1alpha1.ResilienceSpec{
					ModelCall: &agentsv1alpha1.CallResilience{TimeoutSeconds: 30, MaxRetries: 2},
					ToolCall: &agentsv1alpha1.ToolCallResilience{
						TimeoutSeconds: 10,
						MaxRetries:     1,
						CircuitBreaker: &agentsv1alpha1.CircuitBreakerSpec{FailureThreshold: 5, CooldownSeconds: 60},
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	require.NotNil(t, got.Runtime, "runtime must be present when spec.runtime is set")
	assert.True(t, got.Runtime.OutputSchemaSet, "outputSchemaSet must be true when outputSchema is set")
	assert.JSONEq(t, string(schemaRaw), got.Runtime.OutputSchema, "outputSchema JSON must round-trip verbatim")

	require.NotNil(t, got.Runtime.ToolPolicy, "toolPolicy must be projected")
	assert.Equal(t, "allow", got.Runtime.ToolPolicy.Default)
	require.Len(t, got.Runtime.ToolPolicy.Overrides, 1)
	assert.Equal(t, "send_email", got.Runtime.ToolPolicy.Overrides[0].Name)
	assert.Equal(t, "require-approval", got.Runtime.ToolPolicy.Overrides[0].Rule)
	assert.Equal(t, "auto", got.Runtime.ToolPolicy.ForcedChoice)
	assert.Equal(t, int32(4), got.Runtime.ToolPolicy.ParallelLimit)

	require.NotNil(t, got.Runtime.Resilience, "resilience must be projected")
	require.NotNil(t, got.Runtime.Resilience.ModelCall)
	assert.Equal(t, int32(30), got.Runtime.Resilience.ModelCall.TimeoutSeconds)
	assert.Equal(t, int32(2), got.Runtime.Resilience.ModelCall.MaxRetries)
	require.NotNil(t, got.Runtime.Resilience.ToolCall)
	assert.Equal(t, int32(10), got.Runtime.Resilience.ToolCall.TimeoutSeconds)
	assert.Equal(t, int32(1), got.Runtime.Resilience.ToolCall.MaxRetries)
	require.NotNil(t, got.Runtime.Resilience.ToolCall.CircuitBreaker)
	assert.Equal(t, int32(5), got.Runtime.Resilience.ToolCall.CircuitBreaker.FailureThreshold)
	assert.Equal(t, int32(60), got.Runtime.Resilience.ToolCall.CircuitBreaker.CooldownSeconds)
}

// TestAgentDetailRuntimeAbsentIsNil proves that when spec.runtime is absent the
// JSON field is omitted entirely (nil, not an empty object) so agents without a
// runtime config don't produce noise on the wire.
func TestAgentDetailRuntimeAbsentIsNil(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Nil(t, got.Runtime, "runtime must be nil (omitted) when spec.runtime is absent")
	// Confirm the field is truly absent on the wire, not serialized as null.
	assert.NotContains(t, rec.Body.String(), `"runtime"`, "the runtime key must not appear in the JSON when absent")
}

// --- Logs (SSE pod-log tail) -------------------------------------------------

// fakePodLogAccessor is the PodLogAccessor test double: it returns preset pods and
// a preset log stream (or a preset error) so the SSE handler is exercised without a
// real cluster. It records the last request so a test can assert the follow/tail
// flags and the label selector flowed through.
type fakePodLogAccessor struct {
	pods        []corev1.Pod
	listErr     error
	stream      io.ReadCloser
	streamErr   error
	gotFollow   bool
	gotTail     *int64
	gotPod      string
	gotSelector string
}

func (a *fakePodLogAccessor) ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error) {
	a.gotSelector = labelSelector
	if a.listErr != nil {
		return nil, a.listErr
	}
	return &corev1.PodList{Items: a.pods}, nil
}

func (a *fakePodLogAccessor) StreamPodLog(ctx context.Context, namespace, pod, container string, follow bool, tailLines *int64) (io.ReadCloser, error) {
	a.gotPod = pod
	a.gotFollow = follow
	a.gotTail = tailLines
	if a.streamErr != nil {
		return nil, a.streamErr
	}
	return a.stream, nil
}

// runningPod is a terse Running-pod fixture backing the "echo" agent by Knative
// label, created at the given time.
func runningPod(name string, created time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         detailNS,
			Labels:            map[string]string{knativeServiceLabel: "echo"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// getLogs drives GET /api/agents/team-a/echo/logs with the given query and header,
// returning the recorder. auth controls whether a bearer token is attached.
func getLogs(t *testing.T, s *Server, query string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/agents/" + detailNS + "/echo/logs"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer caller-token")
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAgentLogsStreamsLinesAsSSE proves the tail streams each log line as an SSE
// "log" event, in order, and ends with a terminal "end" event (a clean close, not
// a hang). It also asserts follow=false + the bounded tail flowed to the stream.
func TestAgentLogsStreamsLinesAsSSE(t *testing.T) {
	acc := &fakePodLogAccessor{
		pods:   []corev1.Pod{runningPod("echo-xyz", time.Now())},
		stream: io.NopCloser(strings.NewReader("line one\nline two\nline three\n")),
	}
	s := newCallerServer(t, &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), podLogs: acc})

	rec := getLogs(t, s, "follow=false&tailLines=50", true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	body := rec.Body.String()
	assert.Contains(t, body, "event: log\ndata: line one\n")
	assert.Contains(t, body, "event: log\ndata: line two\n")
	assert.Contains(t, body, "event: log\ndata: line three\n")
	// The stream closed cleanly with a terminal event, never left hanging.
	assert.Contains(t, body, "event: end\n")
	// The order is preserved.
	assert.Less(t, strings.Index(body, "line one"), strings.Index(body, "line two"))

	// The caller-scoped label selector resolves the agent's pods.
	assert.Equal(t, knativeServiceLabel+"=echo", acc.gotSelector)
	assert.False(t, acc.gotFollow, "follow=false must reach the stream")
	require.NotNil(t, acc.gotTail)
	assert.Equal(t, int64(50), *acc.gotTail, "the bounded tail must reach the stream")
	assert.Equal(t, "echo-xyz", acc.gotPod)
}

// TestAgentLogsFollowFlag proves follow=true reaches the stream (the live tail).
func TestAgentLogsFollowFlag(t *testing.T) {
	acc := &fakePodLogAccessor{
		pods:   []corev1.Pod{runningPod("echo-xyz", time.Now())},
		stream: io.NopCloser(strings.NewReader("streaming\n")),
	}
	s := newCallerServer(t, &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), podLogs: acc})

	rec := getLogs(t, s, "follow=true", true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, acc.gotFollow, "follow=true must reach the stream")
}

// TestAgentLogsForbiddenPodsListIs403 proves a caller who cannot LIST pods gets an
// HTTP 403 BEFORE the SSE stream opens (an error status, never a hanging stream).
func TestAgentLogsForbiddenPodsListIs403(t *testing.T) {
	acc := &fakePodLogAccessor{
		listErr: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("viewer denied")),
	}
	s := newCallerServer(t, &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), podLogs: acc})

	rec := getLogs(t, s, "", true)
	require.Equal(t, http.StatusForbidden, rec.Code)
	// It is a real HTTP error body, NOT an SSE stream.
	assert.NotEqual(t, "text/event-stream", rec.Header().Get("Content-Type"))
}

// TestAgentLogsForbiddenPodLogIsSSEError proves a caller who can LIST pods but
// cannot read pods/log gets the 403 SURFACED as an SSE "error" event (the SSE
// headers are already committed) — never a silent hang or a 500.
func TestAgentLogsForbiddenPodLogIsSSEError(t *testing.T) {
	acc := &fakePodLogAccessor{
		pods:      []corev1.Pod{runningPod("echo-xyz", time.Now())},
		streamErr: apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "echo-xyz", errors.New("no log access")),
	}
	s := newCallerServer(t, &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), podLogs: acc})

	rec := getLogs(t, s, "", true)
	// The SSE stream had already started (200), so the denial is an SSE error event.
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	assert.Contains(t, body, "event: error\n")
	assert.Contains(t, strings.ToLower(body), "forbidden")
}

// TestAgentLogsPodNotRunningIsWaiting proves an agent with no pod yet (still
// starting / scaled to zero) yields a graceful "waiting" SSE event and a clean
// close — NOT a 500.
func TestAgentLogsPodNotRunningIsWaiting(t *testing.T) {
	acc := &fakePodLogAccessor{pods: nil} // no pods at all
	s := newCallerServer(t, &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), podLogs: acc})

	rec := getLogs(t, s, "", true)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "event: waiting\n")
	// It never tried to open a log stream (no pod resolved).
	assert.Empty(t, acc.gotPod)
}

// TestAgentLogsAnonIs401 proves a request with no bearer token is rejected 401
// BEFORE any K8s call — the caller-scoped seam never falls back to the BFF SA.
func TestAgentLogsAnonIs401(t *testing.T) {
	acc := &fakePodLogAccessor{pods: []corev1.Pod{runningPod("echo-xyz", time.Now())}}
	s := newCallerServer(t, &fakeCallerClientFactory{
		client:       fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		podLogs:      acc,
		requireToken: true,
	})

	rec := getLogs(t, s, "", false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, acc.gotSelector, "no K8s call must happen for an anon request")
}

// TestSelectActivePod proves the pod picker prefers a Running pod, then the newest,
// with a deterministic name tie-break, and returns "" for no pods.
func TestSelectActivePod(t *testing.T) {
	base := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	pending := runningPod("pending", base.Add(time.Hour))
	pending.Status.Phase = corev1.PodPending
	older := runningPod("run-old", base)
	newer := runningPod("run-new", base.Add(2*time.Hour))

	// Running beats a newer Pending.
	assert.Equal(t, "run-old", selectActivePod([]corev1.Pod{pending, older}))
	// Between two Running, the newer wins.
	assert.Equal(t, "run-new", selectActivePod([]corev1.Pod{older, newer}))
	// No pods → "".
	assert.Equal(t, "", selectActivePod(nil))
}

// TestParseTailLines proves the bound: default when absent/invalid, honored when in
// range, clamped when above the cap.
func TestParseTailLines(t *testing.T) {
	assert.Equal(t, int64(defaultLogTailLines), *parseTailLines(""))
	assert.Equal(t, int64(defaultLogTailLines), *parseTailLines("abc"))
	assert.Equal(t, int64(defaultLogTailLines), *parseTailLines("0"))
	assert.Equal(t, int64(100), *parseTailLines("100"))
	assert.Equal(t, int64(maxLogTailLines), *parseTailLines("999999"))
}

// --- Per-agent runs (GET /api/agents/{ns}/{name}/runs, m15.9) ----------------

// getRuns drives GET /api/agents/{ns}/{name}/runs against a server and returns the
// recorder. It sends a bearer token so the caller-scoped Get runs as a caller.
func getRuns(t *testing.T, s *Server, ns, name, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	u := "/api/agents/" + ns + "/" + name + "/runs"
	if query != "" {
		u += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// agentFixture is a minimal AgentDeployment the caller can Get for the runs route.
func agentFixture(ns, name string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
}

// TestAgentRunsCrossNamespaceIsolation is the cross-namespace correctness property
// AT THE HANDLER: default/foo and other/foo both exist; the runs of default/foo must
// contain ONLY default/foo's runs, never other/foo's — even though they share a name.
func TestAgentRunsCrossNamespaceIsolation(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(agentFixture("default", "foo"), agentFixture("other", "foo")).Build()
	lf := fakeLangfuseAdapter{agentRuns: map[string][]RunSummary{
		"default/foo": {{TraceID: "d1", Name: "run"}, {TraceID: "d2", Name: "run"}},
		"other/foo":   {{TraceID: "o1", Name: "run"}},
	}}
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{Langfuse: lf})

	rec := getRuns(t, s, "default", "foo", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentRunsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "default", got.Namespace)
	assert.Equal(t, "foo", got.Name)
	require.Len(t, got.Runs, 2)
	ids := []string{got.Runs[0].TraceID, got.Runs[1].TraceID}
	assert.ElementsMatch(t, []string{"d1", "d2"}, ids)
	for _, r := range got.Runs {
		assert.NotEqual(t, "o1", r.TraceID, "other/foo's run leaked into default/foo's list")
	}

	// The sibling agent's list is its OWN single run — proving the isolation is real.
	rec = getRuns(t, s, "other", "foo", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Runs, 1)
	assert.Equal(t, "o1", got.Runs[0].TraceID)
}

// TestAgentRunsLimitHonored proves ?limit bounds the returned list. It uses a
// DIFFERENT agent name than the other cases to prove the fixture/handler are not
// coupled to a single name.
func TestAgentRunsLimitHonored(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentFixture("default", "bar")).Build()
	lf := fakeLangfuseAdapter{agentRuns: map[string][]RunSummary{
		"default/bar": {{TraceID: "a"}, {TraceID: "b"}, {TraceID: "c"}},
	}}
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{Langfuse: lf})

	rec := getRuns(t, s, "default", "bar", "limit=2")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentRunsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Runs, 2, "the ?limit must bound the run list")
}

// TestAgentRunsEmptyIsNonNullJSON: an agent with no runs serializes as [] not null.
func TestAgentRunsEmptyIsNonNullJSON(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentFixture("default", "foo")).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c},
		Adapters{Langfuse: fakeLangfuseAdapter{agentRuns: map[string][]RunSummary{}}})

	rec := getRuns(t, s, "default", "foo", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"runs":[]`, "runs must be [] not null on the wire")
}

// TestAgentRunsForbiddenAgentIs403: a caller who cannot `get` the agent gets an
// honest 403 — and no runs are fetched (the existence gate runs FIRST).
func TestAgentRunsForbiddenAgentIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, key.Name, errors.New("viewer denied"),
				)
			},
		}).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c},
		Adapters{Langfuse: fakeLangfuseAdapter{agentRuns: map[string][]RunSummary{
			"default/foo": {{TraceID: "should-not-be-returned"}},
		}}})

	rec := getRuns(t, s, "default", "foo", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "should-not-be-returned",
		"no run metadata may be returned for an agent the caller cannot read")
}

// TestAgentRunsNotFoundIs404: a missing agent surfaces as 404 (not an empty 200).
func TestAgentRunsNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c},
		Adapters{Langfuse: fakeLangfuseAdapter{}})

	rec := getRuns(t, s, "default", "ghost", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAgentRunsUpstreamErrorIs502: a Langfuse failure degrades to 502, never a 500.
func TestAgentRunsUpstreamErrorIs502(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentFixture("default", "foo")).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c},
		Adapters{Langfuse: fakeLangfuseAdapter{agentRunsErr: assert.AnError}})

	rec := getRuns(t, s, "default", "foo", "")
	assert.Equal(t, http.StatusBadGateway, rec.Code, "an upstream Langfuse failure is a 502, never a 500")
}

// TestAgentRunsLangfuseAbsentIs501: when the Langfuse adapter is not wired the route
// serves an honest 501 (the m14.8 degrade), never a 500 or a fabricated empty list.
func TestAgentRunsLangfuseAbsentIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentFixture("default", "foo")).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{}) // no Langfuse

	rec := getRuns(t, s, "default", "foo", "")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestAgentRunsAnonIs401: no bearer token → 401 before any K8s or Langfuse call.
func TestAgentRunsAnonIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentFixture("default", "foo")).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c, requireToken: true},
		Adapters{Langfuse: fakeLangfuseAdapter{}})

	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/default/foo/runs", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
