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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
	"github.com/ctxmesh/ctxmesh/internal/statelayer"
)

// The AMP attestation edge (ADR 0153): where a registry member's launcher learns how to prove who it is
// to the agents it calls.
//
// The launcher presents its pod's projected token for the BFF audience and names the agent it is about to
// call. The namespace and agent come from that token's ServiceAccount (agent-<name>, verified by
// TokenReview); the registry and role come from the controller's record of the agent in the capability
// registry. Nothing in the request names any of them: the receiver only narrows where the attestation is
// accepted, so a receiver that captures it cannot replay it to another agent. The answer is a short-lived
// attestation from the run-capability signer, which the callee verifies offline.

const (
	ampAttestationPath = "/api/internal/amp/attestation"
	// ampAttestationTTL bounds how long a leaked attestation names its agent, and how long a callee
	// keeps honouring one after the agent left its registry or changed role. The launcher refreshes
	// before it lapses, so a call never waits on this edge more than once per lifetime.
	ampAttestationTTL = 5 * time.Minute
	// maxAMPAttestationRequest bounds the request body, which carries one agent name.
	maxAMPAttestationRequest = 1 << 10

	// refusedPodTokenTTL is how long a token the review rejected is refused without another review. A
	// rejected pod token stays rejected, so this bounds only memory, not correctness.
	refusedPodTokenTTL = time.Minute
	// maxRefusedPodTokens bounds the refused tokens remembered. Past it the record is cleared: forgetting
	// a refusal costs one more review, never an admission.
	maxRefusedPodTokens = 4096
)

// refusedPodTokens remembers, by hash, pod tokens the review rejected, so a launcher retrying with a
// stale token, or a pod repeating one it made up, costs the API server one TokenReview a minute rather
// than one per request. The zero value is ready to use.
type refusedPodTokens struct {
	mu    sync.Mutex
	until map[[sha256.Size]byte]time.Time
}

func (c *refusedPodTokens) refused(tok string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[sha256.Sum256([]byte(tok))]
	return ok && now.Before(until)
}

func (c *refusedPodTokens) refuse(tok string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil || len(c.until) >= maxRefusedPodTokens {
		for k, until := range c.until {
			if !now.Before(until) {
				delete(c.until, k)
			}
		}
		if c.until == nil || len(c.until) >= maxRefusedPodTokens {
			c.until = map[[sha256.Size]byte]time.Time{}
		}
	}
	c.until[sha256.Sum256([]byte(tok))] = now.Add(refusedPodTokenTTL)
}

// ampAttestationRequest names the agent the attestation is for: an agent in the caller's namespace.
type ampAttestationRequest struct {
	Receiver string `json:"receiver"`
}

// ampAttestationResponse carries the attestation and its lifetime in seconds. Relative, so a launcher
// whose clock disagrees with the BFF's still refreshes in time.
type ampAttestationResponse struct {
	Attestation string `json:"attestation"`
	ExpiresIn   int64  `json:"expiresIn"`
	// Endpoint is where the receiver serves, when it is in the caller's registry and the platform records
	// an address for it. A launcher cannot tell from the name alone that an eventing agent serves at its
	// <agent>-eventing Service; without this it calls the Knative route.
	Endpoint string `json:"endpoint,omitempty"`
}

// registerAMPAttestationRoute wires the edge onto the unauthenticated api mux: it authenticates the pod
// itself. Absent without a signer, a pod authenticator or the capability registry, since it could not
// answer correctly without all three.
func (s *Server) registerAMPAttestationRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil && s.podAuth != nil && s.agentCapabilities != nil {
		api.HandleFunc("POST "+ampAttestationPath, s.handleAMPAttestation)
	}
}

// handleAMPAttestation serves POST /api/internal/amp/attestation. 400 for a receiver that is not an agent
// name, 401 for a missing or rejected pod token, 403 for a pod that is not an agent or an agent in no
// registry, 503 when the token review or the capability registry cannot answer.
func (s *Server) handleAMPAttestation(w http.ResponseWriter, r *http.Request) {
	tok, found := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
	tok = strings.TrimSpace(tok)
	if !found || tok == "" {
		writeError(w, http.StatusUnauthorized,
			"an AMP attestation is issued for the calling pod's projected token (audience "+runcap.BFFPodAudience+")")
		return
	}
	var req ampAttestationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAMPAttestationRequest)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the request names the receiving agent: {\"receiver\":\"<agent>\"}")
		return
	}
	if errs := validation.IsDNS1123Label(req.Receiver); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, "the receiver must be an agent name: "+strings.Join(errs, "; "))
		return
	}
	if s.refusedPodTokens.refused(tok, time.Now()) {
		writeError(w, http.StatusUnauthorized, "the pod token did not authenticate")
		return
	}
	id, err := s.podAuth.Identity(r.Context(), tok)
	switch {
	case errors.Is(err, statelayer.ErrTokenRejected):
		s.refusedPodTokens.refuse(tok, time.Now())
		writeError(w, http.StatusUnauthorized, "the pod token did not authenticate")
		return
	case err != nil:
		s.log.Error(err, "amp attestation: the pod token could not be reviewed")
		writeError(w, http.StatusServiceUnavailable, "the pod token could not be reviewed; retry")
		return
	}
	agent, isAgent := strings.CutPrefix(id.ServiceAccount, agentIdentitySAPrefix)
	if !isAgent || agent == "" {
		writeError(w, http.StatusForbidden, "only an agent's pod is attested")
		return
	}
	self, ok, err := s.agentCapabilities.Get(r.Context(), id.Namespace, agent)
	if err != nil {
		s.log.Error(err, "amp attestation: reading the capability registry failed", "agent", id.Namespace+"/"+agent)
		writeError(w, http.StatusServiceUnavailable, "the capability registry is unavailable; retry")
		return
	}
	if !ok || self.RegistryID == "" {
		// Only a member calls over AMP, and an attestation naming no registry is one no callee could check.
		writeError(w, http.StatusForbidden, "this agent belongs to no registry, so it makes no AMP calls")
		return
	}
	att, err := s.capabilitySigner.MintAttestation(runcap.AttestationRequest{
		Namespace:  id.Namespace,
		Agent:      agent,
		RegistryID: self.RegistryID,
		Role:       self.Role,
		Receiver:   req.Receiver,
		TTL:        ampAttestationTTL,
	})
	if err != nil {
		s.log.Error(err, "amp attestation: minting failed", "agent", id.Namespace+"/"+agent)
		writeError(w, http.StatusInternalServerError, "the attestation could not be minted")
		return
	}
	resp := ampAttestationResponse{Attestation: att, ExpiresIn: int64(ampAttestationTTL / time.Second)}
	peer, ok, err := s.agentCapabilities.Get(r.Context(), id.Namespace, req.Receiver)
	if err != nil {
		s.log.Error(err, "amp attestation: reading the receiver's registration failed",
			"receiver", id.Namespace+"/"+req.Receiver)
		writeError(w, http.StatusServiceUnavailable, "the capability registry is unavailable; retry")
		return
	}
	if ok && peer.RegistryID == self.RegistryID {
		resp.Endpoint = peer.Endpoint
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
