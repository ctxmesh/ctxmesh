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

// Package enduseroidc verifies end-user OIDC ID tokens against a tenant's issuer (M137/EU1b, ADR 0106
// §8). It is the trusted-STS verification half of the implicit RFC 8693 token-exchange: a verified
// (iss, sub) is what the BFF hashes into a run-capability. It wraps github.com/coreos/go-oidc (JWKS +
// discovery + signature/iss/aud/exp checks) and adds the checklist go-oidc does not enforce by itself:
// an explicit signing-algorithm allowlist (reject none/HS*), the OIDC azp check for a multi-audience
// token, and — critically — an SSRF guard on the discovery + JWKS fetches (the issuer URL is
// tenant-admin-supplied). It caches one provider per issuer (discovery once; the JWKS keyset then
// self-refreshes, rate-limited, on an unknown kid).
package enduseroidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// allowedAlgs is the signing-algorithm allowlist (ADR 0106 §8). Asymmetric only — `none` and every
// HMAC (`HS*`) are rejected STRUCTURALLY (an HS token verified against a public JWKS is the classic
// alg-confusion forgery). go-oidc enforces this set via oidc.Config.SupportedSigningAlgs.
var allowedAlgs = []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.EdDSA}

// Identity is the verified end-user identity. The identity KEY is (Issuer, Subject) — NEVER Email or
// PreferredUsername (both are mutable/reassignable at the IdP; keying on them is a cross-tenant
// account-takeover bug, ADR 0106 §5). Email/PreferredUsername are carried for display/audit only.
type Identity struct {
	Issuer            string
	Subject           string
	Email             string
	PreferredUsername string
}

// Options configures the verifier.
type Options struct {
	// AllowPrivateIssuer permits an issuer that resolves to an RFC1918 / IPv6-ULA private address
	// (an in-cluster IdP, e.g. a self-run dex Service). Default false — the secure posture for tenants
	// whose IdP is a public SaaS (Okta/Entra/Google). Loopback, link-local (incl. the cloud-metadata
	// 169.254.169.254), multicast and unspecified addresses are ALWAYS denied regardless of this.
	AllowPrivateIssuer bool
	// AllowLoopback permits a loopback issuer over http as well (a dev/test dex on 127.0.0.1). Default
	// false. Dev only — never enable in production.
	AllowLoopback bool
	// HTTPTimeout bounds each discovery/JWKS fetch. Default 10s.
	HTTPTimeout time.Duration
	// Now overrides the clock (tests). Default time.Now.
	Now func() time.Time
}

// Verifier verifies end-user ID tokens. Safe for concurrent use.
type Verifier struct {
	mu        sync.Mutex
	providers map[string]*oidc.Provider // issuer → provider (discovery cached; keyset self-refreshes)
	client    *http.Client              // SSRF-guarded; used for discovery + JWKS
	allowHTTP bool                      // permit an http issuer (dev: a private/loopback in-cluster IdP)
	now       func() time.Time
}

// NewVerifier builds a verifier with an SSRF-guarded HTTP client per the options.
func NewVerifier(opts Options) *Verifier {
	timeout := opts.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		providers: make(map[string]*oidc.Provider),
		client:    ssrfGuardedClient(opts.AllowPrivateIssuer, opts.AllowLoopback, timeout),
		// http is allowed only under a DEV posture (a private/in-cluster or loopback IdP); production
		// (no dev flags) requires https. The SSRF dial guard still enforces the IP restrictions.
		allowHTTP: opts.AllowLoopback || opts.AllowPrivateIssuer,
		now:       now,
	}
}

