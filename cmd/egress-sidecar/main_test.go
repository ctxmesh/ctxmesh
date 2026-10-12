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
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
)

// TestEgressSidecar_DefaultBindIsLoopback pins the ADR 0072 invariant (m79.3): the egress sidecar —
// the credential-plane proxy that VERIFIES the run capability and injects the invoking user's OBO
// bearer — binds LOOPBACK (127.0.0.1) by default, so its runcap verifier is not reachable off-pod
// without an explicit EGRESS_LISTEN_ADDR override. This is one of the two guardrails that make
// deferring the LTM audience-separation (C9) safe: a stolen runcap cannot be replayed against the
// credential plane from another pod. A routable default (e.g. 0.0.0.0:8081) would silently reopen
// C9's third reopen trigger, so this test FAILS if the default ever becomes non-loopback.
func TestEgressSidecar_DefaultBindIsLoopback(t *testing.T) {
	if !strings.HasPrefix(defaultListenAddr, "127.") {
		t.Fatalf("egress sidecar default bind %q is not loopback (127.x) — a routable default would expose "+
			"the credential-plane runcap verifier off-pod (ADR 0072)", defaultListenAddr)
	}
}

// Without the token service the sidecar has nowhere to resolve a credential, so it refuses to start
// rather than serve tool calls it cannot authorize.
func TestBuildResolverRequiresTheTokenService(t *testing.T) {
	_, err := buildResolver(logr.Discard(), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "TOKEN_SERVICE_URL is required") {
		t.Fatalf("no TOKEN_SERVICE_URL: want a startup error naming it, got %v", err)
	}

	env := map[string]string{"TOKEN_SERVICE_URL": "http://token-service.ctxmesh.svc:8443"}
	r, err := buildResolver(logr.Discard(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("with TOKEN_SERVICE_URL: %v", err)
	}
	if _, ok := r.(*credplane.Client); !ok {
		t.Fatalf("resolver is %T, want the token-service client", r)
	}
}
