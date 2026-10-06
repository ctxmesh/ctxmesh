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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// recordedScore captures one CreateScore relay so the write-path tests can assert what (if anything) was
// sent to Langfuse.
type recordedScore struct {
	traceID, name, comment string
	value                  float64
}

// recordingLangfuse embeds the read-path fake (TraceScores + the no-op stubs) and records CreateScore.
type recordingLangfuse struct {
	fakeLangfuseAdapter
	created *[]recordedScore
}

func (f recordingLangfuse) CreateScore(_ context.Context, traceID, name string, value float64, comment string) error {
	*f.created = append(*f.created, recordedScore{traceID, name, comment, value})
	return nil
}

// feedbackTestServer builds a BFF server with a permissive caller client seeded with the given objects and
// the given Langfuse adapter. Mirrors serverWithAdapters but lets a test seed the agent (with or without
// spec.feedback).
func feedbackTestServer(t *testing.T, adapter LangfuseAdapter, objs ...client.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Langfuse: adapter},
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// agentWithFeedback is the inspector test agent declaring the given spec.feedback (nil = no declaration).
func agentWithFeedback(spec *agentsv1alpha1.FeedbackSpec) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: inspectorTestAgentName, Namespace: inspectorTestAgentNs},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Feedback: spec},
	}
}

// feedbackFixture declares a human "thumbs" score + an external "csat-webhook" channel writing "csat".
func feedbackFixture(mode agentsv1alpha1.FeedbackMode) *agentsv1alpha1.FeedbackSpec {
	return &agentsv1alpha1.FeedbackSpec{
		Mode:  mode,
		Human: &agentsv1alpha1.HumanSource{Scores: []agentsv1alpha1.ScoreDecl{{Name: "thumbs", DataType: agentsv1alpha1.ScoreBoolean}}},
		External: []agentsv1alpha1.ExternalSource{
			{Name: "csat-webhook", Score: agentsv1alpha1.ScoreDecl{Name: "csat", DataType: agentsv1alpha1.ScoreNumeric}},
		},
	}
}

func postFeedback(t *testing.T, s *Server, body SubmitFeedbackRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/feedback", bytes.NewReader(b)))
	return rec
}

// TestSubmitFeedback_DeclaredName_Relayed: a declaration + a declared score name → 202 + relayed to Langfuse.
func TestSubmitFeedback_DeclaredName_Relayed(t *testing.T) {
	created := &[]recordedScore{}
	s := feedbackTestServer(t, recordingLangfuse{created: created},
		agentWithFeedback(feedbackFixture(agentsv1alpha1.FeedbackEnforce)))
	seedRunForTrace(t, s, "t1")

	rec := postFeedback(t, s, SubmitFeedbackRequest{TraceID: "t1", Name: "thumbs", Value: 1, Comment: "great"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, *created, 1, "a declared score is relayed to Langfuse")
	assert.Equal(t, "t1", (*created)[0].traceID)
	assert.Equal(t, "thumbs", (*created)[0].name)
	assert.InDelta(t, 1.0, (*created)[0].value, 1e-9)
}

// TestSubmitFeedback_UndeclaredName_EnforceRejected: an undeclared name under Enforce → 422, NOT relayed.
func TestSubmitFeedback_UndeclaredName_EnforceRejected(t *testing.T) {
	created := &[]recordedScore{}
	s := feedbackTestServer(t, recordingLangfuse{created: created},
		agentWithFeedback(feedbackFixture(agentsv1alpha1.FeedbackEnforce)))
	seedRunForTrace(t, s, "t1")

	rec := postFeedback(t, s, SubmitFeedbackRequest{TraceID: "t1", Name: "undeclared", Value: 1})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "an undeclared name is rejected in Enforce")
	assert.Empty(t, *created, "a rejected score must NOT be relayed")
}

