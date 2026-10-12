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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// Caller authentication (ADR 0154). Every endpoint but the health probes takes a projected ServiceAccount
// token as a Bearer token, reviewed for the audience that endpoint admits. Who the service acts for comes
// from the reviewed identity and, where a user is involved, from the run capability the caller forwards.
// A body field that disagrees with either is refused, never replaced.

// AgentServiceAccountPrefix prefixes every agent's identity ServiceAccount ("agent-<name>"); it must match
// the controller's agentIdentitySAName.
const AgentServiceAccountPrefix = "agent-"

// ErrCallerRejected is returned by a TokenVerifier for a token that is invalid for its audience (401).
// Any other error means the review itself could not be completed (503).
var ErrCallerRejected = errors.New("credplane: caller token rejected")

// CallerIdentity is a reviewed pod principal: the ServiceAccount the token was issued to.
type CallerIdentity struct {
	Namespace      string
	ServiceAccount string
}

func (c CallerIdentity) String() string { return c.Namespace + "/" + c.ServiceAccount }

// agentName is the agent a caller's ServiceAccount names, or "" when it is not an agent's.
func (c CallerIdentity) agentName() string {
	name, ok := strings.CutPrefix(c.ServiceAccount, AgentServiceAccountPrefix)
	if !ok {
		return ""
	}
	return name
}

// TokenVerifier reviews a projected ServiceAccount token for one audience.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (CallerIdentity, error)
}

// CallerAuth is what the service needs to authenticate its callers.
type CallerAuth struct {
	// Sidecar reviews tokens for runcap.TokenServicePodAudience: the egress sidecar and the BFF.
	Sidecar TokenVerifier
	// Agent reviews tokens for runcap.TokenServiceAgentAudience: an agent's own container.
	Agent TokenVerifier
	// Capabilities verifies run capabilities. nil answers every request that needs one with 503, so an
	// install whose capability key does not exist yet still serves what needs none.
	Capabilities *runcap.Verifier
	// PlatformCallers may store and revoke grants and search any namespace's knowledge (the BFF).
	PlatformCallers []CallerIdentity
}

// Refusal codes. Each refusal is a non-200 status with {"error": code, "message": why}.
const (
	errCodeUnauthenticated          = "unauthenticated"
	errCodeForbidden                = "forbidden"
	errCodeAuthUnavailable          = "auth_unavailable"
	errCodeNoCapability             = "no_capability"
	errCodeInvalidCapability        = "invalid_capability"
	errCodeCapabilityKeyUnavailable = "capability_key_unavailable"
)

type refusal struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// caller is an authenticated request's principal.
type caller struct {
	CallerIdentity
	audience string
	// platform is true for a platform caller presenting the sidecar audience.
	platform bool
}

// authenticate reviews the request's Bearer token for one of audiences, writing the refusal and returning
// false when it fails. With two audiences the token's own unverified aud claim picks which to review it
// for; the review is what decides.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, audiences ...string) (caller, bool) {
	if len(audiences) == 0 {
		s.log.Error(errors.New("no audience to review the token for"), "credplane: endpoint misconfigured", "path", r.URL.Path)
		s.refuse(w, r, http.StatusServiceUnavailable, errCodeAuthUnavailable, "the endpoint admits no audience")
		return caller{}, false
	}
	token, ok := bearerToken(r)
	if !ok {
		s.refuse(w, r, http.StatusUnauthorized, errCodeUnauthenticated, fmt.Sprintf(
			"no token-service credential: send this pod's projected ServiceAccount token (audience %s) "+
				"as a Bearer token; an agent built on a base image older than this release sends none and "+
				"must be rebuilt (ADR 0154)", strings.Join(audiences, " or ")))
		return caller{}, false
	}
	audience := audiences[0]
	if len(audiences) > 1 {
		claimed := unverifiedAudiences(token)
		for _, a := range audiences {
			if slices.Contains(claimed, a) {
				audience = a
				break
			}
		}
	}
	verifier := s.auth.verifierFor(audience)
	if verifier == nil {
		s.refuse(w, r, http.StatusServiceUnavailable, errCodeAuthUnavailable, "the token service cannot review tokens for "+audience)
		return caller{}, false
	}
	id, err := verifier.Verify(r.Context(), token)
	switch {
	case errors.Is(err, ErrCallerRejected):
		s.refuse(w, r, http.StatusUnauthorized, errCodeUnauthenticated, "the token was rejected for audience "+audience)
		return caller{}, false
	case err != nil:
		s.log.Error(err, "credplane: token review failed", "path", r.URL.Path)
		s.refuse(w, r, http.StatusServiceUnavailable, errCodeAuthUnavailable, "the token could not be reviewed; retry")
		return caller{}, false
	}
	return caller{
		CallerIdentity: id,
		audience:       audience,
		platform:       audience == runcap.TokenServicePodAudience && slices.Contains(s.auth.PlatformCallers, id),
	}, true
}

