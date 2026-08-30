package bff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

func mkGuardrailPolicy(name string, piiEnabled, judgeEnabled, userRateLimited, validated bool, failMode string, denylistCount int, referencingAgents []string) *agentsv1beta1.GuardrailPolicy {
	gp := &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agentsv1beta1.GuardrailPolicySpec{
			FailMode:        failMode,
			PatternDenylist: make([]agentsv1beta1.PatternRule, denylistCount),
		},
	}
	if piiEnabled {
		gp.Spec.PIIDetectors = &agentsv1beta1.PIIGuardrail{Action: "redact"}
	}
	if judgeEnabled {
		gp.Spec.SemanticJudge = &agentsv1beta1.SemanticJudge{Enabled: true, ModelRoute: "claude-haiku"}
	}
	if userRateLimited {
		gp.Spec.UserRateLimit = &agentsv1beta1.UserRateLimit{RequestsPerMinute: 10}
	}

	status := metav1.ConditionFalse
	reason := "InvalidPattern"
	if validated {
		status = metav1.ConditionTrue
		reason = "Validated"
	}
	gp.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: status, Reason: reason}}
	gp.Status.PolicyHash = "abc123"
	if referencingAgents != nil {
		gp.Status.ReferencingAgents = referencingAgents
	}
	return gp
}

func getGuardrailPolicies(t *testing.T, s *Server, rawQuery string) (GuardrailPolicyListResponse, int) {
	t.Helper()
	url := "/api/guardrailpolicies"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body GuardrailPolicyListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func TestListGuardrailPolicies(t *testing.T) {
	objs := []client.Object{
		mkGuardrailPolicy("strict-policy", true, true, true, true, "closed", 3, []string{"echo-agent", "chat-agent"}),
		mkGuardrailPolicy("open-policy", false, false, false, false, "open", 0, nil),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getGuardrailPolicies(t, s, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	byName := map[string]GuardrailPolicySummary{}
	for _, it := range body.Items {
		byName[it.Name] = it
	}

	strict := byName["strict-policy"]
	assert.Equal(t, "default", strict.Namespace)
	assert.True(t, strict.PIIEnabled, "piiEnabled surfaced from spec.piiDetectors")
	assert.True(t, strict.JudgeEnabled, "judgeEnabled surfaced from spec.semanticJudge.enabled")
	assert.True(t, strict.UserRateLimited, "userRateLimited surfaced from spec.userRateLimit")
	assert.True(t, strict.Validated, "validated surfaced from the Validated condition")
	assert.Equal(t, "closed", strict.FailMode)
	assert.Equal(t, 3, strict.DenylistCount)
	assert.Equal(t, "abc123", strict.PolicyHash)
	assert.Equal(t, []string{"echo-agent", "chat-agent"}, strict.ReferencingAgents, "blast-radius agents surfaced")

	open := byName["open-policy"]
	assert.False(t, open.PIIEnabled)
	assert.False(t, open.JudgeEnabled)
	assert.False(t, open.Validated)
	assert.Equal(t, "InvalidPattern", open.Reason)
	assert.Equal(t, "open", open.FailMode)
	assert.Equal(t, 0, open.DenylistCount)
	assert.Equal(t, []string{}, open.ReferencingAgents, "a nil status.referencingAgents surfaces as empty slice, not null")
}

func TestListGuardrailPolicies_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getGuardrailPolicies(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, body.Items, "no policies ⇒ an empty [] items list, not null")
}

func TestListGuardrailPolicies_FilterByName(t *testing.T) {
	objs := []client.Object{
		mkGuardrailPolicy("strict-policy", true, false, false, true, "closed", 0, nil),
		mkGuardrailPolicy("lenient-policy", false, false, false, true, "open", 0, nil),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getGuardrailPolicies(t, s, "q=strict")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "strict-policy", body.Items[0].Name)
}

func TestListGuardrailPolicies_DefaultFailModeClosed(t *testing.T) {
	// When spec.failMode is "" (absent/defaulted), the BFF surfaces "closed" (the CRD default).
	gp := &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-mode", Namespace: "default"},
		Spec:       agentsv1beta1.GuardrailPolicySpec{}, // failMode absent
	}
	gp.Status.ReferencingAgents = []string{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gp).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getGuardrailPolicies(t, s, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "closed", body.Items[0].FailMode, "absent failMode must surface as 'closed' (the CRD default)")
}
