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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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

// Grant-Secret label/annotation keys + Secret name prefix (ADR 0016 §5). The user
// label value is a HASH of the username (userGrantHash); the server label is the
// register-flow server name. Neither is secret — they are lookup keys only.
const (
	// labelMCPGrantUser holds the HASHED invoking-user identity (userGrantHash). It
	// is a lookup key, never PII: the raw username (which may be an email) is never
	// placed here. Two grants for the same server differ ONLY by this label.
	labelMCPGrantUser = "mcp.ctxmesh.ai/user"
	// labelMCPGrantServer holds the register-flow server name the grant is for. With
	// labelMCPGrantUser it uniquely selects one (user, server) grant Secret.
	labelMCPGrantServer = "mcp.ctxmesh.ai/server"
	// managedByMCPGrant marks a Secret as an OBO per-user grant (the value of
	// labelManagedBy), distinguishing it from a shared register-flow Secret so a
	// grant lookup + a grant revoke never touch a shared credential.
	managedByMCPGrant = "agent-engine-mcp-grant"
	// mcpGrantSecretPrefix prefixes the deterministic (user, server) grant Secret
	// name so it is visibly a grant and never collides with the shared server Secret
	// (named after the server). The name embeds the server + a short user-hash so a
	// re-consent for the SAME (user, server) collides cleanly (idempotent upsert).
	mcpGrantSecretPrefix = "mcp-grant"
)

// annMCPGrantServerURL persists the MCP server URL on the grant Secret (non-secret)
// so a consent flow's callback and a later refresh have the endpoint without a
// second read. It mirrors annMCPURL on the register objects.
const annMCPGrantServerURL = annMCPURL

// errConsentRequired is the distinct signal the OBO resolver returns when the
// invoking user has NO grant for an OAuth server: the caller must prompt the user
// to consent (the m17.2 flow), NOT fall back to a shared credential and NOT fail
// silently. It is a sentinel the egress/loop layer maps to a consent prompt.
var errConsentRequired = errors.New("mcp: per-user consent required for this OAuth server")

// isConsentRequired reports whether err is (or wraps) errConsentRequired — the
// caller uses it to branch to a consent prompt rather than an error surface.
func isConsentRequired(err error) bool { return errors.Is(err, errConsentRequired) }

// userGrantHash derives a stable, non-PII, DNS-1123-safe LABEL VALUE from a raw
// username. The raw username (e.g. "alice@example.com" or a
// "system:serviceaccount:ns:sa" string) may be PII and is not label-safe, so we
// hash it: SHA-256 → hex, truncated to 40 chars (well under the 63-char label
// limit) and prefixed "u-" so it is a valid RFC-1123 label value.
//
// The hash is one-way: the grant Secret's owner is identified by matching the
// same hash at resolve/revoke time, so the raw username is never needed to look a
// grant up and never lands in cluster metadata. Two distinct usernames collide
// only under a SHA-256 preimage collision (infeasible), so per-user isolation
// holds: user A and user B hash to different label values and never share a grant.
func userGrantHash(username string) string {
	sum := sha256.Sum256([]byte(username))
	return "u-" + hex.EncodeToString(sum[:])[:40]
}

// grantSecretName is the deterministic name of the (user, server) grant Secret. It
// embeds the server name and a SHORT (12-hex) slice of the user hash so a
// re-consent for the SAME (user, server) collides on the same object (idempotent
// upsert), while different users/servers get distinct names. The short slice keeps
// the name within the 253-char object-name limit; the full hash lives in the
// label (the authoritative match key), so the short-slice collision risk only
// affects the NAME, and the label is still checked on read.
func grantSecretName(server, userHash string) string {
	short := strings.TrimPrefix(userHash, "u-")
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s-%s-%s", mcpGrantSecretPrefix, server, short)
}

// grantSecretLabels builds the lookup labels for a (user, server) grant Secret.
// They carry ONLY the hashed user + the server name + the managed-by marker —
// never any token material (the m17.2 discipline). These are what a resolve/revoke
// matches on.
func grantSecretLabels(server, userHash string) map[string]string {
	return map[string]string{
		labelManagedBy:      managedByMCPGrant,
		labelMCPGrantUser:   userHash,
		labelMCPGrantServer: server,
	}
}

// oboCredentialResolver is the M17.3 per-user (server, user) resolver. For an OAuth
// server it resolves the INVOKING user's own grant Secret and returns their fresh
// access token; when the user has no grant it returns errConsentRequired. For a
// non-OAuth server (or when per-user is not in play) it delegates to the M14
// shared resolver so the shared-credential mode keeps working unchanged.
//
// The read + refresh run with whichever client the caller supplies (the tool
// proxy's control-plane credential identity at the egress hop, or a caller-scoped
// client). The grant Secret is the SAME trust domain as the shared credentials
// (ADR 0016): readable only by the credential component.
type oboCredentialResolver struct {
	// reader reads + refreshes the grant Secret. Supplied by the egress hop.
	reader client.Client
	// shared is the M14 shared-credential resolver, used for non-OAuth servers so
	// the shared mode is preserved (backward compatibility).
	shared MCPCredentialResolver
	// refresher rotates a near-expiry grant token server-side (the m17.2 helper).
	// It is *Server.refreshMCPOAuthToken in production; a seam so this resolver is
	// unit-testable without a full Server. It returns the fresh access token.
	refresher func(ctx context.Context, c client.Client, ns, secretName string) (string, error)
	// audit records grant use (never the token). Nil → no-op.
	audit *grantAuditor
}

