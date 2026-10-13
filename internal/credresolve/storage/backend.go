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

package storage

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/credresolve/secretbackend"
)

// Deps carries the shared, provider-independent collaborators a backend needs.
type Deps struct {
	// Client reads/writes grant material and the Secrets a backend's configuration names.
	Client client.Client
	// DefaultCredentialNamespace is the locked namespace that holds grant Secrets (kubernetes
	// backend) and every Secret a backend's configuration references.
	DefaultCredentialNamespace string
	// Exchanger performs the OAuth refresh/revoke network calls for passive backends.
	Exchanger credresolve.TokenExchanger
	// AuthTypeIsOAuth reports whether a server needs per-user OAuth (absent grant ⇒
	// consent-required rather than open).
	AuthTypeIsOAuth func(ctx context.Context, ns, server string) (bool, error)
	// IsOrgScoped reports whether a server is org-scoped (so an absent personal grant
	// falls through to the admin-set org credential).
	IsOrgScoped func(ctx context.Context, ns, server string) (bool, error)
	// Audit records credential-plane actions (never a token). Nil ⇒ no-op.
	Audit func(credresolve.AuditEvent)
	// StoreRetry is told each time the postgres backend retries its database at startup, with the
	// error and the wait before the next attempt. Nil ⇒ no-op.
	StoreRetry func(err error, in time.Duration)
}

// BackendFor constructs the CredentialResolver a Config selects. ctx is used to load the Secrets a
// postgres or remote backend references. A backend that cannot be built returns an error, never a
// different backend.
func BackendFor(ctx context.Context, cfg Config, deps Deps) (credresolve.CredentialResolver, error) {
	p := cfg.Provider
	switch {
	case p.Kubernetes != nil:
		credNs := deps.DefaultCredentialNamespace
		return secretbackend.New(secretbackend.Config{
			Client:              deps.Client,
			CredentialNamespace: credNs,
			Exchanger:           deps.Exchanger,
			AuthTypeIsOAuth:     deps.AuthTypeIsOAuth,
			OrgCredential:       secretbackend.OrgCredentialFunc(deps.Client, credNs, deps.IsOrgScoped),
			Audit:               deps.Audit,
		}), nil
	case p.Remote != nil:
		return buildRemoteBackend(ctx, p.Remote, deps)
	case p.Postgres != nil:
		return buildPostgresBackend(ctx, p.Postgres, deps)
	default:
		return nil, errors.New("credential store: no credential backend is configured")
	}
}
