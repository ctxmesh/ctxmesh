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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/propagation"

	"github.com/ctxmesh/ctxmesh/internal/bff"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
	"github.com/ctxmesh/ctxmesh/internal/statelayer"
)

// podTokens reviews pod tokens the way TokenReview would for the BFF audience.
type podTokens map[string]statelayer.PodIdentity

func (p podTokens) Identity(_ context.Context, tok string) (statelayer.PodIdentity, error) {
	id, ok := p[tok]
	if !ok {
		return statelayer.PodIdentity{}, statelayer.ErrTokenRejected
	}
	return id, nil
}

func (p podTokens) Namespace(ctx context.Context, tok string) (string, error) {
	id, err := p.Identity(ctx, tok)
	return id.Namespace, err
}

// The caller's launcher, the BFF's attestation edge and the callee's launcher, each the production code:
// the hop succeeds for a caller on the callee's allowedCallers and is refused for one that is not, and a
// pod the BFF does not know cannot send a hop at all. The registry and role come from the control-plane
// row, so the envelope the launcher builds from its own environment must agree with it.
func TestAttestedHopThroughTheBFFEdge(t *testing.T) {
	t.Parallel()
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps := agentcapability.NewMemStore()
	for _, agent := range []string{"research", "writer"} {
		if err := caps.Set(ctx, agentcapability.AgentCapability{
			Namespace: "team-a", Agent: agent, RegistryID: "research-team", Role: "worker",
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := bff.NewServer(bff.Options{
		MCPCapabilityPrivateSeedB64: runcap.EncodePrivateSeed(priv),
		AgentCapabilities:           caps,
		PodAuth: podTokens{
			"research-pod": {Namespace: "team-a", ServiceAccount: "agent-research"},
			"writer-pod":   {Namespace: "team-a", ServiceAccount: "agent-writer"},
		},
		Log: logr.Discard(),
	})
	edge := httptest.NewServer(server.Handler())
	t.Cleanup(edge.Close)

	callee := baseCfg() // orchestrator in research-team, namespace team-a
	callee.AllowedCallers = []string{"research"}
	member := newMemberProxy(t, callee, runcap.NewVerifier(pub, runcap.DefaultAudience, nil))
	peer := httptest.NewServer(member.handler)
	t.Cleanup(peer.Close)

	call := func(agent, podToken string) *httptest.ResponseRecorder {
		cfg := baseCfg()
		cfg.SelfName, cfg.Role = agent, "worker"
		_, tp := newTestTracer(t)
		s := newAMPServer(cfg, tp.Tracer(tracerName), propagation.TraceContext{}, nil)
		s.attest = newAttestationSource(edge.URL, func() string { return podToken })
		s.resolveHost = func(string) string { return peer.URL }
		return callAMP(t, s, ctx, "orchestrator", `{"q":1}`, nil)
	}

	if rr := call("research", "research-pod"); rr.Code != http.StatusOK {
		t.Fatalf("an allowed member's hop: %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := member.admission(); got != admissionAttested {
		t.Errorf("amp.admission = %q, want attested", got)
	}
	rr := call("writer", "writer-pod")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a member not on allowedCallers: %d, want 403", rr.Code)
	}
	assertAMPError(t, rr.Body.Bytes(), errCallerNotAllowed)

	rr = call("research", "someone-elses-token")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("a pod the BFF does not attest: %d, want 503", rr.Code)
	}
	assertAMPError(t, rr.Body.Bytes(), errAttestationUnavailable)
}

// A hop goes where the platform records its receiver serving, which for an eventing agent is not the
// Knative route the name implies; a receiver with no recorded address keeps the route.
func TestHopGoesToTheAddressThePlatformRecords(t *testing.T) {
	t.Parallel()
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	member := newMemberProxy(t, baseCfg(), runcap.NewVerifier(pub, runcap.DefaultAudience, nil))
	served := httptest.NewServer(member.handler)
	t.Cleanup(served.Close)
	var routeHits atomic.Int32
	route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeHits.Add(1)
		member.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(route.Close)

	for name, recorded := range map[string]string{"recorded": served.URL, "none recorded": ""} {
		t.Run(name, func(t *testing.T) {
			caps := agentcapability.NewMemStore()
			for agent, endpoint := range map[string]string{"research": "", "orchestrator": recorded} {
				if err := caps.Set(context.Background(), agentcapability.AgentCapability{
					Namespace: "team-a", Agent: agent, RegistryID: "research-team", Role: "worker", Endpoint: endpoint,
				}); err != nil {
					t.Fatal(err)
				}
			}
			edge := httptest.NewServer(bff.NewServer(bff.Options{
				MCPCapabilityPrivateSeedB64: runcap.EncodePrivateSeed(priv),
				AgentCapabilities:           caps,
				PodAuth:                     podTokens{"research-pod": {Namespace: "team-a", ServiceAccount: "agent-research"}},
				Log:                         logr.Discard(),
			}).Handler())
			t.Cleanup(edge.Close)

			cfg := baseCfg()
			cfg.SelfName, cfg.Role = "research", "worker"
			_, tp := newTestTracer(t)
			s := newAMPServer(cfg, tp.Tracer(tracerName), propagation.TraceContext{}, nil)
			s.attest = newAttestationSource(edge.URL, func() string { return "research-pod" })
			s.resolveHost = func(string) string { return route.URL }

			before := routeHits.Load()
			if rr := callAMP(t, s, context.Background(), "orchestrator", `{"q":1}`, nil); rr.Code != http.StatusOK {
				t.Fatalf("hop: %d; body=%s", rr.Code, rr.Body.String())
			}
			if usedRoute := routeHits.Load() > before; usedRoute != (recorded == "") {
				t.Errorf("the hop used the Knative route: %v; want %v", usedRoute, recorded == "")
			}
		})
	}
}
