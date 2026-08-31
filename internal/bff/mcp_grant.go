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
	"sync/atomic"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// Per-user on-behalf-of (OBO) MCP grants (m17.3, ADR 0016 §5) — the FILL-IN of the
// (server, user) credential-resolution seam (mcp_credential.go).
//
// M14 stored a SHARED service key + ignored `user`. M17.3 turns the seam per-user:
// the first time a user invokes an agent that calls an OAuth MCP server, THAT user
// consents (the m17.2 Auth-Code + PKCE flow) → their OAuth grant is stored as a
// Secret keyed (user, server) → the agent's calls to that server run AS THAT USER
// (refresh + revocation + audit). User A's grant NEVER resolves for user B.
//
// The token discipline is the m17.2 lesson, unchanged: the OAuth access/refresh
// tokens live ONLY in the Secret's data — NEVER in a label, an annotation, a DTO,
// or a log line. Labels are LOOKUP KEYS ONLY: a hash of the username (not the raw
// username — which may be an email/PII) and the server name. This is why we hash:
// a label value must be a bounded, non-PII, DNS-1123-safe token, and the raw
// username is none of those.

// Grant-Secret label keys + Secret name prefix now live in internal/credresolve — the
// SINGLE SOURCE of the grant wire format (ADR 0030), shared by the BFF consent WRITER
// here and every credential READER (the sidecar / central token service). These aliases
// keep the existing bff references working while credresolve owns the authoritative
// values; a divergence is impossible because both sides resolve to the same constant.
const (
	labelMCPGrantUser     = credresolve.LabelGrantUser
	labelMCPGrantServer   = credresolve.LabelGrantServer
	labelMCPGrantSourceNS = credresolve.LabelGrantSourceNS
	managedByMCPGrant     = credresolve.ManagedByGrant
	mcpGrantSecretPrefix  = credresolve.SecretPrefix
)

// annMCPGrantServerURL persists the MCP server URL on the grant Secret (non-secret)
// so a consent flow's callback and a later refresh have the endpoint without a
// second read. It mirrors annMCPURL on the register objects.
const annMCPGrantServerURL = annMCPURL

// grantHMACKey is the per-cluster HMAC key that salts the user-identity hash so it
// cannot be confirmed offline (a rainbow-table of low-entropy usernames/emails) by
// anyone who can read the grant Secret labels or the mcp-owner annotation (m25.1,
// ADR 0029 §7 / advisor R5). Set once at BFF start-up from the MCP_GRANT_HMAC_KEY
// env (a per-cluster platform Secret). When empty, userGrantHash falls back to the
// legacy unsalted SHA-256 — a DOCUMENTED PRODUCTION PREREQUISITE (like etcd
// encryption-at-rest, ADR 0016): a prod cluster MUST set the key. Immutable after
// start-up; changing it re-keys all grants (⇒ re-consent).
// grantHMACKey is an atomic.Pointer (not a bare []byte) so setGrantHMACKey (called in NewServer) and
// userGrantHash (the hot read path) are DATA-RACE-FREE even when servers are constructed concurrently
// — e.g. parallel tests, `go test -race` (m52.K8b). In production it is still written once at start-up
// and immutable thereafter; the atomic just makes the concurrent-construction case honest.
var grantHMACKey atomic.Pointer[[]byte]

// setGrantHMACKey wires the per-cluster HMAC key at BFF construction (from Options). Stores a COPY
// behind the pointer so the caller's slice can't be mutated out from under a concurrent read.
func setGrantHMACKey(key []byte) {
	k := key
	grantHMACKey.Store(&k)
}

// userGrantHash derives a stable, non-PII, DNS-1123-safe LABEL VALUE from a raw
// username (e.g. "alice@example.com" or a "system:serviceaccount:ns:sa" string),
// which may be PII and is not label-safe. With a per-cluster HMAC key it is
// HMAC-SHA256; without one it degrades to unsalted SHA-256 (dev / not-yet-hardened
// clusters). Either way the result is hex, truncated to 40 chars (well under the
// 63-char label limit) and prefixed "u-" for a valid RFC-1123 label value.
//
// It is deterministic (same user → same hash, so resolve/revoke re-hash to match)
// and one-way (the raw username is never needed for lookup and never lands in cluster
// metadata). Two distinct usernames collide only under a hash preimage collision
// (infeasible), so per-user isolation holds: user A and B hash differently and never
// share a grant. The key additionally blocks OFFLINE confirmation of a guessed
// username against a leaked hash — the extra guarantee the mcp-owner annotation
// (a wider audience) needs before m25.1b spreads the hash to it.
func userGrantHash(username string) string {
	var key []byte
	if p := grantHMACKey.Load(); p != nil {
		key = *p
	}
	return credresolve.UserHash(key, username)
}

