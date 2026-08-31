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

package credresolve

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// The grant-Secret SCHEMA — the single source of truth for WHERE a (source-ns,
// server, user) OBO grant lives, what it is NAMED, and what non-secret LOOKUP KEYS
// select it. The BFF consent flow WRITES with exactly these; every credential
// reader (sidecar / central service / the BFF's own resolver) reads with them.
// A mismatch here silently loses tokens, so it is centralized (ADR 0029 §5/§7).
const (
	// LabelManagedBy / ManagedByGrant mark a Secret as an OBO per-user grant so a
	// grant lookup or revoke never touches a shared register-flow credential.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByGrant = "ctxmesh-mcp-grant"

	// LabelGrantUser holds the HASHED invoking-user identity (UserHash) — a lookup
	// key, never PII, never a token. Two grants for the same server differ ONLY here.
	LabelGrantUser = "mcp.ctxmesh.ai/user"
	// LabelGrantServer holds the register-flow server name the grant is for. With
	// LabelGrantUser it uniquely selects one (user, server) grant Secret.
	LabelGrantServer = "mcp.ctxmesh.ai/server"
	// LabelGrantSourceNS records the ORIGIN namespace (the agent namespace the consent
	// ran in). In LOCKED mode one namespace holds every tenant's grants, so the source
	// namespace is folded into the Secret NAME and mirrored here as the authoritative
	// match key. Empty/absent in legacy per-namespace mode. Non-secret — a lookup key.
	LabelGrantSourceNS = "mcp.ctxmesh.ai/source-namespace"

	// LabelGrantBoundary holds the HASHED trust boundary a personal grant is scoped to
	// (ADR 0033): the registry an agent belongs to (agents in it collaborate + share the
	// user's credential) or, for a standalone agent, the agent itself. With LabelGrantUser
	// + LabelGrantServer it selects one (user, boundary, server) grant. EMPTY/ABSENT = a
	// legacy unscoped (user, server) grant — so old grants resolve unchanged and adopt into
	// a boundary on first scoped use (dual-read migration). Non-secret — a lookup key.
	LabelGrantBoundary = "mcp.ctxmesh.ai/boundary"

	// AnnGrantBoundary mirrors the RAW boundary string (e.g. "r:my-registry") on the grant
	// for debuggability; the label holds the hash (the match key). Non-secret.
	AnnGrantBoundary = "agents.ctxmesh.ai/mcp-boundary"

	// SecretPrefix prefixes the deterministic grant Secret name so it is visibly a
	// grant and never collides with the shared server Secret (named after the server).
	SecretPrefix = "mcp-grant"

	// AnnServerURL persists the MCP server URL on the grant Secret (non-secret) so a
	// refresh has the endpoint without a second read. Mirrors the register objects.
	AnnServerURL = "agents.ctxmesh.ai/mcp-url"
)

// UserHash derives a stable, non-PII, DNS-1123-safe LABEL VALUE from a raw username
// (an email or a "system:serviceaccount:ns:sa" string), which may be PII and is not
// label-safe. With a per-cluster HMAC key it is HMAC-SHA256; without one it degrades
// to unsalted SHA-256 (dev / not-yet-hardened clusters — the key is a documented
// production prerequisite, ADR 0029 §7). The result is hex, truncated to 40 chars
// (well under the 63-char label limit) and prefixed "u-" for a valid RFC-1123 label.
//
// It is deterministic (same user → same hash, so resolve/revoke re-hash to match) and
// one-way (the raw username never lands in cluster metadata). Distinct usernames
// collide only under a hash preimage collision (infeasible), so per-user isolation
// holds. The HMAC key additionally blocks OFFLINE confirmation of a guessed username
// against a leaked hash.
//
// This is the ONE place the hash is computed; both the BFF (which holds the key and
// hashes at capability mint + consent write) and any tooling call through here so the
// writer and reader always agree.
func UserHash(hmacKey []byte, username string) string {
	if len(hmacKey) > 0 {
		mac := hmac.New(sha256.New, hmacKey)
		_, _ = mac.Write([]byte(username))
		return "u-" + hex.EncodeToString(mac.Sum(nil))[:40]
	}
	sum := sha256.Sum256([]byte(username))
	return "u-" + hex.EncodeToString(sum[:])[:40]
}

// EndUserHash derives the run-capability subject for an END-USER identity keyed on (issuer, subject)
// (M137/EU1b, ADR 0106 §5). It is domain-separated from UserHash TWO ways so an end-user subject can
// never share an identity with a K8s console user:
//   - a distinct "eu-" prefix (UserHash is always "u-"), so the two output spaces never overlap as
//     strings — an end-user's grant/memory key can never equal a K8s username's; AND
//   - a distinct HMAC message with a domain tag + LENGTH-PREFIXED (issuer, subject) fields, so two
//     different (iss,sub) pairs can never canonicalize to the same input (the "https://a"+"b" vs
//     "https://ab"+"" concatenation collision) and the input can never equal UserHash's bare username.
//
// The identity KEY is (iss, sub) — never email (mutable/reassignable ⇒ account takeover). When hmacKey
// is empty this falls back to an unsalted SHA-256; callers minting an END-USER capability MUST require a
// key (an unsalted end-user hash is enumerable — ADR 0106 §5).
func EndUserHash(hmacKey []byte, issuer, subject string) string {
	msg := make([]byte, 0, 32+len(issuer)+len(subject))
	msg = append(msg, "ctxmesh-end-user-v1\x00"...)
	msg = appendLenPrefixed(msg, issuer)
	msg = appendLenPrefixed(msg, subject)
	if len(hmacKey) > 0 {
		mac := hmac.New(sha256.New, hmacKey)
		_, _ = mac.Write(msg)
		return "eu-" + hex.EncodeToString(mac.Sum(nil))[:40]
	}
	sum := sha256.Sum256(msg)
	return "eu-" + hex.EncodeToString(sum[:])[:40]
}

