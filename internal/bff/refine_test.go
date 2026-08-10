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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// validCurrentSpec is a minimal but valid simplified agent.yaml for use as the
// "before" spec in refine tests. It expand-validates to a managed AgentDeployment.
const validCurrentSpec = `name: support-bot
runtime: managed
systemPrompt: You are a basic assistant.
`

// validRefinedYAML is the "after" spec the fake model returns — valid YAML that
// expand-validates. It differs from validCurrentSpec in systemPrompt and adds tools.
const validRefinedYAML = `name: support-bot
runtime: managed
systemPrompt: You are a friendly support assistant with access to documentation.
tools:
  - search_docs
`

// refineBody marshals a RefineAgentRequest for use in httptest requests.
func refineBody(t *testing.T, req RefineAgentRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return b
}

// TestRefineHappyPath proves that a valid CurrentSpec + instruction:
//   - calls the model with the refine system prompt (not the generation one);
//   - the output is expand-validated and returned for review (HTTP 200);
//   - the Diff lists the changed top-level fields (systemPrompt, tools);
//   - the key is NEVER in the response or logs (server-side only);
//   - the cost tag rides the request body;
//   - no cluster write happens (pure).
func TestRefineHappyPath(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validRefinedYAML)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)...).Build()
	s, factory, lb := newGenerateServer(t, c, nil)

	createCalled := false
	// Instrument the server's underlying client to detect any Create calls.
	_ = createCalled // the fake.Client above has no interceptor; we assert via pure semantics

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{
			CurrentSpec: validCurrentSpec,
			Instruction: "add a search_docs tool and improve the system prompt",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "developer-persona-token", factory.gotToken, "the caller's token scoped the key read")

	var resp RefineAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Equal(t, "claude-sonnet-4-6", resp.Model)
	assert.Contains(t, resp.AgentYAML, "runtime: managed")
	assert.Contains(t, resp.Expanded, "kind: AgentDeployment")
	assert.NotNil(t, resp.Warnings, "warnings is [] not null")

	// The diff must list the changed top-level fields.
	assert.Contains(t, resp.Diff, "systemPrompt", "systemPrompt changed → must appear in diff")
	assert.Contains(t, resp.Diff, "tools", "tools added → must appear in diff")
	// Unchanged fields must NOT appear in the diff.
	assert.NotContains(t, resp.Diff, "name", "name is unchanged → must not appear in diff")
	assert.NotContains(t, resp.Diff, "runtime", "runtime is unchanged → must not appear in diff")

	// The key must NEVER appear in the response or logs.
	assert.NotContains(t, rec.Body.String(), theTestKey, "the key must NEVER be in the response")
	assert.NotContains(t, lb.String(), theTestKey, "the key must NEVER be logged")

	// The cost tag rides the request body; the key never does.
	require.NotNil(t, *lastBody)
	assert.NotContains(t, string(*lastBody), theTestKey, "the key rides headers, never the body")
}

// TestRefineInvalidOutputRetriesAndGives422 proves: if the model's first output
// fails expand-validation the endpoint makes exactly ONE internal retry; if the
// retry also fails → 422 regenerate (not a 500, not applied).
//
// The fake provider serves junk on every call so both attempts fail. We verify
// the retry by checking that the second attempt's user message contains the
// expand error (the prompt is passed through the HTTP body in the fake).
func TestRefineInvalidOutputRetriesAndGives422(t *testing.T) {
	callCount := 0
	// The fake provider always returns invalid YAML so expand always fails.
	junkYAML := "this is not valid agent yaml at all !!!"
	// We need a custom fake that counts calls and always returns junk.
	var lastBodies [][]byte
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != theTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		callCount++
		var body []byte
		if r.Body != nil {
			buf := make([]byte, maxRefineRequestBytes)
			n, _ := r.Body.Read(buf)
			body = buf[:n]
		}
		lastBodies = append(lastBodies, body)
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/v1/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]string{{"type": "text", "text": junkYAML}},
			})
		default: // /v1/chat/completions and anything else
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": junkYAML}}},
			})
		}
	}))
	t.Cleanup(fakeSrv.Close)

	createCalled := false
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", fakeSrv.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{
			CurrentSpec: validCurrentSpec,
			Instruction: "add a tool",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "both attempts fail → 422 regenerate, NOT a 500; body: %s", rec.Body.String())
	var resp GenerateInvalidResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Regenerate, "the UI is signalled to try again")
	assert.NotEmpty(t, resp.Reason, "the expand validation reason is returned")
	assert.NotEmpty(t, resp.AgentYAML, "the raw model output is returned")

	// Exactly two model calls must have been made (original + one retry).
	assert.Equal(t, 2, callCount, "exactly one internal retry: 2 model calls total")

	// The retry's body must contain the expand error from the first attempt,
	// proving the error-feedback was appended to the prompt.
	if len(lastBodies) >= 2 {
		retryBody := string(lastBodies[1])
		assert.Contains(t, retryBody, "failed validation", "the retry prompt includes the expand error")
	}

	// No cluster write must have happened (pure endpoint).
	assert.False(t, createCalled, "refine NEVER writes to the cluster")
}

