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

package statelayer

import (
	"context"
	"net/http"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/deliveryclaim"
)

// dedupRequest / dedupResponse are the async-dedup body (M53, ADR 0050 §6). The
// caller sends only the messageID + TTL — the proxy scopes the key by the pod's
// namespace (from the SA token), so the caller cannot name another namespace's
// seen-space.
//
// Op selects the claim lifecycle (claim, complete, release; deliveryclaim), and
// Token names the claim. With no op the request is the single-step MarkSeen that
// launchers built before claims send, so they keep working against this proxy.
type dedupRequest struct {
	MessageID  string `json:"messageID"`
	TTLSeconds int    `json:"ttlSeconds"`
	Op         string `json:"op,omitempty"`
	Token      string `json:"token,omitempty"`
}

// dedupResponse answers a claim with its State; FirstSeen is set too, so a claim
// answered as a MarkSeen by an older proxy still reads correctly.
type dedupResponse struct {
	FirstSeen bool                `json:"firstSeen"`
	State     deliveryclaim.State `json:"state,omitempty"`
}

const (
	dedupOpClaim    = "claim"
	dedupOpComplete = "complete"
	dedupOpRelease  = "release"
)

const (
	maxDedupMessageID = 256
	// dedupMaxTTL bounds a caller-supplied TTL so a bad value can't pin a key
	// forever; dedupMinTTL keeps a positive floor.
	dedupMinTTL = time.Second
	dedupMaxTTL = 24 * time.Hour
)

func (s *Server) handleDedup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()

	if s.dedup == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "dedup is not configured on this proxy")
		return
	}
	// Bind the VERIFIED per-agent identity (m79.2): the seen-set stays NAMESPACE-scoped
	// (an intentional shared per-namespace dedup so sibling agents dedup a shared AMP
	// message together — ADR 0050 §6), so the ns is still what keys the space. The
	// agent binding only gates WHO may act — a verified but non-agent SA (e.g. the
	// namespace default) is now 403'd instead of being let in to poison the seen-set,
	// mirroring the memory path. Requiring an agent identity to resolve IS the gate.
	ns, err := s.authenticateAgentNamespace(ctx, bearerToken(r))
	if err != nil {
		writeAgentAuthError(w, err)
		return
	}

	var req dedupRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if err := validateConversationID(req.MessageID); err != nil || len(req.MessageID) > maxDedupMessageID {
		writeJSONError(w, http.StatusBadRequest, "messageID is required and must be a bounded id without control chars")
		return
	}

	if req.Op == dedupOpClaim || req.Op == dedupOpRelease {
		if err := validateConversationID(req.Token); err != nil || len(req.Token) > maxDedupMessageID {
			writeJSONError(w, http.StatusBadRequest, "a claim needs a token: a bounded id without control chars")
			return
		}
	}

	ttl := min(max(time.Duration(req.TTLSeconds)*time.Second, dedupMinTTL), dedupMaxTTL)
	key := dedupKey(ns, req.MessageID)

	// A backend error → 502; the launcher's dedup client fails CLOSED (NACK) on
	// any non-200, per the M11 posture (a dedup blip must not double-process).
	var resp dedupResponse
	switch req.Op {
	case "":
		resp.FirstSeen, err = s.dedup.MarkSeen(ctx, key, ttl)
	case dedupOpClaim:
		resp.State, err = s.dedup.Claim(ctx, key, req.Token, ttl)
		resp.FirstSeen = resp.State == deliveryclaim.Claimed
	case dedupOpComplete:
		err = s.dedup.Complete(ctx, key, ttl)
	case dedupOpRelease:
		err = s.dedup.Release(ctx, key, req.Token)
	default:
		writeJSONError(w, http.StatusBadRequest, "op must be claim, complete or release")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "dedup backend: "+err.Error())
		return
	}
	writeJSON(w, resp)
}