// TestSubmitFeedback_UndeclaredName_MonitorAccepted: an undeclared name under Monitor → 202 + relayed.
func TestSubmitFeedback_UndeclaredName_MonitorAccepted(t *testing.T) {
	created := &[]recordedScore{}
	s := feedbackTestServer(t, recordingLangfuse{created: created},
		agentWithFeedback(feedbackFixture(agentsv1alpha1.FeedbackMonitor)))
	seedRunForTrace(t, s, "t1")

	rec := postFeedback(t, s, SubmitFeedbackRequest{TraceID: "t1", Name: "undeclared", Value: 0.5})
	require.Equal(t, http.StatusAccepted, rec.Code, "Monitor accepts an undeclared name")
	assert.Len(t, *created, 1, "Monitor still relays (accept + count)")
}

// TestSubmitFeedback_NoDeclaration_OpenRelay: an agent without spec.feedback accepts any name → 202 (the open relay).
func TestSubmitFeedback_NoDeclaration_OpenRelay(t *testing.T) {
	created := &[]recordedScore{}
	s := feedbackTestServer(t, recordingLangfuse{created: created}, agentWithFeedback(nil))
	seedRunForTrace(t, s, "t1")

	rec := postFeedback(t, s, SubmitFeedbackRequest{TraceID: "t1", Name: "anything", Value: 1})
	require.Equal(t, http.StatusAccepted, rec.Code, "no spec.feedback ⇒ open relay, any name")
	assert.Len(t, *created, 1)
}

// TestSubmitFeedback_MissingFields400: absent traceId or name → 400, never relayed.
func TestSubmitFeedback_MissingFields400(t *testing.T) {
	created := &[]recordedScore{}
	s := feedbackTestServer(t, recordingLangfuse{created: created}, agentWithFeedback(nil))
	seedRunForTrace(t, s, "t1")

	assert.Equal(t, http.StatusBadRequest, postFeedback(t, s, SubmitFeedbackRequest{Name: "thumbs"}).Code)
	assert.Equal(t, http.StatusBadRequest, postFeedback(t, s, SubmitFeedbackRequest{TraceID: "t1"}).Code)
	assert.Empty(t, *created)
}

// TestFeedbackRead_AttributesSources: GET attributes each score to its declared source, with an explicit
// "unattributed" bucket for an undeclared name (never hidden).
func TestFeedbackRead_AttributesSources(t *testing.T) {
	adapter := recordingLangfuse{
		created: &[]recordedScore{},
		fakeLangfuseAdapter: fakeLangfuseAdapter{scores: []FeedbackScore{
			{ID: "s1", TraceID: "t1", Name: "thumbs", DataType: "BOOLEAN", Value: 1, Source: "API"},
			{ID: "s2", TraceID: "t1", Name: "csat", DataType: "NUMERIC", Value: 0.8, Source: "API"},
			{ID: "s3", TraceID: "t1", Name: "mystery", DataType: "NUMERIC", Value: 0.5, Source: "API"},
		}},
	}
	s := feedbackTestServer(t, adapter,
		agentWithFeedback(feedbackFixture(agentsv1alpha1.FeedbackEnforce)))
	seedRunForTrace(t, s, "t1")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/feedback?traceId=t1", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body FeedbackResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Scores, 3)
	byName := map[string]string{}
	for _, sc := range body.Scores {
		byName[sc.Name] = sc.AttributedSource
	}
	assert.Equal(t, "human", byName["thumbs"])
	assert.Equal(t, "external:csat-webhook", byName["csat"])
	assert.Equal(t, feedbackUnattributed, byName["mystery"], "an undeclared name is surfaced as unattributed, not hidden")
}

// TestFeedbackRead_NoDeclaration_NoAttribution: an agent without spec.feedback leaves attribution empty.
func TestFeedbackRead_NoDeclaration_NoAttribution(t *testing.T) {
	adapter := recordingLangfuse{
		created: &[]recordedScore{},
		fakeLangfuseAdapter: fakeLangfuseAdapter{scores: []FeedbackScore{
			{ID: "s1", TraceID: "t1", Name: "thumbs", DataType: "BOOLEAN", Value: 1, Source: "API"},
		}},
	}
	s := feedbackTestServer(t, adapter, agentWithFeedback(nil))
	seedRunForTrace(t, s, "t1")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/feedback?traceId=t1", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body FeedbackResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Scores, 1)
	assert.Empty(t, body.Scores[0].AttributedSource, "no spec.feedback ⇒ no attribution")
}
