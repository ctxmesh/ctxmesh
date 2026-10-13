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

package credplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory/recall"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge/retrieval"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The caller-authentication matrix of ADR 0154. Every guard has a case that passes it and one that
// trips it, so a guard that stops guarding fails a test.

// recordingResolver records what the service resolved, stored and revoked.
type recordingResolver struct {
	resolves, stores, revokes  int
	ns, boundary, server, user string
}

func (r *recordingResolver) Resolve(_ context.Context, ns, boundary, server, user string) (credresolve.Credential, error) {
	r.resolves++
	r.ns, r.boundary, r.server, r.user = ns, boundary, server, user
	return credresolve.Credential{Kind: credresolve.KindBearer, Value: "USER-TOKEN"}, nil
}

func (r *recordingResolver) Revoke(context.Context, string, string, string, string) error {
	r.revokes++
	return nil
}

func (r *recordingResolver) StoreGrant(context.Context, string, string, string, string, credresolve.Grant) error {
	r.stores++
	return nil
}

// fullServer serves every endpoint, with fresh stores.
func fullServer(t *testing.T, f *authFixture) (*Server, *recordingResolver) {
	t.Helper()
	res := &recordingResolver{}
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(context.Background(), testAgentNS, "docs"))
	s := NewServer(res, f.Auth, logr.Discard()).
		WithMemory(recall.New(agentmemory.NewMemStore(), vecEmbedder{}, logr.Discard())).
		WithKnowledge(retrieval.NewSearcher(store, dirEmbedder{}, logr.Discard()))
	return s, res
}

// The endpoints a caller must authenticate to, with a request each that would otherwise succeed.
func everyEndpoint() map[string]any {
	user := "hash-alice"
	return map[string]any{
		pathResolve: resolveRequest{
			Namespace: testAgentNS, Boundary: credresolve.AgentBoundary(testAgentNS, testAgent),
			Server: "weather", UserHash: user,
		},
		pathRevoke: revokeRequest{Namespace: testAgentNS, Server: "weather", UserHash: user},
		pathStore:  storeRequest{Namespace: testAgentNS, Server: "weather", UserHash: user, AccessToken: "at"},
		pathMemoryRemember: recall.RememberRequest{
			Namespace: testAgentNS, AgentName: testAgent,
			Scope: agentmemory.ScopeAgent, Content: "north", EmbeddingModel: "m",
		},
		pathMemorySearch: recall.SearchRequest{
			Namespace: testAgentNS, AgentName: testAgent,
			Scope: agentmemory.ScopeAgent, Query: "north", EmbeddingModel: "m",
		},
		pathKnowledgeSearch: retrieval.SearchRequest{
			Namespace: testAgentNS, KnowledgeBase: "docs", Query: "q",
			EmbeddingModel: "m",
		},
	}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func withCapability(h map[string]string, capability string) map[string]string {
	h[runcap.HeaderName] = capability
	return h
}

func refusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var r refusal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r), rec.Body.String())
	return r.Error
}

func TestAuth_NoTokenIsRefusedEverywhereButTheProbes(t *testing.T) {
	f := newAuthFixture()
	s, res := fullServer(t, f)
	for path, body := range everyEndpoint() {
		rec := postJSONWith(t, s, path, body, map[string]string{
			runcap.HeaderName: f.StandaloneCapability("hash-alice", testAgentNS, testAgent),
		})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
		assert.Equal(t, errCodeUnauthenticated, refusalCode(t, rec), path)
		rec = postJSONWith(t, s, path, body, map[string]string{"Authorization": "Basic Zm9vOmJhcg=="})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s: a non-Bearer credential", path)
	}
	assert.Zero(t, res.resolves+res.stores+res.revokes, "nothing reached the backend")
	assert.Zero(t, f.sidecar.reviews+f.agent.reviews, "a request with no token costs no TokenReview")

	for _, probe := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probe, nil))
		assert.Equal(t, http.StatusOK, rec.Code, probe)
	}
}

