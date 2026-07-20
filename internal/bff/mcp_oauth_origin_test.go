package bff

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The canonical-origin + opener-allowlist logic behind ADR 0040 (cross-origin MCP consent).

func TestOriginsShareBaseDomain(t *testing.T) {
	const console = "https://console.127.0.0.1.sslip.io"
	cases := []struct {
		opener string
		want   bool
	}{
		{"https://console.127.0.0.1.sslip.io", true},                // the console itself
		{"https://scalekit-agent.default.127.0.0.1.sslip.io", true}, // an agent subdomain
		{"https://127.0.0.1.sslip.io", true},                        // the bare base domain
		{"https://evil.example.com", false},                         // a different site
		{"http://console.127.0.0.1.sslip.io", false},                // scheme mismatch
		{"https://sslip.io", false},                                 // not under the base
		{"not a url", false},
	}
	for _, c := range cases {
		if got := originsShareBaseDomain(c.opener, console); got != c.want {
			t.Errorf("originsShareBaseDomain(%q, console) = %v, want %v", c.opener, got, c.want)
		}
	}
}

func TestCanonicalOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/mcp/oauth/grant", nil)
	r.Host = "console.127.0.0.1.sslip.io"

	if got, want := (&Server{}).canonicalOrigin(r), "http://console.127.0.0.1.sslip.io"; got != want {
		t.Errorf("unset consoleURL → want the request origin %q, got %q", want, got)
	}
	if got, want := (&Server{consoleURL: "https://console.example"}).canonicalOrigin(r), "https://console.example"; got != want {
		t.Errorf("set consoleURL → want %q, got %q", want, got)
	}
}

func TestAllowedOpenerOrigin(t *testing.T) {
	const agent = "https://scalekit-agent.default.127.0.0.1.sslip.io"
	s := &Server{consoleURL: "https://console.127.0.0.1.sslip.io"}

	req := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/mcp/oauth/grant", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	if got := s.allowedOpenerOrigin(req(agent)); got != agent {
		t.Errorf("on-domain opener should be relayed, got %q", got)
	}
	if got := s.allowedOpenerOrigin(req("https://evil.example.com")); got != "" {
		t.Errorf("off-domain opener must be dropped, got %q", got)
	}
	if got := s.allowedOpenerOrigin(req("")); got != "" {
		t.Errorf("no Origin header → no relay, got %q", got)
	}
	if got := (&Server{}).allowedOpenerOrigin(req(agent)); got != "" {
		t.Errorf("no consoleURL configured → relay disabled, got %q", got)
	}
}

// The callback carries the validated opener origin so the SPA's popup-close bridge can relay the
// "connected" signal back cross-origin (ADR 0040) — and omits it (same-origin) when there is none.
func TestOAuthCallbackConnectedCarriesOpenerOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, mcpOAuthCallbackPath, nil)

	w := httptest.NewRecorder()
	oauthCallbackConnected(w, r, "scalekit-mcp-server", "https://scalekit-agent.default.example")
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "mcp_connected=scalekit-mcp-server") {
		t.Errorf("Location missing the connected server: %s", loc)
	}
	if !strings.Contains(loc, "opener_origin=https%3A%2F%2Fscalekit-agent.default.example") {
		t.Errorf("Location missing the opener_origin for the cross-origin relay: %s", loc)
	}

	w2 := httptest.NewRecorder()
	oauthCallbackConnected(w2, r, "srv", "")
	if strings.Contains(w2.Header().Get("Location"), "opener_origin") {
		t.Errorf("no opener origin → the param must be absent (same-origin): %s", w2.Header().Get("Location"))
	}
}
