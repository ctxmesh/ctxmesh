package bff

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

func TestParseAgentFromHost(t *testing.T) {
	cases := []struct{ in, agent, ns string }{
		{"scalekit-agent.default.127.0.0.1.sslip.io", "scalekit-agent", "default"},
		{"chat-agent.prod.agents.example.com:443", "chat-agent", "prod"},
		{"console.127.0.0.1.sslip.io", "console", "127"}, // 2 labels min — caller only puts this on agent routes
		{"localhost", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		a, n := parseAgentFromHost(c.in)
		if a != c.agent || n != c.ns {
			t.Errorf("parseAgentFromHost(%q) = (%q,%q), want (%q,%q)", c.in, a, n, c.agent, c.ns)
		}
	}
}

// TestRegisterExtAuthRoutes_Subtree locks in the fix for the Envoy path_prefix gotcha (ADR 0039):
// Envoy's HTTP ext_authz PREPENDS its path_prefix ("/api/extauth") to the ORIGINAL request path, so
// the auth request arrives as /api/extauth/<original> (e.g. /api/extauth/invoke). The endpoint must
// therefore match the whole /api/extauth/ subtree — matching only the exact path silently 404s every
// real request while direct probes to /api/extauth still pass (a deceptive green). Assert via
// ServeMux.Handler (route lookup only — the handler is never invoked, so no live deps are needed).
func TestRegisterExtAuthRoutes_Subtree(t *testing.T) {
	s := &Server{callerClients: &fakeCallerClientFactory{}, capabilitySigner: &runcap.Signer{}}
	mux := http.NewServeMux()
	s.registerExtAuthRoutes(mux)

	for _, path := range []string{"/api/extauth", "/api/extauth/", "/api/extauth/invoke", "/api/extauth/a/b"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("no ext-auth handler for %q — Envoy prepends path_prefix, so the subtree must match", path)
		}
	}
}

// TestRegisterExtAuthRoutes_Guarded: with no caller-client seam and no signer (a local, no-cluster
// substrate) the routes must NOT be registered — there is nothing to authenticate against or mint.
func TestRegisterExtAuthRoutes_Guarded(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).registerExtAuthRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/extauth", nil)
	if _, pattern := mux.Handler(req); pattern != "" {
		t.Errorf("ext-auth registered without the caller-client seam + signer (pattern %q)", pattern)
	}
}
