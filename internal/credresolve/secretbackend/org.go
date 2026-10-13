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

package secretbackend

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// OrgCredentialFunc builds the OrgCredential seam for a Backend: it resolves the
// admin-set shared bearer for an ORG-SCOPED server, else ErrNoCredential (so resolution
// falls through to personal-consent / public). isOrgScoped reports whether a server is
// org-scoped (the caller reads the ToolRegistry scope label / route); a nil isOrgScoped or a
// lookup error yields ErrNoCredential (fail-closed — never resolve an org credential for a
// non-org server). credNs is the locked credential namespace ("" ⇒ read in the request ns).
func OrgCredentialFunc(
	reader client.Client,
	credNs string,
	isOrgScoped func(ctx context.Context, ns, server string) (bool, error),
) func(ctx context.Context, ns, server string) (credresolve.Credential, error) {
	return func(ctx context.Context, ns, server string) (credresolve.Credential, error) {
		if isOrgScoped == nil {
			return credresolve.Credential{}, credresolve.ErrNoCredential
		}
		org, err := isOrgScoped(ctx, ns, server)
		if err != nil || !org {
			return credresolve.Credential{}, credresolve.ErrNoCredential
		}
		readNS := credNs
		if readNS == "" {
			readNS = ns
		}
		var secret corev1.Secret
		if err := reader.Get(ctx, client.ObjectKey{Name: credresolve.OrgSecretName(server), Namespace: readNS}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return credresolve.Credential{}, credresolve.ErrNoCredential
			}
			return credresolve.Credential{}, err
		}
		bearer := strings.TrimSpace(string(secret.Data[credresolve.KeyOrgCredential]))
		if bearer == "" {
			return credresolve.Credential{}, credresolve.ErrNoCredential
		}
		return credresolve.Credential{Kind: credresolve.KindBearer, Value: bearer}, nil
	}
}
