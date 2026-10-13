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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/egress"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The real tool-call path through the token service (ADR 0154): the sidecar's proxy verifies the run
// capability, its delegating client sends the sidecar's projected token and that capability, and the real
// credplane handler reviews the token (a fake TokenReview), derives the user and boundary, and resolves.

const (
	tsNamespace = "team-a"
	tsAgent     = "helper"
	tsAudience  = "ctxmesh-credential-plane-test"
	tsServer    = "weather"
)

// review is a fake TokenReview for the sidecar audience.
type review struct {
	ids  map[string]credplane.CallerIdentity
	down bool
}

func (r review) Verify(_ context.Context, token string) (credplane.CallerIdentity, error) {
	if r.down {
		return credplane.CallerIdentity{}, errors.New("tokenreview: connection refused")
	}
	id, ok := r.ids[token]
	if !ok {
		return credplane.CallerIdentity{}, credplane.ErrCallerRejected
	}
	return id, nil
}

type resolved struct {
	mu                         sync.Mutex
	calls                      int
	ns, boundary, server, user string
}

func (r *resolved) Resolve(_ context.Context, ns, boundary, server, user string) (credresolve.Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ns, r.boundary, r.server, r.user = ns, boundary, server, user
	return credresolve.Credential{Kind: credresolve.KindBearer, Value: "USER-TOKEN"}, nil
}

func (r *resolved) Revoke(context.Context, string, string, string, string) error { return nil }

func (r *resolved) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type toolPath struct {
	proxy       *egress.Proxy
	capability  string
	resolver    *resolved
	upstreamGot func() []string
	tokenFile   string
}

// newToolPath wires an upstream MCP server, the token service and the sidecar the way the sidecar's
// main does, with sidecarToken known to the token service's review.
func newToolPath(t *testing.T, reviewDown bool) toolPath {
	t.Helper()
	var (
		mu  sync.Mutex
		got []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(upstream.Close)

	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	verifier := runcap.NewVerifier(pub, tsAudience, nil)
	const sidecarToken = "projected-sidecar-token"
	res := &resolved{}
	service := httptest.NewServer(credplane.NewServer(res, credplane.CallerAuth{
		Sidecar: review{down: reviewDown, ids: map[string]credplane.CallerIdentity{
			sidecarToken: {Namespace: tsNamespace, ServiceAccount: credplane.AgentServiceAccountPrefix + tsAgent},
		}},
		Agent:        review{ids: map[string]credplane.CallerIdentity{}},
		Capabilities: verifier,
	}, logr.Discard()).Handler())
	t.Cleanup(service.Close)

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(sidecarToken), 0o600))
	env := map[string]string{envTokenServiceTokenPath: tokenFile}
	httpClient, err := delegatingHTTPClient(logr.Discard())
	require.NoError(t, err)
	httpClient, err = withTokenServiceCredential(httpClient, func(k string) string { return env[k] })
	require.NoError(t, err)

	routes, err := egress.ParseRouteTable(
		[]byte(`[{"name":"` + tsServer + `","targetURL":"` + upstream.URL + `","oauth":true}]`))
	require.NoError(t, err)
	boundary := credresolve.AgentBoundary(tsNamespace, tsAgent)
	proxy := egress.NewProxy(egress.ProxyConfig{
		Verifier:         verifier,
		Resolver:         credplane.NewClient(service.URL, httpClient),
		Namespace:        tsNamespace,
		ExpectedBoundary: boundary,
		Routes:           routes,
		Log:              logr.Discard(),
	})
	capability, err := runcap.NewSigner(priv, tsAudience, nil).Mint(runcap.MintRequest{
		User: "u-alice", Agent: tsNamespace + "/" + tsAgent, Boundary: boundary, RunID: "run-1", TTL: time.Minute,
	})
	require.NoError(t, err)
	return toolPath{
		proxy: proxy, capability: capability, resolver: res, tokenFile: tokenFile,
		upstreamGot: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), got...)
		},
	}
}

func (p toolPath) call(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+tsServer+"/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forecast"}}`)))
	req.Header.Set(runcap.HeaderName, p.capability)
	rec := httptest.NewRecorder()
	p.proxy.ServeHTTP(rec, req)
	return rec
}

func TestToolCallResolvesThroughTheTokenServiceWithBothCredentials(t *testing.T) {
	p := newToolPath(t, false)
	rec := p.call(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"Bearer USER-TOKEN"}, p.upstreamGot(), "the user's credential reaches the MCP server")

	require.Equal(t, 1, p.resolver.count())
	assert.Equal(t, tsNamespace, p.resolver.ns)
	assert.Equal(t, credresolve.AgentBoundary(tsNamespace, tsAgent), p.resolver.boundary)
	assert.Equal(t, tsServer, p.resolver.server)
	assert.Equal(t, "u-alice", p.resolver.user)
}

func TestToolCallWithoutTheSidecarTokenFailsVisibly(t *testing.T) {
	p := newToolPath(t, false)
	require.NoError(t, os.Remove(p.tokenFile))
	rec := p.call(t)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Zero(t, p.resolver.count())
	assert.Empty(t, p.upstreamGot(), "no tool call without the user's credential")
}

func TestToolCallWhileTokenReviewIsDownFailsVisibly(t *testing.T) {
	p := newToolPath(t, true)
	rec := p.call(t)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"a 503 from the token service is a visible 502, never a silent forward")
	assert.Zero(t, p.resolver.count())
	assert.Empty(t, p.upstreamGot())
}

func TestASidecarWithoutATokenPathRefusesToStart(t *testing.T) {
	_, err := withTokenServiceCredential(&http.Client{}, func(string) string { return "" })
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), envTokenServiceTokenPath))
}