func TestAuth_AgentAudienceRefusedWhereOnlyTheSidecarOrPlatformCalls(t *testing.T) {
	f := newAuthFixture()
	s, res := fullServer(t, f)
	bodies := everyEndpoint()
	for _, path := range []string{pathResolve, pathStore, pathRevoke} {
		h := withCapability(bearer(f.AgentToken(testAgentNS, testAgent)),
			f.StandaloneCapability("hash-alice", testAgentNS, testAgent))
		rec := postJSONWith(t, s, path, bodies[path], h)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
	assert.Zero(t, res.resolves+res.stores+res.revokes)
}

func TestAuth_SidecarAudienceRefusedOnMemory(t *testing.T) {
	f := newAuthFixture()
	s, _ := fullServer(t, f)
	bodies := everyEndpoint()
	for _, path := range []string{pathMemoryRemember, pathMemorySearch} {
		rec := postJSONWith(t, s, path, bodies[path], bearer(f.SidecarToken(testAgentNS, testAgent)))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
}

func TestAuth_ReviewUnreachableIs503(t *testing.T) {
	f := newAuthFixture()
	s, res := fullServer(t, f)
	sidecar, agent := f.SidecarToken(testAgentNS, testAgent), f.AgentToken(testAgentNS, testAgent)
	f.sidecar.down, f.agent.down = errReviewDown, errReviewDown
	for path, body := range everyEndpoint() {
		token := sidecar
		if path == pathMemoryRemember || path == pathMemorySearch || path == pathKnowledgeSearch {
			token = agent
		}
		rec := postJSONWith(t, s, path, body, bearer(token))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		assert.Equal(t, errCodeAuthUnavailable, refusalCode(t, rec), path)
	}
	assert.Zero(t, res.resolves+res.stores+res.revokes)
}

func TestAuth_Resolve(t *testing.T) {
	const user = "hash-alice"
	boundary := credresolve.AgentBoundary(testAgentNS, testAgent)
	good := resolveRequest{Namespace: testAgentNS, Boundary: boundary, Server: "weather", UserHash: user}
	f := newAuthFixture()
	registry := credresolve.RegistryBoundary(testAgentNS, "squad")

	cases := []struct {
		name       string
		token      string
		capability string
		body       resolveRequest
		want       int
	}{
		{
			"the sidecar for the capability's user and boundary", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, testAgentNS, testAgent), good, http.StatusOK,
		},
		{
			"a registry teammate's relayed capability", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, testAgentNS+"/caller", registry),
			resolveRequest{Namespace: testAgentNS, Boundary: registry, Server: "weather", UserHash: user},
			http.StatusOK,
		},
		{"no capability", f.SidecarToken(testAgentNS, testAgent), "", good, http.StatusUnauthorized},
		{
			"an expired capability", f.SidecarToken(testAgentNS, testAgent),
			f.ExpiredCapability(user, testAgentNS, testAgent), good, http.StatusUnauthorized,
		},
		{
			"a forged capability", f.SidecarToken(testAgentNS, testAgent),
			f.ForgedCapability(user, testAgentNS, testAgent), good, http.StatusUnauthorized,
		},
		{
			"a body naming another user", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, testAgentNS, testAgent),
			resolveRequest{Namespace: testAgentNS, Boundary: boundary, Server: "weather", UserHash: "hash-bob"},
			http.StatusForbidden,
		},
		{
			"a body naming another boundary", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, testAgentNS, testAgent),
			resolveRequest{Namespace: testAgentNS, Boundary: registry, Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a body naming another namespace", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, testAgentNS, testAgent),
			resolveRequest{Namespace: "other", Boundary: boundary, Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a capability whose agent is in another namespace", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, "other/"+testAgent, boundary), good, http.StatusForbidden,
		},
		{
			"a capability for another namespace's agent, body to match", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, "other", testAgent),
			resolveRequest{Namespace: testAgentNS, Boundary: credresolve.AgentBoundary("other", testAgent), Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a capability for another namespace's registry", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, testAgentNS+"/"+testAgent, credresolve.RegistryBoundary("other", "squad")),
			resolveRequest{Namespace: testAgentNS, Boundary: credresolve.RegistryBoundary("other", "squad"), Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"another standalone agent's capability", f.SidecarToken(testAgentNS, testAgent),
			f.StandaloneCapability(user, testAgentNS, "someone-else"),
			resolveRequest{Namespace: testAgentNS, Boundary: credresolve.AgentBoundary(testAgentNS, "someone-else"), Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a capability with no boundary", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, testAgentNS+"/"+testAgent, ""),
			resolveRequest{Namespace: testAgentNS, Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a capability with no acting agent", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, "", boundary), good, http.StatusForbidden,
		},
		{
			"the BFF, which is no agent", f.BFFToken(),
			f.StandaloneCapability(user, testAgentNS, testAgent), good, http.StatusForbidden,
		},
		{
			"a pod in the agent's namespace that is no agent", nonAgentToken(f, runcap.TokenServicePodAudience),
			f.Capability(user, testAgentNS+"/caller", registry),
			resolveRequest{Namespace: testAgentNS, Boundary: registry, Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"a capability whose boundary is another namespace's agent", f.SidecarToken(testAgentNS, testAgent),
			f.Capability(user, testAgentNS+"/"+testAgent, credresolve.AgentBoundary("other", testAgent)),
			resolveRequest{Namespace: testAgentNS, Boundary: credresolve.AgentBoundary("other", testAgent), Server: "weather", UserHash: user},
			http.StatusForbidden,
		},
		{
			"an unknown token", f.podToken(runcap.TokenServicePodAudience),
			f.StandaloneCapability(user, testAgentNS, testAgent), good, http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, res := fullServer(t, f)
			h := bearer(tc.token)
			if tc.capability != "" {
				h = withCapability(h, tc.capability)
			}
			rec := postJSONWith(t, s, pathResolve, tc.body, h)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.want != http.StatusOK {
				assert.Zero(t, res.resolves, "a refused resolve reached the backend")
				return
			}
			assert.Equal(t, 1, res.resolves)
			assert.Equal(t, testAgentNS, res.ns, "the namespace is the caller's")
			assert.Equal(t, tc.body.Boundary, res.boundary, "the boundary is the capability's")
			assert.Equal(t, user, res.user, "the user is the capability's")
		})
	}
}

func TestAuth_ResolveWithoutACapabilityKeyIs503(t *testing.T) {
	f := newAuthFixture()
	f.Auth.Capabilities = nil
	s, res := fullServer(t, f)
	h := withCapability(bearer(f.SidecarToken(testAgentNS, testAgent)),
		f.StandaloneCapability("hash-alice", testAgentNS, testAgent))
	rec := postJSONWith(t, s, pathResolve, everyEndpoint()[pathResolve], h)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, errCodeCapabilityKeyUnavailable, refusalCode(t, rec))
	assert.Zero(t, res.resolves)
}

func TestAuth_StoreAndRevokeArePlatformOnly(t *testing.T) {
	f := newAuthFixture()
	bodies := everyEndpoint()
	for _, path := range []string{pathStore, pathRevoke} {
		s, res := fullServer(t, f)
		rec := postJSONWith(t, s, path, bodies[path], bearer(f.SidecarToken(testAgentNS, testAgent)))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s by an agent", path)
		assert.Zero(t, res.stores+res.revokes)

		rec = postJSONWith(t, s, path, bodies[path], bearer(f.BFFToken()))
		assert.Equal(t, http.StatusOK, rec.Code, "%s by the BFF", path)
		assert.Equal(t, 1, res.stores+res.revokes)
	}
}

func TestAuth_Memory(t *testing.T) {
	f := newAuthFixture()
	agent := func() map[string]string { return bearer(f.AgentToken(testAgentNS, testAgent)) }
	perUser := func(user string) recall.SearchRequest {
		return recall.SearchRequest{
			Namespace: testAgentNS, AgentName: testAgent, Scope: agentmemory.ScopeAgentUser,
			Subject: user, Query: "north", EmbeddingModel: "m",
		}
	}
	agentWide := recall.SearchRequest{
		Namespace: testAgentNS, AgentName: testAgent, Scope: agentmemory.ScopeAgent,
		Query: "north", EmbeddingModel: "m",
	}

	cases := []struct {
		name    string
		headers map[string]string
		body    recall.SearchRequest
		want    int
	}{
		{"agent-wide, its own", agent(), agentWide, http.StatusOK},
		{
			"per-user, the capability's user", withCapability(agent(), f.StandaloneCapability("hash-alice", testAgentNS, testAgent)),
			perUser("hash-alice"), http.StatusOK,
		},
		{"another namespace", agent(), recall.SearchRequest{
			Namespace: "other", AgentName: testAgent,
			Scope: agentmemory.ScopeAgent, Query: "north", EmbeddingModel: "m",
		}, http.StatusForbidden},
		{"another agent", agent(), recall.SearchRequest{
			Namespace: testAgentNS, AgentName: "other",
			Scope: agentmemory.ScopeAgent, Query: "north", EmbeddingModel: "m",
		}, http.StatusForbidden},
		{"agent-wide with a subject", agent(), recall.SearchRequest{
			Namespace: testAgentNS, AgentName: testAgent,
			Scope: agentmemory.ScopeAgent, Subject: "hash-alice", Query: "north", EmbeddingModel: "m",
		}, http.StatusForbidden},
		{"per-user without a capability", agent(), perUser("hash-alice"), http.StatusUnauthorized},
		{"per-user, an empty capability header", withCapability(agent(), ""), perUser("hash-alice"), http.StatusUnauthorized},
		{
			"per-user, a foreign subject", withCapability(agent(), f.StandaloneCapability("hash-alice", testAgentNS, testAgent)),
			perUser("hash-bob"), http.StatusForbidden,
		},
		{
			"per-user, a capability whose agent is in another namespace",
			withCapability(agent(), f.StandaloneCapability("hash-alice", "other", testAgent)),
			perUser("hash-alice"), http.StatusForbidden,
		},
		{
			"per-user, a forged capability", withCapability(agent(), f.ForgedCapability("hash-alice", testAgentNS, testAgent)),
			perUser("hash-alice"), http.StatusUnauthorized,
		},
		{
			"per-user, an expired capability", withCapability(agent(), f.ExpiredCapability("hash-alice", testAgentNS, testAgent)),
			perUser("hash-alice"), http.StatusUnauthorized,
		},
		{"the BFF, which is no agent", bearer(f.BFFToken()), agentWide, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fullServer(t, f)
			rec := postJSONWith(t, s, pathMemorySearch, tc.body, tc.headers)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			remember := recall.RememberRequest{
				Namespace: tc.body.Namespace, AgentName: tc.body.AgentName,
				Scope: tc.body.Scope, Subject: tc.body.Subject, Content: "north", EmbeddingModel: "m",
			}
			rec = postJSONWith(t, s, pathMemoryRemember, remember, tc.headers)
			assert.Equal(t, tc.want, rec.Code, "remember: %s", rec.Body.String())
		})
	}
}

// nonAgentToken is a token for audience issued to the agent namespace's default ServiceAccount.
func nonAgentToken(f *authFixture, audience string) string {
	token := f.podToken(audience)
	id := CallerIdentity{Namespace: testAgentNS, ServiceAccount: "default"}
	if audience == runcap.TokenServicePodAudience {
		f.sidecar.issue(token, id)
	} else {
		f.agent.issue(token, id)
	}
	return token
}

func TestAuth_MemoryAndKnowledgeRefuseACallerThatIsNoAgent(t *testing.T) {
	f := newAuthFixture()
	s, _ := fullServer(t, f)
	bodies := everyEndpoint()
	for _, path := range []string{pathMemorySearch, pathMemoryRemember, pathKnowledgeSearch} {
		rec := postJSONWith(t, s, path, bodies[path], bearer(nonAgentToken(f, runcap.TokenServiceAgentAudience)))
		assert.Equal(t, http.StatusForbidden, rec.Code, path)
	}
	// Naming no agent does not make a pod that is no agent one: there is no memory partition of "".
	nameless := recall.SearchRequest{Namespace: testAgentNS, Scope: agentmemory.ScopeAgent, Query: "q", EmbeddingModel: "m"}
	rec := postJSONWith(t, s, pathMemorySearch, nameless, bearer(nonAgentToken(f, runcap.TokenServiceAgentAudience)))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAuth_Knowledge(t *testing.T) {
	f := newAuthFixture()
	own := retrieval.SearchRequest{Namespace: testAgentNS, KnowledgeBase: "docs", Query: "q", EmbeddingModel: "m"}
	other := retrieval.SearchRequest{Namespace: "other", KnowledgeBase: "docs", Query: "q", EmbeddingModel: "m"}
	perUser := func(user string) retrieval.SearchRequest {
		r := own
		r.Subject = user
		return r
	}
	agent := func() map[string]string { return bearer(f.AgentToken(testAgentNS, testAgent)) }

	cases := []struct {
		name    string
		headers map[string]string
		body    retrieval.SearchRequest
		want    int
	}{
		{"an agent, its own namespace", agent(), own, http.StatusOK},
		{"an agent, another namespace", agent(), other, http.StatusForbidden},
		{
			"an agent, per-user as the capability's user",
			withCapability(agent(), f.StandaloneCapability("hash-alice", testAgentNS, testAgent)), perUser("hash-alice"), http.StatusOK,
		},
		{"an agent, per-user without a capability", agent(), perUser("hash-alice"), http.StatusUnauthorized},
		{
			"an agent, per-user with a capability whose agent is in another namespace",
			withCapability(agent(), f.StandaloneCapability("hash-alice", "other", testAgent)),
			perUser("hash-alice"), http.StatusForbidden,
		},
		{
			"an agent, per-user as a foreign subject",
			withCapability(agent(), f.StandaloneCapability("hash-alice", testAgentNS, testAgent)), perUser("hash-bob"), http.StatusForbidden,
		},
		{"the BFF, another namespace", bearer(f.BFFToken()), other, http.StatusOK},
		{"the BFF, a per-user subject", bearer(f.BFFToken()), perUser("hash-bob"), http.StatusOK},
		{"an egress sidecar, its own namespace", bearer(f.SidecarToken(testAgentNS, testAgent)), own, http.StatusForbidden},
		{"the BFF's identity with the agent audience", bearer(platformAgentAudienceToken(f)), other, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fullServer(t, f)
			rec := postJSONWith(t, s, pathKnowledgeSearch, tc.body, tc.headers)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

func TestUnverifiedAudiences(t *testing.T) {
	f := newAuthFixture()
	assert.Equal(t, []string{runcap.TokenServiceAgentAudience}, unverifiedAudiences(f.podToken(runcap.TokenServiceAgentAudience)))
	for _, bad := range []string{"", "a.b", "a.!!!.c", "a." + "e30" + ".c"} {
		assert.Empty(t, unverifiedAudiences(bad), bad)
	}
}

func TestBearerTransport(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "token")
	client := &http.Client{Transport: NewBearerTransport(srv.Client().Transport, path)}

	_, err := client.Get(srv.URL)
	require.Error(t, err, "no token file: the request fails rather than going out unauthenticated")
	require.NoError(t, os.WriteFile(path, []byte("  \n"), 0o600))
	_, err = client.Get(srv.URL)
	require.Error(t, err, "an empty token file fails the request")
	mu.Lock()
	assert.Empty(t, got, "nothing was sent without a token")
	mu.Unlock()

	require.NoError(t, os.WriteFile(path, []byte("first\n"), 0o600))
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, os.WriteFile(path, []byte("rotated"), 0o600))
	resp, err = client.Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"Bearer first", "Bearer rotated"}, got, "the token is read per request, as the kubelet rotates it")
}

func TestAuth_MissingCapabilityIsNamed(t *testing.T) {
	f := newAuthFixture()
	s, _ := fullServer(t, f)
	rec := postJSONWith(t, s, pathResolve, everyEndpoint()[pathResolve], bearer(f.SidecarToken(testAgentNS, testAgent)))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, errCodeNoCapability, refusalCode(t, rec))
	rec = postJSONWith(t, s, pathResolve, everyEndpoint()[pathResolve],
		withCapability(bearer(f.SidecarToken(testAgentNS, testAgent)), f.ForgedCapability("hash-alice", testAgentNS, testAgent)))
	assert.Equal(t, errCodeInvalidCapability, refusalCode(t, rec))
}

