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
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// mint is a capability for audience signed with priv.
func mint(t *testing.T, priv ed25519.PrivateKey, audience string) string {
	t.Helper()
	tok, err := runcap.NewSigner(priv, audience, nil).Mint(runcap.MintRequest{User: "u", RunID: "r", TTL: time.Minute})
	require.NoError(t, err)
	return tok
}

// reviewing is a clientset whose TokenReview authenticates "good:<audience>" for that audience only, as
// the given ServiceAccount, and fails outright when down.
func reviewing(sa string, down bool) *fake.Clientset {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if down {
			return true, nil, errors.New("connection refused")
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		out := tr.DeepCopy()
		if len(tr.Spec.Audiences) == 1 && tr.Spec.Token == "good:"+tr.Spec.Audiences[0] {
			out.Status = authv1.TokenReviewStatus{
				Authenticated: true, Audiences: tr.Spec.Audiences,
				User: authv1.UserInfo{Username: "system:serviceaccount:" + sa},
			}
		}
		return true, out, nil
	})
	return cs
}

func TestCallerAuth_ReviewsEachAudienceAndMapsItsAnswers(t *testing.T) {
	pub, _, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	env := envOf(map[string]string{envCapabilityPublicKey: runcap.EncodePublicKey(pub)})

	auth, err := callerAuth(logr.Discard(), reviewing("team-a:agent-helper", false), env)
	require.NoError(t, err)
	for _, tc := range []struct {
		v           credplane.TokenVerifier
		own, others string
	}{
		{auth.Sidecar, runcap.TokenServicePodAudience, runcap.TokenServiceAgentAudience},
		{auth.Agent, runcap.TokenServiceAgentAudience, runcap.TokenServicePodAudience},
	} {
		id, err := tc.v.Verify(context.Background(), "good:"+tc.own)
		require.NoError(t, err, tc.own)
		assert.Equal(t, credplane.CallerIdentity{Namespace: "team-a", ServiceAccount: "agent-helper"}, id)
		_, err = tc.v.Verify(context.Background(), "good:"+tc.others)
		assert.ErrorIs(t, err, credplane.ErrCallerRejected, "%s: a token for the other audience is refused", tc.own)
		_, err = tc.v.Verify(context.Background(), "bad")
		assert.ErrorIs(t, err, credplane.ErrCallerRejected, "a rejected token is a 401")
	}

	down, err := callerAuth(logr.Discard(), reviewing("team-a:agent-helper", true), env)
	require.NoError(t, err)
	_, err = down.Sidecar.Verify(context.Background(), "good:"+runcap.TokenServicePodAudience)
	require.Error(t, err)
	assert.NotErrorIs(t, err, credplane.ErrCallerRejected, "an unreachable review is a 503, not a 401")
}

func TestCallerAuth_TheCapabilityKey(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)

	t.Run("verifies with the key and audience", func(t *testing.T) {
		auth, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(map[string]string{
			envCapabilityPublicKey: runcap.EncodePublicKey(pub), envCapabilityAudience: "aud-x",
		}))
		require.NoError(t, err)
		require.NotNil(t, auth.Capabilities)
		_, err = auth.Capabilities.Verify(mint(t, priv, "aud-x"))
		assert.NoError(t, err)
		_, err = auth.Capabilities.Verify(mint(t, priv, runcap.DefaultAudience))
		assert.ErrorIs(t, err, runcap.ErrWrongAudience)
	})
	t.Run("defaults the audience", func(t *testing.T) {
		auth, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(map[string]string{
			envCapabilityPublicKey: runcap.EncodePublicKey(pub),
		}))
		require.NoError(t, err)
		_, err = auth.Capabilities.Verify(mint(t, priv, runcap.DefaultAudience))
		assert.NoError(t, err)
	})
	t.Run("a malformed key refuses to start", func(t *testing.T) {
		_, err := callerAuth(logr.Discard(), reviewing("x:y", false),
			envOf(map[string]string{envCapabilityPublicKey: "not-a-key"}))
		require.ErrorContains(t, err, envCapabilityPublicKey)
	})
	t.Run("no key serves, refusing what needs one", func(t *testing.T) {
		auth, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(nil))
		require.NoError(t, err)
		assert.Nil(t, auth.Capabilities)
	})
	t.Run("no key with OBO required refuses to start", func(t *testing.T) {
		_, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(map[string]string{envOBORequired: "true"}))
		require.ErrorContains(t, err, envCapabilityPublicKey)
	})
}

func TestCallerAuth_PlatformCallersAreInTheServicesNamespace(t *testing.T) {
	auth, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(map[string]string{
		envPodNamespace: "ctxmesh", envPlatformServiceAccounts: " ctxmesh-bff, ,other ",
	}))
	require.NoError(t, err)
	assert.Equal(t, []credplane.CallerIdentity{
		{Namespace: "ctxmesh", ServiceAccount: "ctxmesh-bff"},
		{Namespace: "ctxmesh", ServiceAccount: "other"},
	}, auth.PlatformCallers)

	_, err = callerAuth(logr.Discard(), reviewing("x:y", false),
		envOf(map[string]string{envPlatformServiceAccounts: "ctxmesh-bff"}))
	require.ErrorContains(t, err, envPodNamespace)

	none, err := callerAuth(logr.Discard(), reviewing("x:y", false), envOf(nil))
	require.NoError(t, err)
	assert.Empty(t, none.PlatformCallers, "no platform caller unless named")
}
