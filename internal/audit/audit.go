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

// Package audit records mutating actions (create/update/delete) on the
// agent.ctxmesh.ai control-plane CRDs into a structured audit trail (M11.4,
// PRD §20). It is a controller-emitted audit: an informer on each agent CRD
// with Add/Update/Delete handlers emits a structured AuditEntry. Capturing
// Delete is why an informer (with a Delete handler) is used rather than the
// reconciler alone — a reconciler without a finalizer never observes the
// delete of an object.
//
// The "who" (Subject) is best-effort in v1, derived from the object's
// managedFields field-manager (server-side apply / last-applied). The precise
// authenticated caller (AdmissionReview.userInfo) requires a validating
// admission webhook and its TLS cert infrastructure; that is deferred to
// phase-2 (see the trace-governance-security spec §2).
package audit

import (
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Verb is the mutating action recorded in an AuditEntry.
type Verb string

const (
	// VerbCreate records the creation of a CRD object.
	VerbCreate Verb = "create"
	// VerbUpdate records an update (spec/metadata change) to a CRD object.
	VerbUpdate Verb = "update"
	// VerbDelete records the deletion of a CRD object — the most
	// security-relevant mutating action, captured via the informer DeleteFunc.
	VerbDelete Verb = "delete"
)

// unknownSubject is the placeholder used when no field-manager can be derived
// from the object's managedFields (best-effort "who" in v1).
const unknownSubject = "unknown"

// AuditEntry is one structured record of a mutating action on an agent CRD.
// It answers who/what/when: Subject (who, best-effort), Verb+Kind+Name+
// Namespace (what), Timestamp (when).
type AuditEntry struct {
	// Timestamp is when the audit entry was emitted (RFC3339 in the log sink).
	Timestamp time.Time
	// Verb is the mutating action: create, update, or delete.
	Verb Verb
	// Kind is the CRD kind, e.g. "AgentDeployment".
	Kind string
	// Name is the object's name.
	Name string
	// Namespace is the object's namespace (empty for cluster-scoped objects).
	Namespace string
	// Subject is the best-effort actor derived from managedFields (the
	// field-manager of the most recent apply). "unknown" when undeterminable.
	// The precise authenticated caller requires an admission webhook (phase-2).
	Subject string
}

// Sink consumes audit entries. The default sink writes to a structured log;
// tests inject a capturing sink to assert the emitted entries.
type Sink interface {
	// Record persists one audit entry. Implementations must not block the
	// caller (the informer event loop) for long and must not return errors —
	// audit is observability, never an admission gate.
	Record(entry AuditEntry)
}

// LogSink writes audit entries to a structured logr logger. It is the default
// v1 sink: an operator log line per mutating action, greppable and shippable
// to any log backend.
type LogSink struct {
	log logr.Logger
}

// NewLogSink returns a Sink that writes structured audit entries to log.
func NewLogSink(log logr.Logger) *LogSink {
	return &LogSink{log: log.WithName("audit")}
}

// Record writes the entry as a structured log line with stable keys.
func (s *LogSink) Record(entry AuditEntry) {
	s.log.Info("control-plane mutation",
		"timestamp", entry.Timestamp.UTC().Format(time.RFC3339),
		"verb", string(entry.Verb),
		"kind", entry.Kind,
		"name", entry.Name,
		"namespace", entry.Namespace,
		"subject", entry.Subject,
	)
}

// SinkFunc adapts a plain function to the Sink interface (used by tests).
type SinkFunc func(entry AuditEntry)

// Record calls the underlying function.
func (f SinkFunc) Record(entry AuditEntry) { f(entry) }

// subjectFromObject derives a best-effort actor for the audit entry from the
// object's managedFields — the field-manager of the most recently updated
// managed-fields entry. This is the closest "who" available without an
// admission webhook (which alone carries the true authenticated userInfo).
// Returns unknownSubject when no field-manager is recorded.
func subjectFromObject(obj metav1.Object) string {
	if obj == nil {
		return unknownSubject
	}
	mf := obj.GetManagedFields()
	if len(mf) == 0 {
		return unknownSubject
	}
	// Pick the entry with the most recent Time; fall back to the last entry.
	best := mf[0]
	for i := 1; i < len(mf); i++ {
		if mf[i].Time != nil && (best.Time == nil || mf[i].Time.After(best.Time.Time)) {
			best = mf[i]
		}
	}
	if best.Manager == "" {
		return unknownSubject
	}
	return best.Manager
}