// platformAgentAudienceToken is a token for the agent audience issued to the BFF's identity: the platform
// is the platform only with the sidecar audience.
func platformAgentAudienceToken(f *authFixture) string {
	token := f.podToken(runcap.TokenServiceAgentAudience)
	f.agent.issue(token, fixturePlatform)
	return token
}

// A capability without a namespaced boundary is refused for that reason, not by accident of a later check.
func TestAuth_ResolveNamesAMissingBoundary(t *testing.T) {
	f := newAuthFixture()
	s, _ := fullServer(t, f)
	for _, boundary := range []string{"", "r:squad"} {
		h := withCapability(bearer(f.SidecarToken(testAgentNS, testAgent)), f.Capability("hash-alice", testAgentNS+"/"+testAgent, boundary))
		rec := postJSONWith(t, s, pathResolve, resolveRequest{
			Namespace: testAgentNS, Boundary: boundary, Server: "weather", UserHash: "hash-alice",
		}, h)
		assert.Equal(t, http.StatusForbidden, rec.Code, boundary)
		assert.Contains(t, rec.Body.String(), "no namespaced boundary", boundary)
	}
}

// An endpoint that admits no audience is a programming error, refused rather than indexed past.
func TestAuth_NoAudienceIsRefused(t *testing.T) {
	f := newAuthFixture()
	s, _ := fullServer(t, f)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, pathResolve, nil)
	req.Header.Set("Authorization", "Bearer "+f.SidecarToken(testAgentNS, testAgent))
	_, ok := s.authenticate(rec, req)
	assert.False(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, errCodeAuthUnavailable, refusalCode(t, rec))
}
