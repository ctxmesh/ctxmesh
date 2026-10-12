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
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
	"github.com/ctxmesh/ctxmesh/internal/statelayer"
)

// tokenPodAuth reviews tokens the way TokenReview does: a known token is a ServiceAccount, an unknown
// one is rejected, and infra fails every review.
type tokenPodAuth struct {
	ids   map[string]statelayer.PodIdentity
	infra error
}

func (a tokenPodAuth) Identity(_ context.Context, tok string) (statelayer.PodIdentity, error) {
	if a.infra != nil {
		return statelayer.PodIdentity{}, a.infra
	}
	id, ok := a.ids[tok]
	if !ok {
		return statelayer.PodIdentity{}, statelayer.ErrTokenRejected
	}
	return id, nil
}

func (a tokenPodAuth) Namespace(ctx context.Context, tok string) (string, error) {
	id, err := a.Identity(ctx, tok)
	return id.Namespace, err
}

type failingCapabilities struct{ agentcapability.Store }

func (failingCapabilities) Get(context.Context, string, string) (agentcapability.AgentCapability, bool, error) {
	return agentcapability.AgentCapability{}, false, errors.New("postgres is down")
}

// newAttestationServer serves the edge for three pods: a registry member, an agent in no registry, and
// a pod that is not an agent.
func newAttestationServer(t *testing.T) (*Server, *runcap.Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	caps := agentcapability.NewMemStore()
	require.NoError(t, caps.Set(context.Background(), agentcapability.AgentCapability{
		Namespace: "team-a", Agent: "planner", RegistryID: "research", Role: "orchestrator",
	}))
	require.NoError(t, caps.Set(context.Background(), agentcapability.AgentCapability{
		Namespace: "team-b", Agent: "planner", RegistryID: "research-b", Role: "worker",
	}))
	require.NoError(t, caps.Set(context.Background(), agentcapability.AgentCapability{
		Namespace: "team-a", Agent: "described", Description: "advertises, belongs to no registry",
	}))
	return &Server{
		capabilitySigner:  runcap.NewSigner(priv, "aud", nil),
		agentCapabilities: caps,
		podAuth: tokenPodAuth{ids: map[string]statelayer.PodIdentity{
			"planner-token":   {Namespace: "team-a", ServiceAccount: "agent-planner"},
			"described-token": {Namespace: "team-a", ServiceAccount: "agent-described"},
			"loner-token":     {Namespace: "team-a", ServiceAccount: "agent-loner"},
			"default-token":   {Namespace: "team-a", ServiceAccount: "default"},
			"other-ns-token":  {Namespace: "team-b", ServiceAccount: "agent-planner"},
			"bare-sa-token":   {Namespace: "team-a", ServiceAccount: "planner"},
		}},
		log: logr.Discard(),
	}, runcap.NewVerifier(pub, "aud", nil)
}

func postAttestation(t *testing.T, s *Server, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ampAttestationPath, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The attestation names the pod's own agent and namespace, and the registry and role the platform
// recorded for it. A request that claims otherwise changes nothing.
func TestAMPAttestation_ClaimsComeFromThePlatform(t *testing.T) {
	s, verifier := newAttestationServer(t)
	rec := postAttestation(t, s, "Bearer planner-token",
		`{"receiver":"writer","namespace":"team-b","agent":"admin","registryId":"other","role":"orchestrator"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var resp ampAttestationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(300), resp.ExpiresIn)
	att, err := verifier.VerifyAttestation(resp.Attestation)
	require.NoError(t, err)
	assert.Equal(t, "team-a", att.Namespace, "from the reviewed token")
	assert.Equal(t, "planner", att.Agent, "from the token's ServiceAccount")
	assert.Equal(t, "research", att.RegistryID, "from the capability registry")
	assert.Equal(t, "orchestrator", att.Role, "from the capability registry")
	assert.Equal(t, "writer", att.Receiver, "good only at the agent it was asked for")
	assert.WithinDuration(t, time.Now().Add(ampAttestationTTL), att.ExpiresAt, 5*time.Second)

	// The same agent name in another namespace is that namespace's agent, with that agent's registry.
	rec = postAttestation(t, s, "Bearer other-ns-token", `{"receiver":"reader"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	att, err = verifier.VerifyAttestation(resp.Attestation)
	require.NoError(t, err)
	assert.Equal(t, "team-b", att.Namespace)
	assert.Equal(t, "research-b", att.RegistryID)
	assert.Equal(t, "worker", att.Role)
	assert.Equal(t, "reader", att.Receiver)
}

func TestAMPAttestation_Refusals(t *testing.T) {
	s, _ := newAttestationServer(t)
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"not a bearer", "planner-token", http.StatusUnauthorized},
		{"a token the review rejects", "Bearer forged", http.StatusUnauthorized},
		{"a pod that is not an agent", "Bearer default-token", http.StatusForbidden},
		{"an agent with no registry row", "Bearer loner-token", http.StatusForbidden},
		{"an agent whose row names no registry", "Bearer described-token", http.StatusForbidden},
		{"a ServiceAccount named like an agent", "Bearer bare-sa-token", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postAttestation(t, s, tc.auth, `{"receiver":"writer"}`)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "attestation\":\"", "no attestation on a refusal")
		})
	}
}

