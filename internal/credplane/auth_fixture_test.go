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
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The test fixture for caller authentication: a fake TokenReview per audience and a capability keypair.
// Exported to the external tests through export_test.go.

// fixtureCapabilityAudience is the audience the fixture mints capabilities for.
const fixtureCapabilityAudience = "ctxmesh-credential-plane-test"

// fixturePlatform is the BFF's identity in the fixture.
var fixturePlatform = CallerIdentity{Namespace: "ctxmesh", ServiceAccount: "ctxmesh-bff"}

// fakeReview stands in for TokenReview: a token is valid only for the audience it was issued for.
type fakeReview struct {
	mu      sync.Mutex
	ids     map[string]CallerIdentity
	down    error
	reviews int
}

func (f *fakeReview) Verify(_ context.Context, token string) (CallerIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews++
	if f.down != nil {
		return CallerIdentity{}, f.down
	}
	id, ok := f.ids[token]
	if !ok {
		return CallerIdentity{}, ErrCallerRejected
	}
	return id, nil
}

func (f *fakeReview) issue(token string, id CallerIdentity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[token] = id
}

// authFixture issues pod tokens and run capabilities a test server accepts.
type authFixture struct {
	Auth    CallerAuth
	sidecar *fakeReview
	agent   *fakeReview
	priv    ed25519.PrivateKey
	signer  *runcap.Signer
	other   *runcap.Signer // a key the server does not trust
	seq     int
	mu      sync.Mutex
}

func newAuthFixture() *authFixture {
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		panic(err)
	}
	_, otherPriv, err := runcap.GenerateKeyPair()
	if err != nil {
		panic(err)
	}
	f := &authFixture{
		sidecar: &fakeReview{ids: map[string]CallerIdentity{}},
		agent:   &fakeReview{ids: map[string]CallerIdentity{}},
		priv:    priv,
		signer:  runcap.NewSigner(priv, fixtureCapabilityAudience, nil),
		other:   runcap.NewSigner(otherPriv, fixtureCapabilityAudience, nil),
	}
	f.Auth = CallerAuth{
		Sidecar:         f.sidecar,
		Agent:           f.agent,
		Capabilities:    runcap.NewVerifier(pub, fixtureCapabilityAudience, nil),
		PlatformCallers: []CallerIdentity{fixturePlatform},
	}
	return f
}

// podToken is a JWT-shaped token whose aud claim names audience, as a projected token's does.
func (f *authFixture) podToken(audience string) string {
	f.mu.Lock()
	f.seq++
	n := f.seq
	f.mu.Unlock()
	enc := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"aud": []string{audience}, "n": n})
	if err != nil {
		panic(err)
	}
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc(fmt.Appendf(nil, "sig-%d", n))
}

// SidecarToken is an egress sidecar's token for agent ns/agent.
func (f *authFixture) SidecarToken(ns, agent string) string {
	t := f.podToken(runcap.TokenServicePodAudience)
	f.sidecar.issue(t, CallerIdentity{Namespace: ns, ServiceAccount: AgentServiceAccountPrefix + agent})
	return t
}

// AgentToken is an agent container's token for agent ns/agent.
func (f *authFixture) AgentToken(ns, agent string) string {
	t := f.podToken(runcap.TokenServiceAgentAudience)
	f.agent.issue(t, CallerIdentity{Namespace: ns, ServiceAccount: AgentServiceAccountPrefix + agent})
	return t
}

// BFFToken is the BFF's token.
func (f *authFixture) BFFToken() string {
	t := f.podToken(runcap.TokenServicePodAudience)
	f.sidecar.issue(t, fixturePlatform)
	return t
}

// Capability mints a run capability for user, acted on by actor ("ns/agent"), in boundary.
func (f *authFixture) Capability(user, actor, boundary string) string {
	return f.mint(f.signer, user, actor, boundary, time.Minute)
}

func (f *authFixture) mint(s *runcap.Signer, user, actor, boundary string, ttl time.Duration) string {
	tok, err := s.Mint(runcap.MintRequest{User: user, Agent: actor, Boundary: boundary, RunID: "run-1", TTL: ttl})
	if err != nil {
		panic(err)
	}
	return tok
}

// ExpiredCapability is a capability for the standalone agent ns/agent that expired an hour ago.
func (f *authFixture) ExpiredCapability(user, ns, agent string) string {
	past := runcap.NewSigner(f.priv, fixtureCapabilityAudience, func() time.Time { return time.Now().Add(-time.Hour) })
	return f.mint(past, user, ns+"/"+agent, credresolve.AgentBoundary(ns, agent), time.Minute)
}

// ForgedCapability is a capability for the standalone agent ns/agent signed by a key the server does not trust.
func (f *authFixture) ForgedCapability(user, ns, agent string) string {
	return f.mint(f.other, user, ns+"/"+agent, credresolve.AgentBoundary(ns, agent), time.Minute)
}

// StandaloneCapability is a capability for user of the standalone agent ns/agent.
func (f *authFixture) StandaloneCapability(user, ns, agent string) string {
	return f.Capability(user, ns+"/"+agent, credresolve.AgentBoundary(ns, agent))
}

// testAuth is the fixture shared by the package's handler tests.
var testAuth = newAuthFixture()

// Requests the handler tests send by default come from agent prod/asst.
const (
	testAgentNS = "prod"
	testAgent   = "asst"
)

var errReviewDown = errors.New("tokenreview: connection refused")
