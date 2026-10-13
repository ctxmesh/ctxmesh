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

// The ADMIN-SET SHARED ORG credential (ADR 0029 §1/§7) — a single Secret per org-scoped
// server holding one bearer used by everyone, no per-user consent. It is the org analogue
// of a per-user grant: same locked-namespace trust domain, but keyed by SERVER only (not
// user). Only an admin (RBAC on the credential namespace) may write it; the resolver reads
// it when the invoker has no personal grant and the server is org-scoped.
const (
	// OrgSecretPrefix prefixes the org-credential Secret name so it never collides with a
	// per-user grant (mcp-grant-*) or the register-flow server Secret.
	OrgSecretPrefix = "mcp-org"
	// ManagedByOrgCredential marks a Secret as an org credential (value of LabelManagedBy).
	ManagedByOrgCredential = "ctxmesh-mcp-org"
	// LabelOrgServer holds the server the org credential is for (a lookup key).
	LabelOrgServer = LabelGrantServer
	// KeyOrgCredential is the Secret data key holding the shared bearer.
	KeyOrgCredential = "org-credential"
)

// OrgSecretName is the deterministic name of a server's org-credential Secret.
func OrgSecretName(server string) string {
	return OrgSecretPrefix + "-" + server
}

// OrgSecretLabels are the lookup labels for an org-credential Secret (never a token).
func OrgSecretLabels(server string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByOrgCredential,
		LabelOrgServer: server,
	}
}

// OrgSecretData builds the org-credential Secret data from a shared bearer key.
func OrgSecretData(bearer string) map[string][]byte {
	return map[string][]byte{KeyOrgCredential: []byte(bearer)}
}