// capability verifies the run capability the request carries, writing the refusal and returning false when
// there is none or it does not verify.
func (s *Server) capability(w http.ResponseWriter, r *http.Request) (runcap.Capability, bool) {
	if s.auth.Capabilities == nil {
		s.refuse(w, r, http.StatusServiceUnavailable, errCodeCapabilityKeyUnavailable,
			"the token service has no capability public key (MCP_CAPABILITY_PUBLIC_KEY, the bff-capability Secret); "+
				"restart it once the key exists")
		return runcap.Capability{}, false
	}
	token := r.Header.Get(runcap.HeaderName)
	if token == "" {
		s.refuse(w, r, http.StatusUnauthorized, errCodeNoCapability, "this request needs the run capability ("+runcap.HeaderName+")")
		return runcap.Capability{}, false
	}
	c, err := s.auth.Capabilities.Verify(token)
	if err != nil {
		s.refuse(w, r, http.StatusUnauthorized, errCodeInvalidCapability, err.Error())
		return runcap.Capability{}, false
	}
	return c, true
}

func (s *Server) forbid(w http.ResponseWriter, r *http.Request, c caller, why string) {
	s.log.Info("credplane: request refused", "path", r.URL.Path, "caller", c.String(), "reason", why)
	writeRefusal(w, http.StatusForbidden, refusal{Error: errCodeForbidden, Message: why})
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.log.Info("credplane: request refused", "path", r.URL.Path, "status", status, "code", code)
	writeRefusal(w, status, refusal{Error: code, Message: message})
}

func writeRefusal(w http.ResponseWriter, status int, v refusal) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (a CallerAuth) verifierFor(audience string) TokenVerifier {
	switch audience {
	case runcap.TokenServicePodAudience:
		return a.Sidecar
	case runcap.TokenServiceAgentAudience:
		return a.Agent
	default:
		return nil
	}
}

func bearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	return token, ok && token != ""
}

// unverifiedAudiences reads a JWT's aud claim without verifying it. It only chooses which audience to
// review the token for.
func unverifiedAudiences(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	var many []string
	if json.Unmarshal(claims.Aud, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil {
		return []string{one}
	}
	return nil
}

// userScopeError is why caller c may not act for subject with capability cp, or "". The capability's
// agent must be in the caller's namespace, as for a resolve, and the subject must be its user.
func userScopeError(c caller, cp runcap.Capability, subject string) string {
	switch {
	case actorNamespace(cp) != c.Namespace:
		return "the run capability's agent is in another namespace"
	case subject != cp.User:
		return "the request names another user than the run capability"
	}
	return ""
}

// actorNamespace is the namespace of a capability's acting agent ("<namespace>/<agent>").
func actorNamespace(c runcap.Capability) string {
	ns, _, ok := strings.Cut(c.Agent, "/")
	if !ok {
		return ""
	}
	return ns
}

// bearerTransport adds a projected ServiceAccount token to every request it carries.
type bearerTransport struct {
	base      http.RoundTripper
	tokenFile string
}

// NewBearerTransport returns a RoundTripper that sends the token in tokenFile as a Bearer token on every
// request. The file is read per request because the kubelet rotates a projected token in place; a request
// is failed, not sent unauthenticated, when the file is missing or empty. base nil uses
// http.DefaultTransport.
func NewBearerTransport(base http.RoundTripper, tokenFile string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{base: base, tokenFile: tokenFile}
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, err := os.ReadFile(t.tokenFile)
	if err != nil {
		closeBody(req)
		return nil, fmt.Errorf("credplane: read the token-service credential: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		closeBody(req)
		return nil, fmt.Errorf("credplane: the token-service credential %s is empty", t.tokenFile)
	}
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(out)
}

// closeBody honours the RoundTripper contract on a request that is never sent.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
