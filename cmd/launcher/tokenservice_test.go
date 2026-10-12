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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// testPodToken is what the launcher tests' proxies present to the token service, read from
// testPodTokenPath as the agent-audience token is in a pod (ADR 0154).
const testPodToken = "agent-pod-token"

var testPodTokenPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "launcher-token")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testPodTokenPath = filepath.Join(dir, "token")
	if err := os.WriteFile(testPodTokenPath, []byte(testPodToken+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// capturedCall is what a fake token service saw of one call.
type capturedCall struct {
	path, authorization, capability string
	body                            []byte
}

// recordingTokenService answers every path with status and body and records each call.
func recordingTokenService(t *testing.T, status int, body string) (*httptest.Server, func() []capturedCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []capturedCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		calls = append(calls, capturedCall{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			capability: r.Header.Get(runcap.HeaderName), body: buf.Bytes(),
		})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedCall(nil), calls...)
	}
}

func mintFor(t *testing.T, user string) (*runcap.Verifier, string) {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	token, err := runcap.NewSigner(priv, kbTestAudience, nil).Mint(runcap.MintRequest{
		User: user, Agent: "prod/asst", RunID: "run-1", TTL: time.Minute,
	})
	require.NoError(t, err)
	return runcap.NewVerifier(pub, kbTestAudience, nil), token
}

func searchWith(t *testing.T, p *longTermProxy, capability string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.register(mux)
	req := httptest.NewRequest(http.MethodPost, "/memory/agent/search", bytes.NewReader([]byte(`{"query":"q"}`)))
	if capability != "" {
		req.Header.Set(runcap.HeaderName, capability)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Every memory call carries the pod's token; only a per-user one carries the capability.
func TestTokenService_MemoryCallsCarryThePodTokenAndAPerUserCapability(t *testing.T) {
	srv, calls := recordingTokenService(t, http.StatusOK, `{"results":[]}`)
	verifier, capability := mintFor(t, "u-alice")

	agentWide := ltProxyTo(srv.URL, scopeAgent)
	require.Equal(t, http.StatusOK, searchWith(t, agentWide, capability).Code)
	perUser := ltProxyTo(srv.URL, scopeAgentUser)
	perUser.verifier = verifier
	require.Equal(t, http.StatusOK, searchWith(t, perUser, capability).Code)

	got := calls()
	require.Len(t, got, 2)
	for _, c := range got {
		assert.Equal(t, "Bearer "+testPodToken, c.authorization, c.path)
		assert.NotContains(t, string(c.body), capability, "the capability never travels in the body")
	}
	assert.Empty(t, got[0].capability, "agent-wide memory acts for no user, so it forwards no capability")
	assert.Equal(t, capability, got[1].capability, "per-user memory forwards the capability its subject came from")
}

// The background remember forwards the capability it was handed, after the request has returned.
func TestTokenService_RememberForwardsThePerUserCapability(t *testing.T) {
	srv, calls := recordingTokenService(t, http.StatusOK, `{}`)
	verifier, capability := mintFor(t, "u-alice")
	p := ltProxyTo(srv.URL, scopeAgentUser)
	p.verifier = verifier

	mux := http.NewServeMux()
	p.register(mux)
	req := httptest.NewRequest(http.MethodPost, "/memory/agent/remember", bytes.NewReader([]byte(`{"content":"c"}`)))
	req.Header.Set(runcap.HeaderName, capability)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	require.Eventually(t, func() bool { return len(calls()) == 1 }, 2*time.Second, 10*time.Millisecond)
	c := calls()[0]
	assert.Equal(t, "Bearer "+testPodToken, c.authorization)
	assert.Equal(t, capability, c.capability)
}

// Knowledge calls carry the pod's token, and a per-user search the capability.
func TestTokenService_KnowledgeCallsCarryThePodToken(t *testing.T) {
	srv, calls := recordingTokenService(t, http.StatusOK, `{"results":[]}`)
	p, signer := kbPerUserProxy(t, srv.URL)
	p.roster["docs"] = kbGrant{embeddingRoute: "embed-v1"}
	capability, err := signer.Mint(runcap.MintRequest{
		User: "u-alice", Agent: "prod/asst", RunID: "run-1", TTL: time.Minute,
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, kbPostWithCap(t, p, `{"knowledgeBase":"docs","query":"q"}`, capability).Code)
	require.Equal(t, http.StatusOK, kbPostWithCap(t, p, `{"knowledgeBase":"personal","query":"q"}`, capability).Code)

	got := calls()
	require.Len(t, got, 2)
	assert.Equal(t, "Bearer "+testPodToken, got[0].authorization)
	assert.Empty(t, got[0].capability, "an org-wide search forwards no capability")
	assert.Equal(t, "Bearer "+testPodToken, got[1].authorization)
	assert.Equal(t, capability, got[1].capability)
}

// Without the token file the launcher does not call unauthenticated, and a refusal names its reason.
func TestTokenService_NoTokenFailsAndRefusalsAreNamed(t *testing.T) {
	srv, calls := recordingTokenService(t, http.StatusUnauthorized,
		`{"error":"unauthenticated","message":"rebuild the agent"}`)

	err := postToTokenService(context.Background(), srv.Client(), tokenServiceCall{
		baseURL: srv.URL, path: "/v1/memory/search",
	}, map[string]any{}, nil)
	require.ErrorContains(t, err, "TOKEN_SERVICE_TOKEN_PATH")
	err = postToTokenService(context.Background(), srv.Client(), tokenServiceCall{
		baseURL: srv.URL, path: "/v1/memory/search", tokenPath: filepath.Join(t.TempDir(), "absent"),
	}, map[string]any{}, nil)
	require.Error(t, err)
	assert.Empty(t, calls(), "nothing is sent without the pod's token")

	var out json.RawMessage
	err = postToTokenService(context.Background(), srv.Client(), tokenServiceCall{
		baseURL: srv.URL, path: "/v1/memory/search", tokenPath: testPodTokenPath,
	}, map[string]any{}, &out)
	require.ErrorContains(t, err, "unauthenticated: rebuild the agent")
	assert.Nil(t, out)
}

// The pod's token path is a platform credential the agent's own process must not inherit.
func TestTokenService_TokenPathIsScrubbedFromTheChild(t *testing.T) {
	env := buildChildEnv(Config{UpstreamPort: 8081}, []string{"TOKEN_SERVICE_TOKEN_PATH=/x", "KEEP=1"})
	assert.NotContains(t, env, "TOKEN_SERVICE_TOKEN_PATH=/x")
	assert.Contains(t, env, "KEEP=1")
}
