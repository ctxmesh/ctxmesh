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

package credprovider

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// maxRequestBytes bounds a provider request body.
const maxRequestBytes = 1 << 16

// NewHandler serves the credprovider contract for a Go Backend, so a third-party vault (or
// the conformance harness) exposes an SPI-conformant endpoint. Mount it behind mTLS. The
// resolve outcomes consent_required / no_credential are mapped to first-class `signal`
// fields (HTTP 200), NOT 5xx — so the cause is never masked as a generic internal error.
func NewHandler(b Backend) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET "+PathCapabilities, func(w http.ResponseWriter, r *http.Request) {
		caps, err := b.Capabilities(r.Context())
		if err != nil {
			http.Error(w, "capabilities failed", http.StatusInternalServerError)
			return
		}
		if caps.APIVersion == "" {
			caps.APIVersion = APIVersion
		}
		writeJSON(w, caps)
	})

	mux.HandleFunc("POST "+PathResolve, func(w http.ResponseWriter, r *http.Request) {
		var req resolveRequest
		if !decode(w, r, &req) {
			return
		}
		cred, err := b.Resolve(r.Context(), req.Namespace, req.Boundary, req.Server, req.UserHash, req.Tenant)
		switch {
		case errors.Is(err, credresolve.ErrConsentRequired):
			writeJSON(w, resolveResponse{Signal: SignalConsentRequired})
		case errors.Is(err, credresolve.ErrNoCredential):
			writeJSON(w, resolveResponse{Signal: SignalNoCredential})
		case err != nil:
			// A real failure: report the stable code, not the cause (non-leak).
			writeJSON(w, resolveResponse{Error: errCodeInternal})
		default:
			writeJSON(w, resolveResponse{Kind: cred.Kind, Value: cred.Value})
		}
	})

	mux.HandleFunc("POST "+PathStore, func(w http.ResponseWriter, r *http.Request) {
		var req storeRequest
		if !decode(w, r, &req) {
			return
		}
		if err := b.Store(r.Context(), req.Namespace, req.Boundary, req.Server, req.UserHash, req.Tenant, req.Grant); err != nil {
			writeJSON(w, ackResponse{Error: errCodeInternal})
			return
		}
		writeJSON(w, ackResponse{})
	})

	mux.HandleFunc("POST "+PathRevoke, func(w http.ResponseWriter, r *http.Request) {
		var req revokeRequest
		if !decode(w, r, &req) {
			return
		}
		if err := b.Revoke(r.Context(), req.Namespace, req.Boundary, req.Server, req.UserHash, req.Tenant); err != nil {
			writeJSON(w, ackResponse{Error: errCodeInternal})
			return
		}
		writeJSON(w, ackResponse{})
	})

	return mux
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(out); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(VersionHeader, APIVersion)
	_ = json.NewEncoder(w).Encode(v)
}
