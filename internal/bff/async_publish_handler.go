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
	"io"
	"net/http"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The async AMP PUBLISH edge (M141.4, ADR 0121) — where an agent hands a durable hop to the platform.
//
// An agent pod never holds a broker connection: credentials live in the control plane, the same rule that
// put provider keys behind the token-service and Valkey behind the state-layer proxy. So the launcher
// POSTs its CloudEvent here, exactly as it would to a Knative Broker, and the BFF is what talks to the bus.
//
// This is the same internal, capability-authenticated class as the spawn and discovery edges: the caller
// is a launcher relaying its run capability, not a browser bearer token.

const (
	// maxAsyncEventBytes bounds a published CloudEvent. The launcher already offloads an oversize payload
	// to the object store before publishing, so anything near this is a bug or an attack, not a big task.
	maxAsyncEventBytes = 1 << 20 // 1 MiB

	// headerAsyncNamespace / headerAsyncRegistry carry the PLATFORM's view of where a message came from.
	// They are stamped here from the verified run and OVERWRITE anything the caller sent: a pod that
	// could name its own registry could publish into a registry it was never a member of, and the
	// dispatcher trusts these to decide where a message may be delivered.
	headerAsyncNamespace = "X-Ctxmesh-Async-Namespace"
	headerAsyncRegistry  = "X-Ctxmesh-Async-Registry"

	// ceIDHeader / ceTypeHeader are the CloudEvent binary-mode attributes we read: the event id is the
	// envelope's messageId (the idempotency key), and the type is the RECEIVER agent — which is how the
	// existing Knative Trigger already filters, so routing needs no envelope parsing at all.
	ceIDHeader   = "Ce-Id"
	ceTypeHeader = "Ce-Type"
)

// registerAsyncPublishRoute wires the publish edge onto the UNAUTHENTICATED api mux (the handler verifies
// the relayed capability itself, like spawn). Wired only when a bus is configured AND capabilities can be
// verified; otherwise the route is absent, so an install without an async backend simply does not have it.
func (s *Server) registerAsyncPublishRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil && s.asyncPublisher != nil && s.runStore != nil && s.agentCapabilities != nil {
		api.HandleFunc("POST /api/internal/async/publish", s.handleAsyncPublish)
	}
}

// handleAsyncPublish serves POST /api/internal/async/publish — durably enqueue one async AMP hop.
//
// The request body is the encoded CloudEvent and the request headers are its binding headers, moved
// through unchanged: the platform does not parse the AMP envelope here, so the wire format stays owned by
// the AMP layer (ADR 0121).
//
// Authorization mirrors the discovery edge: verify the capability, load the run it scopes to, and take the
// caller's namespace + registry from the CONTROL PLANE — its own capability-registry row — rather than
// from anything the pod says. An agent that belongs to no registry has no async peers and is refused.
func (s *Server) handleAsyncPublish(w http.ResponseWriter, r *http.Request) {
	// Sender-constrained: the capability must verify AND, when it is bound to a key, carry a
	// proof-of-possession for this request (M142.5, ADR 0124) — so a copied token is not authority.
	capab, capErr := s.verifyRuncapWithProof(r)
	if capErr != nil {
		writeError(w, http.StatusUnauthorized, capErr.Error())
		return
	}
	caller, err := s.runStore.Get(capab.RunID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "the run capability does not resolve to a run")
		return
	}

	self, ok, err := s.agentCapabilities.Get(r.Context(), caller.Namespace, caller.Agent)
	if err != nil {
		s.log.Error(err, "async publish: reading the capability registry failed",
			"agent", caller.Namespace+"/"+caller.Agent)
		writeError(w, http.StatusBadGateway, "the capability registry is unavailable")
		return
	}
	if !ok || self.RegistryID == "" {
		// No registry ⇒ no async peers. Refusing beats publishing into a subject nobody serves, which
		// would look like success and lose the hop.
		writeError(w, http.StatusForbidden, "this agent belongs to no registry — async AMP is registry-scoped")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAsyncEventBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the CloudEvent is too large to publish")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "an async AMP publish carries a CloudEvent body")
		return
	}
	messageID := strings.TrimSpace(r.Header.Get(ceIDHeader))
	if messageID == "" {
		// The id is the idempotency key the whole at-least-once contract rests on: the broker dedupes on
		// it and the callee's launcher dedupes on it. Publishing without one is not something to paper over.
		writeError(w, http.StatusBadRequest, "the CloudEvent needs an id (ce-id) — it is the idempotency key")
		return
	}
	if strings.TrimSpace(r.Header.Get(ceTypeHeader)) == "" {
		writeError(w, http.StatusBadRequest, "the CloudEvent needs a type (ce-type) — it names the receiver")
		return
	}

	msg := asyncbus.Message{
		ID:      messageID,
		Subject: asyncbus.Subject(self.RegistryID),
		Data:    body,
		Headers: asyncPublishHeaders(r.Header, caller.Namespace, self.RegistryID),
	}
	if err := s.asyncPublisher.Publish(r.Context(), msg); err != nil {
		s.log.Error(err, "async publish failed", "agent", caller.Namespace+"/"+caller.Agent,
			"messageId", messageID)
		writeError(w, http.StatusBadGateway, "the async backend did not accept the message")
		return
	}
	// 202: durably accepted, not yet delivered — the seam's contract exactly.
	w.WriteHeader(http.StatusAccepted)
}

// asyncPublishHeaders copies the CloudEvent binding headers and stamps the platform's authoritative
// namespace + registry over the top. Copying rather than filtering keeps the wire format the AMP layer's
// business; overwriting the two platform headers is what stops a pod choosing its own routing context.
func asyncPublishHeaders(in http.Header, namespace, registryID string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k := range in {
		switch http.CanonicalHeaderKey(k) {
		case http.CanonicalHeaderKey(runcap.HeaderName):
			continue // never carry the run capability onto the bus
		case headerAsyncNamespace, headerAsyncRegistry:
			continue // caller-supplied routing context is discarded, then re-stamped below
		}
		out[k] = in.Get(k)
	}
	out[headerAsyncNamespace] = namespace
	out[headerAsyncRegistry] = registryID
	return out
}