// NewOBOCredentialResolver returns the M17.3 per-user resolver. reader reads the
// grant Secret server-side; shared is the M14 fallback for non-OAuth servers;
// refresh rotates a near-expiry grant; audit records use (may be nil).
func NewOBOCredentialResolver(
	reader client.Client,
	shared MCPCredentialResolver,
	refresh func(ctx context.Context, c client.Client, ns, secretName string) (string, error),
	audit *grantAuditor,
) MCPCredentialResolver {
	return &oboCredentialResolver{reader: reader, shared: shared, refresher: refresh, audit: audit}
}

// Resolve implements MCPCredentialResolver for the per-user case (ADR 0016 §5):
//
//   - the server has a per-user grant for THIS user → refresh it (rotate if near
//     expiry) and return the fresh access token as a bearer credential, auditing
//     the use (never the token);
//   - the server is an OAuth server but this user has NO grant → errConsentRequired
//     (a distinct signal, NOT a shared-credential fallback and NOT a silent
//     failure);
//   - the server is not an OAuth server → delegate to the M14 shared resolver so
//     shared-credential mode is preserved.
//
// Per-user isolation: the grant is selected by BOTH the server name AND the
// invoking user's hash, so user A can never resolve user B's grant.
func (r *oboCredentialResolver) Resolve(ctx context.Context, ns, server, user string) (MCPCredential, error) {
	userHash := userGrantHash(user)

	// Look up THIS user's grant for THIS server (server + user-hash — the pair is
	// the isolation guarantee). A read failure that is NOT "not found" is a real
	// error the caller surfaces.
	secret, found, err := r.findGrant(ctx, ns, server, userHash)
	if err != nil {
		return MCPCredential{}, err
	}
	if !found {
		// No grant for this (user, server). If a shared credential exists for the
		// server (a non-OAuth / M14 server), fall through to it; otherwise the OAuth
		// server needs this user's consent.
		if r.serverIsOAuth(ctx, ns, server) {
			return MCPCredential{}, errConsentRequired
		}
		return r.shared.Resolve(ctx, ns, server, user)
	}

	// The user has a grant. Refresh (rotate if near expiry) server-side and attach
	// the fresh access token. errNoRefreshToken from the refresher means the grant
	// cannot be rotated and is at/near expiry → the user must re-consent.
	access, rErr := r.refresh(ctx, ns, secret.Name)
	if rErr != nil {
		if errors.Is(rErr, errNoRefreshToken) {
			return MCPCredential{}, errConsentRequired
		}
		return MCPCredential{}, rErr
	}

	r.audit.record(grantAuditEntry{action: grantActionUse, server: server, userHash: userHash, namespace: ns})
	return MCPCredential{Kind: credentialKindBearer, Value: access}, nil
}

// refresh calls the injected refresher (the m17.2 refreshMCPOAuthToken) if wired,
// else reads the current access token directly. Keeping it behind the field makes
// the resolver testable without a *Server.
func (r *oboCredentialResolver) refresh(ctx context.Context, ns, secretName string) (string, error) {
	if r.refresher != nil {
		return r.refresher(ctx, r.reader, ns, secretName)
	}
	var secret corev1.Secret
	if err := r.reader.Get(ctx, client.ObjectKey{Name: secretName, Namespace: ns}, &secret); err != nil {
		return "", err
	}
	return string(secret.Data[secretKeyOAuthAccessToken]), nil
}

// findGrant returns the (user, server) grant Secret for the given namespace by its
// deterministic name, verifying the labels match the requested (user-hash, server)
// so a name collision can NEVER return another user's grant (the label is the
// authoritative match). A missing Secret is (nil, false, nil) — the caller decides
// consent-required vs shared-fallback.
func (r *oboCredentialResolver) findGrant(ctx context.Context, ns, server, userHash string) (*corev1.Secret, bool, error) {
	var secret corev1.Secret
	name := grantSecretName(server, userHash)
	if err := r.reader.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Defence in depth: the labels MUST match the requested (user, server). A
	// name-only match without this check could, under a truncated-hash name
	// collision, hand user A user B's grant — the isolation invariant forbids that.
	if secret.Labels[labelMCPGrantUser] != userHash || secret.Labels[labelMCPGrantServer] != server {
		return nil, false, nil
	}
	return &secret, true, nil
}

// serverIsOAuth reports whether the named server was registered as an OAuth server
// (so an absent grant is "consent required" rather than "no credential"). It reads
// the register-managed ToolRegistry's non-secret auth-type annotation. A read
// failure is treated as "not OAuth" (conservative: fall through to shared, which
// itself returns errNoMCPCredential for an open server) so a transient error never
// masquerades as a consent prompt.
func (r *oboCredentialResolver) serverIsOAuth(ctx context.Context, ns, server string) bool {
	var tr agentsv1alpha1.ToolRegistry
	if err := r.reader.Get(ctx, client.ObjectKey{Name: server, Namespace: ns}, &tr); err != nil {
		return false
	}
	return tr.Annotations[annMCPAuthType] == oauthAuthType
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
	// grantActionUse records the resolver attaching a user's grant to a tool call.
	grantActionUse grantAction = "grant.use"
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
