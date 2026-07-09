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

package main

// Async A2A over Knative Eventing (M7, specs/eventing-scaling.md §"Async A2A
// envelope = the M6 envelope as a CloudEvent"). The SAME platform envelope
// (a2a.go, §12.5) that carries a synchronous A2A call is carried on the async
// path as a CloudEvent whose attributes MIRROR the envelope's routing fields:
//
//	CloudEvent id     = envelope.MessageID      (the idempotency key, §12.6)
//	CloudEvent type   = envelope.ReceiverAgentID (the target agent — MUST match
//	                                              the m7.5 Trigger filter
//	                                              `type == <agentName>` or the
//	                                              broker never routes to it)
//	CloudEvent source = envelope.SenderAgentID   (the emitting agent)
//	CloudEvent data   = the envelope JSON        (payload nested inside, opaque)
//
// The launcher owns this mapping end to end: a producer's launcher encodes the
// envelope into a CloudEvent and publishes it to the registry broker; the
// consumer's launcher decodes it back into an envelope before invoking the agent.
// The agent code never sees the CloudEvent transport — exactly as the sync path
// hides the HTTP envelope header.

import (
	"encoding/json"
	"fmt"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/event"
)

// ceExtensionRegistryID is the CloudEvent extension attribute carrying the
// registry id. It is redundant with the registryId inside the envelope data, but
// exposing it as a first-class context attribute lets the broker / a Trigger
// filter on it without parsing the data body (registry isolation is data-plane
// enforced app-layer per §12.2, but the attribute keeps the transport
// self-describing). Extension names must be lowercase alphanumerics.
const ceExtensionRegistryID = "registryid"

// envelopeToCloudEvent maps a platform envelope onto a CloudEvent per the table
// in the file header. It returns an error only if the envelope cannot be
// serialized as the event data (a marshal failure of well-formed JSON is
// effectively impossible, but it is surfaced rather than swallowed).
//
// The event is v1.0 (SpecVersion), data content-type application/json. id/type/
// source are the routing mirror; the whole envelope (including its own copy of
// messageId/receiver/sender) travels as data so the consumer reconstructs the
// exact envelope the agent would have seen on the sync path.
func envelopeToCloudEvent(env envelope) (cloudevents.Event, error) {
	evt := cloudevents.NewEvent()
	evt.SetID(env.MessageID)
	evt.SetType(env.ReceiverAgentID)
	evt.SetSource(env.SenderAgentID)
	evt.SetExtension(ceExtensionRegistryID, env.RegistryID)

	// The envelope JSON is the event data. Marshal it ourselves and set it as a
	// raw JSON blob (SetData with an already-marshalled []byte + the JSON content
	// type) so the nested payload is preserved byte-for-byte rather than being
	// re-encoded by reflection.
	data, err := json.Marshal(env)
	if err != nil {
		return cloudevents.Event{}, fmt.Errorf("marshal envelope for CloudEvent data: %w", err)
	}
	if err := evt.SetData(cloudevents.ApplicationJSON, json.RawMessage(data)); err != nil {
		return cloudevents.Event{}, fmt.Errorf("set CloudEvent data: %w", err)
	}
	return evt, nil
}

// cloudEventToEnvelope reverses envelopeToCloudEvent: it decodes the event data
// back into a platform envelope. The envelope's own fields (messageId, receiver,
// sender, registryId) are authoritative — they are what the consumer dedupes and
// routes on — so the CloudEvent attributes are NOT trusted over the data (a
// mismatched id/type/source is a producer bug, but the data is the contract the
// agent path already understands).
//
// A missing or non-JSON data body is an error: an async A2A CloudEvent MUST
// carry an envelope, and a consumer that cannot decode one has nothing to invoke
// the agent with (the caller DLQs it rather than invoking blindly).
func cloudEventToEnvelope(evt event.Event) (envelope, error) {
	data := evt.Data()
	if len(data) == 0 {
		return envelope{}, fmt.Errorf("CloudEvent %q carries no data (no envelope)", evt.ID())
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return envelope{}, fmt.Errorf("decode envelope from CloudEvent data: %w", err)
	}
	// The messageId is the idempotency key; an envelope with none cannot be
	// deduped and is a malformed async message. Fall back to the CloudEvent id
	// (they are the same by construction) so a producer that only set the CE id
	// still yields a dedupe key, but reject when both are empty.
	if env.MessageID == "" {
		env.MessageID = evt.ID()
	}
	if env.MessageID == "" {
		return envelope{}, fmt.Errorf("async envelope has no messageId (cannot dedupe)")
	}
	return env, nil
}
