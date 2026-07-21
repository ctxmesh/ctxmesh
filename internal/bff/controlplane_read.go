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
	"errors"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// Resource names for the control-plane read-switch SSAR (ADR 0042 Amendment 4).
// They MUST match the RBAC resource names so the SSAR is the exact decision the
// CRD read made.
const (
	resourceToolRegistries = "toolregistries"
	resourcePromptVersions = "promptversions"
)

// authorizeStoreRead runs a caller-scoped SelfSubjectAccessReview for a
// store-backed read (m43.4). Once a read comes from Postgres the API server is no
// longer in the path, so the BFF must authorize it itself — with the SAME
// caller-scoped SSAR the CRD read implicitly performed (exact RBAC parity, no new
// BFF privilege). Returns nil (allowed), authz.ErrForbidden (denied), or a wrapped
// API error.
func (s *Server) authorizeStoreRead(ctx context.Context, caller client.Client, verb, resource, namespace, name string) error {
	return s.authorizer.Authorize(ctx, caller, authz.Action{
		Verb:      verb,
		Group:     agentsv1alpha1.GroupVersion.Group,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	})
}

// writeAuthzError maps an authorizeStoreRead failure to an honest status: a denial
// is 403 (the caller is authenticated but not permitted), any other error is a 500
// (never silently allow).
func (s *Server) writeAuthzError(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, authz.ErrForbidden) {
		writeError(w, http.StatusForbidden, "you do not have permission to "+action)
		return
	}
	s.log.Error(err, "control-plane read authorization failed", "action", action)
	writeError(w, http.StatusInternalServerError, "authorization check failed")
}
