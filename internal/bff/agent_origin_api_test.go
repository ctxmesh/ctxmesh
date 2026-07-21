package bff

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// M39: at an agent's own hostname (the edge set agentChatboxHeader) only the chatbox's allowlisted
// /api/* endpoints reach the handler; the rest of the console API is 404'd. The console origin (no
// header) is unaffected.
func TestRestrictAgentOriginAPI(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := (&Server{}).restrictAgentOriginAPI(next)

	call := func(method, path string, agentOrigin bool) int {
		reached = false
		r := httptest.NewRequest(method, path, nil)
		if agentOrigin {
			r.Header.Set(agentChatboxHeader, "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// Agent origin: allowlisted endpoints reach the handler.
	allowed := []struct{ m, p string }{
		{"GET", "/api/agents/default/foo"}, // agent detail
		{"POST", "/api/invoke"},
		{"POST", "/api/mcp/oauth/grant"},
		{"GET", "/api/traces/abc123"},
		{"GET", "/api/traces/abc123/detail"},
		{"GET", "/api/authconfig"},
		{"GET", "/api/whoami"},
	}
	for _, tc := range allowed {
		if code := call(tc.m, tc.p, true); code != http.StatusOK || !reached {
			t.Errorf("agent origin %s %s should be allowed, got code=%d reached=%v", tc.m, tc.p, code, reached)
		}
	}

	// Agent origin: everything else is 404'd, and the handler is never reached.
	blocked := []struct{ m, p string }{
		{"GET", "/api/secrets"},
		{"GET", "/api/modelroutes"},
		{"GET", "/api/agentregistries"},
		{"GET", "/api/topology"},
		{"GET", "/api/runs"},
		{"GET", "/api/agents"},                  // the agents LIST
		{"DELETE", "/api/agents/default/foo"},   // a mutation on the detail path
		{"PUT", "/api/agents/default/foo"},      // a mutation on the detail path
		{"GET", "/api/agents/default/foo/logs"}, // a sub-resource
		{"GET", "/api/agents/default/foo/runs"}, // a sub-resource
		{"GET", "/api/extauth"},                 // internal ext-auth endpoint
		{"GET", "/api/mcpservers"},
	}
	for _, tc := range blocked {
		if code := call(tc.m, tc.p, true); code != http.StatusNotFound || reached {
			t.Errorf("agent origin %s %s should be 404, got code=%d reached=%v", tc.m, tc.p, code, reached)
		}
	}

	// Console origin (no header): everything passes through, even the blocked-at-agent endpoints.
	for _, tc := range append(allowed, blocked...) {
		if code := call(tc.m, tc.p, false); code != http.StatusOK || !reached {
			t.Errorf("console origin %s %s should pass, got code=%d reached=%v", tc.m, tc.p, code, reached)
		}
	}
}
