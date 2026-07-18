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

package credstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// GrantLister enumerates legacy grants for a backfill (implemented by credresolve.K8sBackend).
type GrantLister interface {
	ListGrants(ctx context.Context) ([]credresolve.MigratableGrant, error)
}

// Migrate backfills every grant from source into target — a one-time cutover of existing
// k8s-Secret grants into the config-selected backend so no connected account is lost
// (m28.2). It is idempotent: StoreGrant upserts, so re-running is safe.
func Migrate(ctx context.Context, source GrantLister, target credresolve.GrantWriter) (int, error) {
	grants, err := source.ListGrants(ctx)
	if err != nil {
		return 0, err
	}
	migrated := 0
	for _, mg := range grants {
		if err := target.StoreGrant(ctx, mg.Namespace, mg.Server, mg.UserHash, mg.Grant); err != nil {
			return migrated, fmt.Errorf("credstore: migrate grant %s/%s: %w", mg.Namespace, mg.Server, err)
		}
		migrated++
	}
	return migrated, nil
}

// DualRead wraps a primary resolver with a legacy fallback so a resolve during a migration
// window finds a grant WHEREVER it currently lives: try the primary (the new config-selected
// backend), and on a not-found (consent-required / no-credential) fall back to the legacy
// backend. Writes go to the primary; revokes hit BOTH (so a revoke mid-migration is complete).
// Remove the fallback once Migrate has backfilled everything.
type DualRead struct {
	primary  credresolve.CredentialResolver
	fallback credresolve.CredentialResolver
}

// NewDualRead builds a dual-read resolver.
func NewDualRead(primary, fallback credresolve.CredentialResolver) *DualRead {
	return &DualRead{primary: primary, fallback: fallback}
}

// Resolve tries the primary, then the legacy fallback ONLY on a not-found signal.
func (d *DualRead) Resolve(ctx context.Context, ns, server, userHash string) (credresolve.Credential, error) {
	cred, err := d.primary.Resolve(ctx, ns, server, userHash)
	if err == nil {
		return cred, nil
	}
	if errors.Is(err, credresolve.ErrConsentRequired) || errors.Is(err, credresolve.ErrNoCredential) {
		if c2, e2 := d.fallback.Resolve(ctx, ns, server, userHash); e2 == nil {
			return c2, nil
		}
	}
	return cred, err // preserve the primary's (semantic) error — e.g. consent_required
}

// Revoke revokes in BOTH backends (best-effort) so a revoke during migration is complete.
func (d *DualRead) Revoke(ctx context.Context, ns, server, userHash string) error {
	return errors.Join(
		d.primary.Revoke(ctx, ns, server, userHash),
		d.fallback.Revoke(ctx, ns, server, userHash),
	)
}

// StoreGrant writes to the PRIMARY (new writes land in the config-selected backend), if it
// supports writes.
func (d *DualRead) StoreGrant(ctx context.Context, ns, server, userHash string, g credresolve.Grant) error {
	w, ok := d.primary.(credresolve.GrantWriter)
	if !ok {
		return fmt.Errorf("credstore: dual-read primary backend does not support writes")
	}
	return w.StoreGrant(ctx, ns, server, userHash, g)
}

// Compile-time assertions.
var (
	_ credresolve.CredentialResolver = (*DualRead)(nil)
	_ credresolve.GrantWriter        = (*DualRead)(nil)
)