// grantSecretName is the deterministic name of the (user, server) grant Secret. It
// embeds the server name and a SHORT (12-hex) slice of the user hash so a
// re-consent for the SAME (user, server) collides on the same object (idempotent
// upsert), while different users/servers get distinct names. The short slice keeps
// the name within the 253-char object-name limit; the full hash lives in the
// label (the authoritative match key), so the short-slice collision risk only
// affects the NAME, and the label is still checked on read.
// boundary is the grant's trust-boundary hash (ADR 0033); "" here keeps the BFF's direct
// write/read path on the legacy unscoped key. m30.2 threads the invoking agent's registry
// boundary through these wrappers (the token-service delegation already carries it).
func grantSecretName(server, userHash string) string {
	return credresolve.SecretName(server, userHash, "")
}

// grantSecretLabels builds the lookup labels for a (user, server) grant Secret.
// They carry ONLY the hashed user + the server name + the managed-by marker (+ the
// source namespace when grants are consolidated into the locked credential namespace)
// — never any token material (the m17.2 discipline). These are what a resolve/revoke
// matches on. sourceNs is "" in legacy per-namespace mode.
func grantSecretLabels(server, userHash, sourceNs, boundary string) map[string]string {
	return credresolve.SecretLabels(server, userHash, sourceNs, credresolve.BoundaryHash(boundary))
}

// grantSecretCoordinates resolves WHERE the (sourceNs, server, userHash) grant Secret
// lives and what it is named — the single source of truth shared by the consent
// write, the OBO read, the revoke delete, and the refresh path so all four agree.
//
//   - LOCKED mode (credNs != "", m25.1b): every tenant's grant lives in the one
//     RBAC-isolated credential namespace, so the source namespace is folded into the
//     NAME (via a short hash) to keep (ns, server, user) grants distinct there, and
//     mirrored in the labelMCPGrantSourceNS label (the authoritative match on read).
//   - LEGACY mode (credNs == ""): the grant stays in its source namespace under the
//     original (server, user) name — pre-m25.1 clusters, dev, and envtest, unchanged.
func grantSecretCoordinates(credNs, sourceNs, boundary, server, userHash string) (namespace, name string) {
	return credresolve.SecretCoordinates(credNs, sourceNs, server, userHash, credresolve.BoundaryHash(boundary))
}

// --- audit (M11 vocabulary, BFF-side) ---------------------------------------
//
// The M11 audit seam (internal/audit) is a CONTROLLER-side informer over CRD
// mutations — it cannot observe a BFF request-handler action. So the BFF emits the
// SAME structured vocabulary (an action verb + the who/what) as a log line, via the
// server's logger. The entry carries the (user-hash, server) identity + the action
// (create/use/revoke) and NEVER the token — the m17.2 discipline.

// grantAction is the audited action on a per-user grant.
type grantAction string

const (
	// grantActionCreate records a user consenting → their grant is stored.
	grantActionCreate grantAction = "grant.create"
	// grantActionRevoke records a user revoking their own grant.
	grantActionRevoke grantAction = "grant.revoke"
)

// grantAuditEntry is one audited action on a per-user grant. It carries only the
// HASHED user identity + the server + the action — never the raw username and
// NEVER any token material.
type grantAuditEntry struct {
	action    grantAction
	server    string
	userHash  string
	namespace string
}

// grantAuditor emits grant-audit entries to the server logger with stable keys,
// mirroring internal/audit's structured shape. It is deliberately a thin wrapper so
// grant create/use/revoke all audit through ONE place (no ad-hoc log lines that
// might drift or leak a token).
type grantAuditor struct {
	log logr.Logger
}

// newGrantAuditor builds a grant auditor over the given logger.
func newGrantAuditor(log logr.Logger) *grantAuditor {
	return &grantAuditor{log: log.WithName("mcp-grant-audit")}
}

// record emits one audit entry. A nil auditor is a no-op (so callers need not
// nil-check). The entry NEVER contains a token — only the action + (user-hash,
// server) identity.
func (a *grantAuditor) record(e grantAuditEntry) {
	if a == nil {
		return
	}
	a.log.Info("mcp per-user grant",
		"action", string(e.action),
		"server", e.server,
		"user", e.userHash, // HASHED — never the raw username, never a token
		"namespace", e.namespace,
	)
}
