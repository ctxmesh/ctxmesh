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

// Package credresolve is the credential-plane: it resolves the on-behalf-of (OBO)
// credential to attach to an agent's MCP tool call — the invoking user's own,
// freshly-refreshed access token — and is the single source of truth for the
// grant-Secret WIRE FORMAT (label/data-key schema, coordinate layout, user-hash)
// so the consent WRITER (the BFF) and every credential READER agree byte-for-byte.
//
// It is deliberately STANDALONE (no dependency on package bff or on any CRD) so
// the two-tier OBO topology (ADR 0030) can import it from where the credential is
// actually needed:
//
//   - the per-pod injecting egress sidecar (embeds a K8sBackend for the first cut),
//   - the central token service (runs a K8sBackend behind an internal API),
//   - the BFF (delegates its grant schema + OAuth refresh here, no duplication).
//
// Design invariants (ADR 0029 §5/§7, ADR 0030):
//
//   - Identity is the already-HASHED user (a capability's `sub`, minted by the BFF).
//     The backend never sees a raw username and never holds the per-cluster HMAC key
//     — hashing happens once, at capability mint. UserHash lives here as the shared
//     function so the writer and reader hash identically, but the backend operates
//     purely in user-hash space.
//   - Tokens live ONLY in the grant Secret (and transiently in memory during a
//     refresh). They are NEVER placed in a label, annotation, DTO, or log line.
//   - Concurrent refresh at expiry is singleflighted PER GRANT (in-process herd) and
//     written back with OPTIMISTIC CONCURRENCY (resourceVersion) so two callers never
//     persist a stale token (ADR 0029 §6 R3). Cross-PROCESS deduplication of the
//     OAuth refresh is the central token service's job (global singleflight, ADR 0030
//     increment 5); until then a cross-process conflict re-reads and adopts the
//     winner's fresh token rather than re-calling the authorization server.
//   - Revocation is "forget + best-effort revoke" (RFC 7009): delete the grant Secret
//     (so we forget) and, when a revocation endpoint is stored, POST a best-effort
//     revoke to the authorization server (in-flight calls may complete).
package credresolve
