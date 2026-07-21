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

package promptversion

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// Validate replicates the PromptVersion CRD's API-server schema validation for the
// write path once PromptVersion writes leave the CRD (ADR 0044): the git pointer
// fields are all required non-empty (MinLength=1 in api/v1alpha1), and the object
// name/namespace must be valid Kubernetes identifiers (the API server enforced
// DNS-1123 subdomain names). It returns an error wrapping controlplane.ErrInvalid
// so the BFF maps it to 422; nil when the record is valid.
func Validate(pv PromptVersion) error {
	if strings.TrimSpace(pv.Namespace) == "" {
		return fmt.Errorf("%w: namespace is required", controlplane.ErrInvalid)
	}
	if errs := validation.IsDNS1123Subdomain(pv.Name); pv.Name == "" || len(errs) > 0 {
		return fmt.Errorf("%w: name %q is not a valid Kubernetes object name: %s",
			controlplane.ErrInvalid, pv.Name, strings.Join(errs, "; "))
	}
	if strings.TrimSpace(pv.Repo) == "" {
		return fmt.Errorf("%w: git.repo is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(pv.Ref) == "" {
		return fmt.Errorf("%w: git.ref is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(pv.Path) == "" {
		return fmt.Errorf("%w: git.path is required", controlplane.ErrInvalid)
	}
	return nil
}