// appendLenPrefixed writes an 8-byte big-endian length followed by the bytes of s — unambiguous framing
// so concatenated fields can never be confused across a field boundary.
func appendLenPrefixed(dst []byte, s string) []byte {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	dst = append(dst, lenBuf[:]...)
	return append(dst, s...)
}

// RegistryBoundary is the trust boundary for an agent that belongs to a registry — the
// collaboration boundary (ADR 0033): every agent in the registry shares the invoking
// user's personal grant. Empty registry → "" (an unscoped/legacy grant).
func RegistryBoundary(registry string) string {
	if registry == "" {
		return ""
	}
	return "r:" + registry
}

// AgentBoundary is the trust boundary for a STANDALONE agent (no registry) — the agent is
// its own boundary, so its grants are per-agent (ADR 0033). Namespaced to avoid a registry
// and an agent of the same name colliding.
func AgentBoundary(ns, agent string) string {
	return "a:" + ns + "/" + agent
}

// BoundaryHash derives a short, label/name-safe token from a raw boundary string
// ("r:<registry>" / "a:<ns>/<agent>"). Empty boundary → "" (the legacy unscoped key, so
// pre-ADR-0033 grant names/labels are byte-identical and resolve unchanged).
func BoundaryHash(boundary string) string {
	if boundary == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(boundary))
	return "b-" + hex.EncodeToString(sum[:])[:16]
}

// SecretName is the deterministic name of the (user, boundary, server) grant Secret. It
// embeds the server name, a SHORT (12-hex) slice of the user hash, and — when the grant is
// boundary-scoped (ADR 0033) — a short boundary-hash segment, so a re-consent for the SAME
// (user, boundary, server) collides on the same object (idempotent upsert) while different
// users/boundaries/servers get distinct names. An EMPTY boundaryHash yields the exact
// legacy name (backward compatible). The full hashes live in labels (the authoritative
// match keys), so short-slice collisions only affect the NAME, still checked on read.
func SecretName(server, userHash, boundaryHash string) string {
	short := strings.TrimPrefix(userHash, "u-")
	if len(short) > 12 {
		short = short[:12]
	}
	name := fmt.Sprintf("%s-%s-%s", SecretPrefix, server, short)
	if boundaryHash != "" {
		bshort := strings.TrimPrefix(boundaryHash, "b-")
		if len(bshort) > 12 {
			bshort = bshort[:12]
		}
		name += "-" + bshort
	}
	return name
}

// ShortNamespaceHash is a stable 8-hex slice of sha256(sourceNs), used only to keep
// grant Secret NAMES distinct when many namespaces' grants share the locked credential
// namespace. The full source namespace is the label (the match key), so this short
// slice's only job is object-name uniqueness, not security.
func ShortNamespaceHash(ns string) string {
	sum := sha256.Sum256([]byte(ns))
	return hex.EncodeToString(sum[:])[:8]
}

// SecretCoordinates resolves WHERE the (sourceNs, server, userHash) grant Secret lives
// and what it is named — the single truth shared by the consent write, the OBO read,
// the revoke delete, and the refresh path so all four agree.
//
//   - LOCKED mode (credNs != ""): every tenant's grant lives in the one RBAC-isolated
//     credential namespace, so the source namespace is folded into the NAME (short
//     hash) to keep grants distinct there, and mirrored in LabelGrantSourceNS (the
//     authoritative match on read).
//   - LEGACY mode (credNs == ""): the grant stays in its source namespace under the
//     original (server, user) name — pre-locked-namespace clusters, dev, envtest.
func SecretCoordinates(credNs, sourceNs, server, userHash, boundaryHash string) (namespace, name string) {
	base := SecretName(server, userHash, boundaryHash)
	if credNs == "" {
		return sourceNs, base
	}
	return credNs, base + "-" + ShortNamespaceHash(sourceNs)
}

// SecretLabels builds the lookup labels for a (user, boundary, server) grant Secret: ONLY
// the hashed user + the server + the managed-by marker (+ the source namespace in locked
// mode, + the hashed boundary when boundary-scoped) — never any token material. These are
// what a resolve/revoke matches on. sourceNs is "" in legacy per-namespace mode;
// boundaryHash is "" for a legacy unscoped grant (ADR 0033).
func SecretLabels(server, userHash, sourceNs, boundaryHash string) map[string]string {
	labels := map[string]string{
		LabelManagedBy:   ManagedByGrant,
		LabelGrantUser:   userHash,
		LabelGrantServer: server,
	}
	if sourceNs != "" {
		labels[LabelGrantSourceNS] = sourceNs
	}
	if boundaryHash != "" {
		labels[LabelGrantBoundary] = boundaryHash
	}
	return labels
}
