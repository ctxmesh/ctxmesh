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
	"errors"
	"net/http"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// Sender-constrained run capabilities at the internal edges (M142.5, ADR 0124).
//
// Every capability-authenticated edge — spawn, await, handoff, discover, async publish, guardrail events
// — used to treat POSSESSION as authority. That is what a bearer token means, and it is why a capability
// lifted from a log or a trace could be spent by whoever found it. These edges now additionally demand a
// per-request proof signed by the key the capability is bound to.
//
// The rollout posture is explicit rather than implied. A capability with no `cnf` is a legacy bearer
// token, and during a rollout the launcher minting it may simply be older than the BFF verifying it.
// RUNCAP_REQUIRE_POP decides what happens then:
//
//   - unset/false (default): accept, and log once per edge that a bearer capability was honoured. The
//     mechanism is live for everything that CAN present a proof, without breaking a mixed-version fleet.
//   - true: refuse. This is the end state, flipped deliberately once the fleet is known to be minting
//     bound capabilities — the M128/M134 pattern for a fail-closed default.
//
// A capability that IS bound always requires a valid proof, in both postures: there is no configuration
// under which a sender-constrained token degrades back to bearer, because that would hand an attacker the
// downgrade as a feature.

// verifyRuncapWithProof verifies the relayed capability and, when it is sender-constrained, the
// proof-of-possession for THIS request. It returns the verified capability, or an error whose message is
// safe to return to the caller.
func (s *Server) verifyRuncapWithProof(r *http.Request) (runcap.Capability, error) {
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		return runcap.Capability{}, errors.New("missing run capability")
	}
	capab, err := s.capabilitySigner.Verifier().Verify(token)
	if err != nil {
		return runcap.Capability{}, errors.New("invalid run capability")
	}

	proof := strings.TrimSpace(r.Header.Get(runcap.PoPHeaderName))
	perr := s.proofVerifier().VerifyProof(capab, proof, r.Method, requestURL(r))
	switch {
	case perr == nil:
		return capab, nil

	case errors.Is(perr, runcap.ErrNotSenderConstrained):
		// The capability itself is a legacy bearer token. Nothing about the request can fix that, so the
		// question is purely the fleet's posture.
		if s.requireProofOfPossession {
			return runcap.Capability{}, errors.New(
				"this capability is not sender-constrained; the platform requires proof-of-possession")
		}
		s.log.V(1).Info("accepting a legacy BEARER run capability (no cnf) — set RUNCAP_REQUIRE_POP=true "+
			"once the fleet mints sender-constrained capabilities",
			"run", capab.RunID, "agent", capab.Agent, "path", r.URL.Path)
		return capab, nil

	default:
		// The capability IS bound and the proof did not hold. Never accepted, in any posture — accepting
		// here would let an attacker downgrade a constrained token by simply omitting the proof.
		s.log.Info("run-capability proof REJECTED", "run", capab.RunID, "agent", capab.Agent,
			"path", r.URL.Path, "reason", perr.Error())
		return runcap.Capability{}, errors.New("invalid run-capability proof")
	}
}

// requestURL reconstructs the absolute URL the caller addressed, which is what its proof is bound to. A
// server-side request carries only the path in r.URL, so scheme and host come from the request context —
// and the caller's Host header is what it actually dialled, which is the value it signed over.
func requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

// proofVerifier returns the shared verifier, whose replay set is what makes a proof single-use. One per
// Server rather than one per request: a fresh verifier would remember nothing and every proof would be
// replayable, which is the kind of mistake that looks like working code.
func (s *Server) proofVerifier() *runcap.ProofVerifier {
	s.proofOnce.Do(func() { s.popVerifier = runcap.NewProofVerifier(nil) })
	return s.popVerifier
}
