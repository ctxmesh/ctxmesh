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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
)

// BFF audit actions (source="bff") — the security events the audit surface answers "who
// connected/consented/revoked what" with (ADR 0056 §2). CRD-mutation actions come from the
// controller sink; these are the caller-authenticated ones only the BFF can attribute precisely.
const (
	auditActionConnect     = "connect"      // a provider connection was created/rotated
	auditActionGrantCreate = "grant.create" // a per-user MCP OAuth grant was stored (consent)
	auditActionGrantRevoke = "grant.revoke" // a per-user MCP grant was revoked
	auditActionInvoke      = "invoke"       // an end-user invoked an agent (M91 EU2 — who invoked which agent/run)

	// auditKindAgent is the audit ResourceKind for an agent invocation (EU2). Matches the AgentDeployment
	// the caller ran, so the audit read-API can filter "all invocations of agent X".
	auditKindAgent = "AgentDeployment"

	// actorUnknown is the fallback actor when the authenticated username can't be resolved (never an
	// error — audit is observability, never a gate). actorKindUser marks a caller-authenticated row.
	actorUnknown  = "unknown"
	actorKindUser = "user"

	// auditOutcomeDenied is the outcome value for an access that was refused (grant revoked, provider
	// denied, guardrail blocked, etc.). Shared across all BFF audit rows that record a denial.
	auditOutcomeDenied = "denied"
	// auditPolicyActionBlock is the policy_action value for a guardrail block decision (m66.9).
	// Defined here (alongside the other audit constants) so the ingest handler and its callers share
	// the same string without triggering goconst.
	auditPolicyActionBlock = "block"
)

// auditActor resolves the PRECISE authenticated username for an audit row (ADR 0056 §1: store the real
// principal, not a hash — an audit trail without attribution is not an audit trail). "unknown" when it
// can't be resolved, never an error: audit is observability, never a gate on the audited action.
func (s *Server) auditActor(ctx context.Context, caller client.Client) string {
	if caller == nil {
		return actorUnknown
	}
	username, err := callerUsername(ctx, caller)
	if err != nil || username == "" {
		return actorUnknown
	}
	return username
}

// appendAudit records ONE BFF security event to the audit_log, BEST-EFFORT: a nil store (audit not
// wired) is a no-op, and a store error is logged but NEVER fails the caller's action (ADR 0056 §3 — the
// same "observability, never a gate" contract the controller sink honours). Call it AFTER the audited
// effect has happened (record-after-effect), or with outcome="denied" right before returning a 403.
func (s *Server) appendAudit(ctx context.Context, e auditlog.Entry) {
	if s.auditStore == nil {
		return
	}
	e.Source = "bff"
	if e.ActorKind == "" {
		e.ActorKind = actorKindUser
	}
	if err := s.auditStore.Append(ctx, e); err != nil {
		s.log.Error(err, "audit event not persisted (the action succeeded regardless)",
			"action", e.Action, "resource", e.ResourceName)
	}
}

// auditInvoke records an `invoke` audit event — "end-user <actor> invoked agent <ns>/<agent>" (M91 EU2,
// ADR 0056). actor is the ALREADY-RESOLVED caller username (callers pass the identity they already have —
// the durable-run path reuses the persisted Run.CallerUsername, avoiding a redundant SelfSubjectReview);
// an empty actor is normalized to "unknown". runID is the durable run's id (empty for the synchronous
// /api/invoke path, which has no durable run) and, when present, is stored as the row's TraceID so the
// audit read-API links the invocation to its run/trace. Best-effort + record-after-effect: a no-op when
// audit is not wired, and it NEVER gates the invoke.
func (s *Server) auditInvoke(ctx context.Context, actor, agent, namespace, runID string) {
	if s.auditStore == nil {
		return
	}
	if actor == "" {
		actor = actorUnknown
	}
	s.appendAudit(ctx, auditlog.Entry{
		Actor:        actor,
		Action:       auditActionInvoke,
		ResourceKind: auditKindAgent,
		ResourceName: agent,
		Namespace:    namespace,
		Outcome:      "success",
		TraceID:      runID,
	})
}
