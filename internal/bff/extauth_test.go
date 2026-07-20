package bff

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestAgentPinForRequest: the SPA is pinned to the host's agent ONLY when the edge set the chatbox
// flag (m37.3); the agent is read from the forwarded host, else "".
func TestAgentPinForRequest(t *testing.T) {
	noFlag := httptest.NewRequest(http.MethodGet, "/", nil)
	noFlag.Host = "scalekit-agent.default.127.0.0.1.sslip.io"
	if pin := agentPinForRequest(noFlag); pin != "" {
		t.Errorf("no chatbox flag → want empty pin (console origin), got %q", pin)
	}

	flagged := httptest.NewRequest(http.MethodGet, "/", nil)
	flagged.Host = "scalekit-agent.default.127.0.0.1.sslip.io"
	flagged.Header.Set(agentChatboxHeader, "1")
	if got, want := agentPinForRequest(flagged), "default/scalekit-agent"; got != want {
		t.Errorf("agentPinForRequest = %q, want %q", got, want)
	}

	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.Host = "agent-engine-bff:9090" // internal svc host — must be ignored
	forwarded.Header.Set(agentChatboxHeader, "1")
	forwarded.Header.Set("X-Forwarded-Host", "sk-agent.prod.agents.example.com")
	if got, want := agentPinForRequest(forwarded), "prod/sk-agent"; got != want {
		t.Errorf("forwarded-host pin = %q, want %q", got, want)
	}
}

// TestInjectAgentPin: the pin meta lands at the start of <head>, and a shell with no <head> is
// returned unchanged (best-effort — the app then falls back to the console router).
func TestInjectAgentPin(t *testing.T) {
	const meta = `<meta name="agent-pin" content="default/scalekit-agent">`
	out := string(injectHeadMeta([]byte("<html><head><title>x</title></head></html>"), "agent-pin", "default/scalekit-agent"))
	if !strings.Contains(out, "<head>"+meta) {
		t.Errorf("meta not injected at head start: %s", out)
	}
	noHead := []byte("<html>hi</html>")
	if got := injectHeadMeta(noHead, "agent-pin", "a/b"); string(got) != string(noHead) {
		t.Errorf("no <head> should be unchanged, got %s", got)
	}
}
