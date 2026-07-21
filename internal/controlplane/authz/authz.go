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

// Package authz gates access to control-plane (Postgres-backed) entities (ADR 0042, m40.2). K8s RBAC
// gates CRD entities via the caller-scoped client (ADR 0011), but once an entity's data lives in
// Postgres the API server is no longer in the read/write path — so the BFF must authorize it itself.
//
// The Authorizer here does so with a SelfSubjectAccessReview submitted through the SAME caller-scoped
// client the BFF already uses (the pattern behind /api/capabilities). That is deliberately NOT a
// TokenReview + SubjectAccessReview from the BFF's own ServiceAccount: the caller-scoped SSAR needs no
// new BFF privilege, is the exact RBAC decision the CRD path made, and keeps working after the entity's
// CRD is deprecated (RBAC rules match by resource name, not by a live CRD). This QUALIFIES ADR 0011:
// RBAC gates CRD entities directly; in-app authz (backed by the same RBAC via SSAR) gates Postgres ones.
package authz

import (
	"context"
	"errors"
	"fmt"

	authzv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrForbidden is returned when the caller is authenticated but not permitted to perform the action —
// the handler maps it to 403 (distinct from a real API error, which is a 500/honest upstream code).
var ErrForbidden = errors.New("authz: forbidden")

// Standard RBAC verbs.
const (
	VerbGet    = "get"
	VerbList   = "list"
	VerbCreate = "create"
	VerbUpdate = "update"
	VerbDelete = "delete"
)

// Action is a K8s-RBAC-shaped authorization query: a verb on a (group, resource) in a namespace, and an
// optional resource name. It maps 1:1 to authorizationv1.ResourceAttributes.
type Action struct {
	Verb      string
	Group     string
	Resource  string
	Namespace string
	Name      string // optional (a named-resource check)
}

// Authorizer decides whether a caller may perform an Action. The caller is identified by the
// caller-scoped client (its bearer token), never by a param — so a handler can't accidentally authorize
// as the wrong identity.
type Authorizer interface {
	Authorize(ctx context.Context, caller client.Client, a Action) error
}

// SSARAuthorizer authorizes via a SelfSubjectAccessReview through the caller-scoped client. Nil-value
// usable (SSARAuthorizer{}).
type SSARAuthorizer struct{}

// Authorize returns nil if allowed, ErrForbidden if the API server denies, or a wrapped error on an API
// failure (never silently allowing on error).
func (SSARAuthorizer) Authorize(ctx context.Context, caller client.Client, a Action) error {
	review := &authzv1.SelfSubjectAccessReview{
		Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:      a.Verb,
				Group:     a.Group,
				Resource:  a.Resource,
				Namespace: a.Namespace,
				Name:      a.Name,
			},
		},
	}
	if err := caller.Create(ctx, review); err != nil {
		return fmt.Errorf("authz: SelfSubjectAccessReview: %w", err)
	}
	if !review.Status.Allowed {
		return ErrForbidden
	}
	return nil
}
