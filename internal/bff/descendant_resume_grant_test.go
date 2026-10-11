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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// TestResumedSubRunUsesTheGrantGivenInTheChat: a supervisor delegates to a sub-agent outside any
// registry, the sub-run pauses for consent, and the person connects from the chat, which scopes the
// grant to the agent they are talking to (prepareGrantConsent with the chat's agent). Resuming the
// sub-run mints its capability in the sub-run's boundary, the parent's, so the credential plane
// resolves that grant for it; minted in the sub-agent's own boundary it would find nothing and the
// run would ask again.
func TestResumedSubRunUsesTheGrantGivenInTheChat(t *testing.T) {
	ctx := context.Background()
	const server = "search-mcp"
	c := identityClient(t, aliceUser,
		readyAgent("supervisor", "prod", goldenSupervisorEndpoint),
		readyAgent("researcher", "prod", goldenResearcherEndpoint),
	)
	inv := &scriptedInvoke{replies: map[string][]scriptedReply{
		goldenSupervisorEndpoint: {{body: `{"delegate_waiting":{"checkpoint":"cp-1","delegates":[{"sub_agent":"researcher","endpoint":"` +
			goldenResearcherEndpoint + `","input":{"input":"find sources"},"step":"1","call_id":"call-1"}]}}`}},
		goldenResearcherEndpoint: {
			{body: `{"consent_required":["` + server + `"]}`},
			{body: `{"output":"3 sources"}`},
		},
	}}
	s := newInvokeServerNoWorkers(t, newFakeFactory(c), inv)
	_, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, "credential-plane", nil)
	s.capabilitySigner = signer

	// The supervisor's run, as the chat creates it: the caller's identity and the agent's boundary.
	chatBoundary := agentBoundary(ctx, c, "prod", "supervisor")
	root := run.New("run-supervisor", "prod", "supervisor", []byte(`{"input":"research"}`), "", time.Now())
	root.CallerUsername, root.Boundary = aliceUser, chatBoundary
	require.NoError(t, s.runStore.Create(root))
	s.executeRun(ctx, root.ID, goldenSupervisorEndpoint, []byte(`{"input":"research"}`))
	subID := run.SpawnRunID(root.ID, "1", "call-1")
	s.executeRun(ctx, subID, goldenResearcherEndpoint, []byte(`{"input":"find sources"}`))
	sub, err := s.runStore.Get(subID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRequiresAction, sub.Status, sub.Error)
	require.Equal(t, chatBoundary, sub.Boundary, "a sub-run inherits its parent's boundary")
	require.NotEqual(t, chatBoundary, agentBoundary(ctx, c, "prod", "researcher"),
		"the sub-agent is outside any registry, so its own boundary differs")

	// Connect from the chat stores the grant in the chat agent's boundary.
	grants := credresolve.NewK8sBackend(credresolve.K8sBackendConfig{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	})
	require.NoError(t, grants.StoreGrant(ctx, "prod", chatBoundary, server, userGrantHash(aliceUser), credresolve.Grant{
		Tokens: credresolve.Tokens{AccessToken: "alice-search-token"},
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+subID+"/resume", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var token string
	require.Eventually(t, func() bool {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		if caps := inv.caps[goldenResearcherEndpoint]; len(caps) == 2 {
			token = caps[1]
			return true
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the resumed sub-run is invoked")
	require.NotEmpty(t, token, "the resumed sub-run carries a capability")

	capab, err := signer.Verifier().Verify(token)
	require.NoError(t, err)
	assert.Equal(t, sub.Boundary, capab.Boundary, "minted in the run's boundary")
	assert.Equal(t, subID, capab.RunID)
	cred, err := grants.Resolve(ctx, "prod", capab.Boundary, server, capab.User)
	require.NoError(t, err, "the credential plane finds the grant given in the chat")
	assert.Equal(t, "alice-search-token", cred.Value)
}
