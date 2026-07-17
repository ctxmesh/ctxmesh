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
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
	ManagedByOrgCredential = "agent-engine-mcp-org"
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

// NewOrgCredentialFunc builds the OrgCredential seam for a K8sBackend: it resolves the
// admin-set shared bearer for an ORG-SCOPED server, else ErrNoCredential (so resolution
// falls through to personal-consent / public). isOrgScoped reports whether a server is
// org-scoped (the caller reads the ToolRegistry scope label / route); a nil isOrgScoped or a
// lookup error yields ErrNoCredential (fail-closed — never resolve an org credential for a
// non-org server). credNs is the locked credential namespace ("" ⇒ read in the request ns).
func NewOrgCredentialFunc(
	reader client.Client,
	credNs string,
	isOrgScoped func(ctx context.Context, ns, server string) (bool, error),
) func(ctx context.Context, ns, server string) (Credential, error) {
	return func(ctx context.Context, ns, server string) (Credential, error) {
		if isOrgScoped == nil {
			return Credential{}, ErrNoCredential
		}
		org, err := isOrgScoped(ctx, ns, server)
		if err != nil || !org {
			return Credential{}, ErrNoCredential
		}
		readNS := credNs
		if readNS == "" {
			readNS = ns
		}
		var secret corev1.Secret
		if err := reader.Get(ctx, client.ObjectKey{Name: OrgSecretName(server), Namespace: readNS}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return Credential{}, ErrNoCredential
			}
			return Credential{}, err
		}
		bearer := strings.TrimSpace(string(secret.Data[KeyOrgCredential]))
		if bearer == "" {
			return Credential{}, ErrNoCredential
		}
		return Credential{Kind: KindBearer, Value: bearer}, nil
	}
}