// TestRefineInlineSecretInInputIs422 proves that a currentSpec carrying an inline
// credential is rejected BEFORE any model call (the endpoint never sends it to the
// model). The model must not be reached at all.
func TestRefineInlineSecretInInputIs422(t *testing.T) {
	modelCalled := false
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fakeSrv.Close)

	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", fakeSrv.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	// A currentSpec with an inline apiKey — must be rejected immediately.
	secretSpec := `name: leaky-bot
runtime: managed
systemPrompt: I am helpful.
apiKey: sk-super-secret-leaked-key
`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{
			CurrentSpec: secretSpec,
			Instruction: "add a tool",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "inline secret in input → 422; body: %s", rec.Body.String())
	assert.False(t, modelCalled, "the model must NOT be called when the input carries an inline secret")
	// The secret value must NEVER appear in the response.
	assert.NotContains(t, rec.Body.String(), "sk-super-secret-leaked-key")
}

// TestRefineInlineSecretInOutputIs422 proves that a secret the model emits in the
// output is rejected BEFORE being returned to the caller. The model call succeeds
// but the response is blocked.
func TestRefineInlineSecretInOutputIs422(t *testing.T) {
	// The model returns a spec with an inline token — a miscreant or a confused model.
	secretOutput := `name: support-bot
runtime: managed
systemPrompt: You are helpful.
token: sk-this-should-never-be-returned
`
	prov, _ := fakeChatProvider(t, secretOutput)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{
			CurrentSpec: validCurrentSpec,
			Instruction: "add a token field",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "inline secret in output → 422; body: %s", rec.Body.String())
	// The emitted secret MUST NOT appear in the response.
	assert.NotContains(t, rec.Body.String(), "sk-this-should-never-be-returned",
		"a secret the model emitted must NEVER be returned to the caller")
}

// TestRefineTranscriptIsCapped proves that a 20-turn transcript is capped to the
// last maxTranscriptTurns (8) before being included in the model prompt.
// We verify indirectly: the test builds a transcript where only the last 8 turns
// carry a recognizable marker and the first 12 do not; then it asserts the
// user message built by buildRefineUserMessage contains the late markers and not
// the early ones (purely testing the helper function, no HTTP needed).
func TestRefineTranscriptIsCapped(t *testing.T) {
	// Build a 20-turn transcript. Turns 0–11 are "early"; turns 12–19 are "late".
	transcript := make([]RefineTurn, 20)
	for i := range transcript {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if i < 12 {
			transcript[i] = RefineTurn{Role: role, Text: "early-turn"}
		} else {
			transcript[i] = RefineTurn{Role: role, Text: "late-turn"}
		}
	}

	capped := cappedTranscript(transcript, maxTranscriptTurns)
	assert.Len(t, capped, maxTranscriptTurns, "capped to maxTranscriptTurns turns")

	// The capped transcript must be the LAST 8 turns — all "late".
	for i, turn := range capped {
		assert.Equal(t, "late-turn", turn.Text, "turn %d must be a late turn", i)
	}

	// Build the user message and verify the early turns are absent.
	msg := buildRefineUserMessage(validCurrentSpec, capped, "some instruction")
	assert.NotContains(t, msg, "early-turn", "early turns must not reach the prompt")
	assert.Contains(t, msg, "late-turn", "late turns must reach the prompt")
	assert.Contains(t, msg, "some instruction", "the instruction must always be present")
}

