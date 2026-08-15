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
)

// dedupRequest / dedupResponse are the async-dedup body (M53, ADR 0050 §6). The
// caller sends only the messageID + TTL — the proxy scopes the key by the pod's
// namespace (from the SA token), so the caller cannot name another namespace's
// seen-space.
type dedupRequest struct {
	MessageID  string `json:"messageID"`
	TTLSeconds int    `json:"ttlSeconds"`
}

type dedupResponse struct {
	FirstSeen bool `json:"firstSeen"`
}

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
	// (an intentional shared per-namespace dedup so sibling agents dedup a shared A2A
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

	ttl := min(max(time.Duration(req.TTLSeconds)*time.Second, dedupMinTTL), dedupMaxTTL)

	firstSeen, err := s.dedup.MarkSeen(ctx, dedupKey(ns, req.MessageID), ttl)
	if err != nil {
		// A backend error → 502; the launcher's dedup client fails CLOSED (NACK) on
		// any non-200, per the M11 posture (a dedup blip must not double-process).
		writeJSONError(w, http.StatusBadGateway, "dedup backend: "+err.Error())
		return
	}
	writeJSON(w, dedupResponse{FirstSeen: firstSeen})
}
