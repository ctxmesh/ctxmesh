package bff

import (
	"net/http"
	"net/http/httptest"
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