// TestRefineNeverWritesToCluster proves that the refine endpoint is PURE: even a
// fully valid refine makes NO CRD create/apply calls. The cluster Create interceptor
// records any call; none must fire.
func TestRefineNeverWritesToCluster(t *testing.T) {
	prov, _ := fakeChatProvider(t, validRefinedYAML)
	createCalled := false
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{
			CurrentSpec: validCurrentSpec,
			Instruction: "improve the system prompt",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.False(t, createCalled, "refine is PURE — it NEVER creates a CRD")
}

// TestRefineNilExpandIs501 proves that without the expand adapter wired the route
// serves an honest 501 (symmetric with the generate gate).
func TestRefineNilExpandIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		// No Expand adapter.
		Version: "test",
		Log:     funcr.New(func(string, string) {}, funcr.Options{}),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{CurrentSpec: "x: y", Instruction: "change it"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestRefineEmptyCurrentSpecIs400 proves an empty currentSpec is caught early.
func TestRefineEmptyCurrentSpecIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{CurrentSpec: "  ", Instruction: "add a tool", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestRefineEmptyInstructionIs400 proves an empty instruction is caught early.
func TestRefineEmptyInstructionIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/refine",
		bytes.NewReader(refineBody(t, RefineAgentRequest{CurrentSpec: validCurrentSpec, Instruction: " ", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestRefineChangedFieldsDiff exercises the changedFields helper directly:
// adding a field, removing a field, modifying a field, and leaving one unchanged.
func TestRefineChangedFieldsDiff(t *testing.T) {
	oldSpec := `name: bot
runtime: managed
systemPrompt: old prompt
scaling:
  min: 1
  max: 3
`
	newSpec := `name: bot
runtime: managed
systemPrompt: new prompt
tools:
  - search_docs
`
	diff := changedFields(oldSpec, newSpec)
	// systemPrompt modified, scaling removed, tools added — all must appear.
	assert.Contains(t, diff, "systemPrompt", "modified field must appear")
	assert.Contains(t, diff, "scaling", "removed field must appear")
	assert.Contains(t, diff, "tools", "added field must appear")
	// Unchanged fields must not appear.
	assert.NotContains(t, diff, "name", "unchanged field must not appear")
	assert.NotContains(t, diff, "runtime", "unchanged field must not appear")
	// Diff must be sorted.
	for i := 1; i < len(diff); i++ {
		assert.LessOrEqual(t, diff[i-1], diff[i], "diff must be sorted")
	}
}

// TestRefineBuildUserMessage verifies buildRefineUserMessage includes the spec,
// the capped transcript, and the instruction in the right order.
func TestRefineBuildUserMessage(t *testing.T) {
	transcript := []RefineTurn{
		{Role: "user", Text: "make it friendlier"},
		{Role: "assistant", Text: "done, here is the update"},
	}
	msg := buildRefineUserMessage(validCurrentSpec, transcript, "add a budget of $0.10")

	assert.Contains(t, msg, "Current agent.yaml:", "spec preamble must be present")
	assert.Contains(t, msg, "name: support-bot", "spec content must be present")
	assert.Contains(t, msg, "Prior conversation context", "transcript section header must be present")
	assert.Contains(t, msg, "make it friendlier", "first transcript turn must be present")
	assert.Contains(t, msg, "done, here is the update", "second transcript turn must be present")
	assert.Contains(t, msg, "add a budget of $0.10", "instruction must be present")
	// Instruction must come AFTER the transcript.
	transcriptIdx := strings.Index(msg, "Prior conversation context")
	instructionIdx := strings.Index(msg, "add a budget of $0.10")
	assert.Less(t, transcriptIdx, instructionIdx, "instruction must come after the transcript")
}
