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

package toolregistry

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// Bounds mirroring the ToolRegistry CRD's OpenAPI/CEL markers (api/v1alpha1/
// toolregistry_types.go) — replicated here so the write path enforces them once
// ToolRegistry writes leave the CRD (ADR 0044). These are duplicated by design:
// the api package must not become a dependency of the CRD-independent control
// plane, and the numbers change ~never (a drift would be caught by the
// conformance test alongside the CRD's own schema tests).
const (
	minTools       = 1
	maxTools       = 200
	maxToolNameLen = 63
	maxImageLen    = 512
	maxURLLen      = 512
	maxDescLen     = 1024

	sourceCurated    = "curated"
	sourceUserAdded  = "user-added"
	approvalApproved = "approved"
	approvalPending  = "pending"
)

// Validate replicates the ToolRegistry CRD's API-server schema validation for the
// write path once ToolRegistry writes leave the CRD (ADR 0044 / M45): a valid
// DNS-1123 namespace + name, 1–200 tools, per-entry length caps, the source /
// approvalStatus enums, and tool-name uniqueness within the registry. It returns
// an error wrapping controlplane.ErrInvalid so the BFF maps it to 422; nil when
// the record is valid.
//
// approvalStatus is validated (a stored row must carry a legal value or empty)
// but NOT set by the console write path — it is controller/approval-owned; the
// BFF handlers carry the live value forward, so a well-formed row always passes.
func Validate(tr ToolRegistry) error {
	if errs := validation.IsDNS1123Label(tr.Namespace); tr.Namespace == "" || len(errs) > 0 {
		return fmt.Errorf("%w: namespace %q is not a valid Kubernetes namespace: %s",
			controlplane.ErrInvalid, tr.Namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(tr.Name); tr.Name == "" || len(errs) > 0 {
		return fmt.Errorf("%w: name %q is not a valid Kubernetes object name: %s",
			controlplane.ErrInvalid, tr.Name, strings.Join(errs, "; "))
	}
	if len(tr.Tools) < minTools {
		return fmt.Errorf("%w: tools must have at least %d entry", controlplane.ErrInvalid, minTools)
	}
	if len(tr.Tools) > maxTools {
		return fmt.Errorf("%w: tools has %d entries, exceeds the maximum of %d",
			controlplane.ErrInvalid, len(tr.Tools), maxTools)
	}
	seen := make(map[string]struct{}, len(tr.Tools))
	for i := range tr.Tools {
		if err := validateToolEntry(tr.Tools[i]); err != nil {
			return err
		}
		if _, dup := seen[tr.Tools[i].Name]; dup {
			return fmt.Errorf("%w: tool name %q is not unique within the registry",
				controlplane.ErrInvalid, tr.Tools[i].Name)
		}
		seen[tr.Tools[i].Name] = struct{}{}
	}
	return nil
}

// validateToolEntry enforces the per-ToolEntry markers: name length 1–63, the
// image/url/description caps, and the two enums (empty allowed — the CRD treats
// an empty source/approvalStatus as its backward-compatible default).
func validateToolEntry(e ToolEntry) error {
	switch {
	case len(e.Name) < 1:
		return fmt.Errorf("%w: each tool entry must have a non-empty name", controlplane.ErrInvalid)
	case len(e.Name) > maxToolNameLen:
		return fmt.Errorf("%w: tool name %q exceeds the maximum length of %d",
			controlplane.ErrInvalid, e.Name, maxToolNameLen)
	case len(e.Image) > maxImageLen:
		return fmt.Errorf("%w: tool %q image exceeds the maximum length of %d",
			controlplane.ErrInvalid, e.Name, maxImageLen)
	case len(e.URL) > maxURLLen:
		return fmt.Errorf("%w: tool %q url exceeds the maximum length of %d",
			controlplane.ErrInvalid, e.Name, maxURLLen)
	case len(e.Description) > maxDescLen:
		return fmt.Errorf("%w: tool %q description exceeds the maximum length of %d",
			controlplane.ErrInvalid, e.Name, maxDescLen)
	}
	if e.Source != "" && e.Source != sourceCurated && e.Source != sourceUserAdded {
		return fmt.Errorf("%w: tool %q source %q must be %q or %q",
			controlplane.ErrInvalid, e.Name, e.Source, sourceCurated, sourceUserAdded)
	}
	if e.ApprovalStatus != "" && e.ApprovalStatus != approvalApproved && e.ApprovalStatus != approvalPending {
		return fmt.Errorf("%w: tool %q approvalStatus %q must be %q or %q",
			controlplane.ErrInvalid, e.Name, e.ApprovalStatus, approvalApproved, approvalPending)
	}
	return nil
}
