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

package bff

import (
	"net/http"
	"strings"
)

// Authenticator gates every /api request. It is the seam for the M11
// control-plane authn (PRD §20): the UI is behind an authenticated caller, and
// the RBAC personas (developer/operator/viewer ClusterRoles) gate writes when
// the request is carried through to the Kubernetes API.
//
// v1 default (BearerAuthenticator) enforces PRESENCE of a bearer credential —
// an unauthenticated caller is rejected (spec "Edge cases: Auth"). The precise
// per-persona RBAC decision is made by the Kubernetes API server when the BFF
// performs the CRD operation on the caller's behalf (K8s RBAC is the
// authorization source of truth; the BFF does not re-implement it). Wiring the
// caller's token into a per-request client-go client is the m12.5+ step; this
// interface is the stable seam for it.
type Authenticator interface {
	// Authenticate returns nil if the request carries an acceptable credential,
	// or an error describing why it was rejected (surfaced as 401).
	Authenticate(r *http.Request) error
}

// BearerAuthenticator accepts any request presenting a non-empty
// "Authorization: Bearer <token>" header. It intentionally does NOT validate the
// token itself — validation + authorization are delegated to the Kubernetes API
// (M11 RBAC) at the point the token is used. This keeps the browser out of the
// authorization decision while still rejecting anonymous callers at the edge.
type BearerAuthenticator struct{}

// Authenticate implements Authenticator.
func (BearerAuthenticator) Authenticate(r *http.Request) error {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) || strings.TrimSpace(h[len(prefix):]) == "" {
		return errUnauthenticated
	}
	return nil
}

// AllowAll is a permissive Authenticator for local dev / tests only. It must
// never be wired in a deployed BFF; production uses BearerAuthenticator (or a
// TokenReview-backed authenticator once that lands).
type AllowAll struct{}

// Authenticate implements Authenticator (always allows).
func (AllowAll) Authenticate(*http.Request) error { return nil }

// authError is a typed sentinel so the middleware can distinguish auth
// rejection from other failures.
type authError string

func (e authError) Error() string { return string(e) }

const errUnauthenticated authError = "unauthenticated: missing or empty bearer token"

// requireAuth wraps a handler so every request is authenticated first. A
// rejection returns 401 with a JSON error body; the wrapped handler only runs
// for authenticated callers.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.auth.Authenticate(r); err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}