// A review or a registry that cannot answer is the platform's failure, not the pod's: 503, so the
// launcher retries rather than concluding it is unknown.
func TestAMPAttestation_UnavailableIsRetryable(t *testing.T) {
	s, _ := newAttestationServer(t)
	s.podAuth = tokenPodAuth{infra: fmt.Errorf("tokenreview: %w", errors.New("connection refused"))}
	assert.Equal(t, http.StatusServiceUnavailable,
		postAttestation(t, s, "Bearer planner-token", `{"receiver":"writer"}`).Code)

	s, _ = newAttestationServer(t)
	s.agentCapabilities = failingCapabilities{}
	assert.Equal(t, http.StatusServiceUnavailable,
		postAttestation(t, s, "Bearer planner-token", `{"receiver":"writer"}`).Code)
}

// An attestation names its receiver, so a request must name an agent: never none, never a pattern.
func TestAMPAttestation_NeedsAReceiver(t *testing.T) {
	s, _ := newAttestationServer(t)
	for _, body := range []string{
		"", "{}", `{"receiver":""}`, `{"receiver":"Writer"}`, `{"receiver":"*"}`,
		`{"receiver":"writer.team-b"}`, `{"receiver":"` + strings.Repeat("a", 64) + `"}`, "not json",
	} {
		rec := postAttestation(t, s, "Bearer planner-token", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body %q", body)
	}
}

func TestAMPAttestation_RouteNeedsAllThreeDependencies(t *testing.T) {
	for name, strip := range map[string]func(*Server){
		"signer":   func(s *Server) { s.capabilitySigner = nil },
		"pod auth": func(s *Server) { s.podAuth = nil },
		"registry": func(s *Server) { s.agentCapabilities = nil },
	} {
		s, _ := newAttestationServer(t)
		strip(s)
		mux := http.NewServeMux()
		s.registerAMPAttestationRoute(mux)
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, ampAttestationPath, nil))
		assert.Empty(t, pattern, "no %s, no edge", name)
	}
}

// countingPodAuth counts the reviews it is asked for.
type countingPodAuth struct {
	tokenPodAuth
	reviews *int
}

func (a countingPodAuth) Identity(ctx context.Context, tok string) (statelayer.PodIdentity, error) {
	*a.reviews++
	return a.tokenPodAuth.Identity(ctx, tok)
}

// A token the review rejected is refused again without another review, so repeating it costs the API
// server nothing; a token that authenticates is unaffected.
func TestAMPAttestation_ARejectedTokenIsNotReviewedAgain(t *testing.T) {
	s, _ := newAttestationServer(t)
	reviews := 0
	s.podAuth = countingPodAuth{tokenPodAuth: s.podAuth.(tokenPodAuth), reviews: &reviews}

	for range 3 {
		assert.Equal(t, http.StatusUnauthorized, postAttestation(t, s, "Bearer forged", `{"receiver":"writer"}`).Code)
	}
	assert.Equal(t, 1, reviews, "a rejected token is reviewed once")
	assert.Equal(t, http.StatusOK, postAttestation(t, s, "Bearer planner-token", `{"receiver":"writer"}`).Code)
	assert.Equal(t, http.StatusOK, postAttestation(t, s, "Bearer planner-token", `{"receiver":"writer"}`).Code)
	assert.Equal(t, 3, reviews, "an authenticated token is never refused from the record")
}

func TestRefusedPodTokensExpireAndStayBounded(t *testing.T) {
	var c refusedPodTokens
	now := time.Now()
	assert.False(t, c.refused("t", now))
	c.refuse("t", now)
	assert.True(t, c.refused("t", now.Add(refusedPodTokenTTL-time.Second)))
	assert.False(t, c.refused("t", now.Add(refusedPodTokenTTL)), "a refusal is remembered for its TTL only")

	for i := range maxRefusedPodTokens + 10 {
		c.refuse(fmt.Sprint(i), now)
	}
	assert.LessOrEqual(t, len(c.until), maxRefusedPodTokens)
}