// Verify verifies rawIDToken against the tenant's (issuer, clientID) and returns the end-user Identity.
// It enforces: exact issuer match (config == discovery == token `iss`, via go-oidc), signature over the
// discovery JWKS, the algorithm allowlist, `aud` ∋ clientID, `azp` == clientID when multi-audience, and
// a non-empty `sub`. Any failure returns an error (fail-closed — the caller mints no capability).
func (v *Verifier) Verify(ctx context.Context, issuer, clientID, rawIDToken string) (Identity, error) {
	if strings.TrimSpace(clientID) == "" {
		return Identity{}, errors.New("enduseroidc: clientID is required")
	}
	provider, err := v.provider(issuer)
	if err != nil {
		return Identity{}, err
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: allowedAlgs,
		Now:                  v.now,
	})
	tok, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("enduseroidc: id-token verification failed: %w", err)
	}
	var claims struct {
		Aud   audience `json:"aud"`
		AZP   string   `json:"azp"`
		Email string   `json:"email"`
		PU    string   `json:"preferred_username"`
	}
	if cErr := tok.Claims(&claims); cErr != nil {
		return Identity{}, fmt.Errorf("enduseroidc: decode claims: %w", cErr)
	}
	// OIDC Core §3.1.3.7: with more than one audience the token MUST carry azp, and azp MUST be the
	// client. go-oidc only checks aud ∋ clientID, so enforce the multi-aud azp rule here.
	if len(claims.Aud) > 1 && claims.AZP != clientID {
		return Identity{}, errors.New("enduseroidc: multi-audience token requires azp == clientID")
	}
	if strings.TrimSpace(tok.Subject) == "" {
		return Identity{}, errors.New("enduseroidc: token has no subject")
	}
	return Identity{
		Issuer:            tok.Issuer,
		Subject:           tok.Subject,
		Email:             claims.Email,
		PreferredUsername: claims.PU,
	}, nil
}

// provider returns a cached go-oidc Provider for the issuer, doing discovery (through the SSRF-guarded
// client) on first use. go-oidc validates the discovery document's `issuer` equals the requested issuer
// (exact string) — the config==discovery half of the checklist.
func (v *Verifier) provider(issuer string) (*oidc.Provider, error) {
	iss := strings.TrimSpace(issuer)
	if err := v.validateIssuerURL(iss); err != nil {
		return nil, err
	}
	v.mu.Lock()
	p, ok := v.providers[iss]
	v.mu.Unlock()
	if ok {
		return p, nil
	}
	// Discovery carries the SSRF-guarded client on the context; go-oidc captures it for the keyset's
	// later JWKS fetches too. A detached (Background) context so the cached keyset isn't bound to this
	// request's deadline, but with a bound so a hung discovery can't wedge the caller.
	dctx, cancel := context.WithTimeout(oidc.ClientContext(context.Background(), v.client), 20*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(dctx, iss)
	if err != nil {
		return nil, fmt.Errorf("enduseroidc: OIDC discovery for %q failed: %w", iss, err)
	}
	v.mu.Lock()
	v.providers[iss] = p
	v.mu.Unlock()
	return p, nil
}

// validateIssuerURL requires https (a loopback http issuer is allowed only when AllowLoopback — dev).
func (v *Verifier) validateIssuerURL(issuer string) error {
	if issuer == "" {
		return errors.New("enduseroidc: issuer is empty")
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return fmt.Errorf("enduseroidc: invalid issuer URL %q", issuer)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && v.allowHTTP {
		return nil // dev posture: a private/in-cluster or loopback IdP over http (dial guard still applies)
	}
	return fmt.Errorf("enduseroidc: issuer must be https (got scheme %q)", u.Scheme)
}

// audience decodes the OIDC `aud` claim, which may be a single string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// ssrfGuardedClient builds an http.Client whose dialer rejects any connection to a denied IP AFTER DNS
// resolution (the resolved IP is checked, so DNS-rebinding to a metadata/internal address is caught).
// Proxies-from-env are disabled (a proxy is itself an SSRF vector); redirects are bounded + https-pinned.
func ssrfGuardedClient(allowPrivate, allowLoopback bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("enduseroidc: bad dial address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("enduseroidc: could not parse dialed IP %q", host)
		}
		if denied := deniedIP(ip, allowPrivate, allowLoopback); denied != "" {
			return fmt.Errorf("enduseroidc: issuer resolves to a denied address %s (%s) — SSRF guard", ip, denied)
		}
		return nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // never honor HTTP(S)_PROXY for issuer fetches
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("enduseroidc: too many redirects on issuer fetch")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("enduseroidc: refusing non-https redirect to %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// deniedIP returns a non-empty reason when the IP must not be dialed for an issuer fetch. Loopback,
// link-local (incl. 169.254.169.254 cloud metadata), multicast and unspecified are ALWAYS denied
// (loopback only relaxed for AllowLoopback dev); RFC1918 / IPv6-ULA private is denied unless
// AllowPrivateIssuer.
func deniedIP(ip net.IP, allowPrivate, allowLoopback bool) string {
	switch {
	case ip.IsLoopback():
		if allowLoopback {
			return ""
		}
		return "loopback"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "link-local/metadata"
	case ip.IsMulticast():
		return "multicast"
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsPrivate():
		if allowPrivate {
			return ""
		}
		return "private"
	}
	return ""
}
