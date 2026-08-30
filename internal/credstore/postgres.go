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
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net/http"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/credpostgres"
	"github.com/ctxmesh/agentry/internal/credresolve"
	"github.com/ctxmesh/agentry/internal/dbpool"
)

// buildPostgresBackend constructs the Postgres reference backend: open the DB from the DSN
// Secret, build the envelope Sealer from the configured KEK, and wrap the sql store. The
// org-credential path stays a k8s Secret in the credential namespace (personal grants move
// to Postgres; the admin-set org credential does not).
func buildPostgresBackend(ctx context.Context, spec *agentsv1alpha1.CredentialProviderPostgres, deps Deps) (credresolve.CredentialResolver, error) {
	if spec.Encryption == nil {
		return nil, fmt.Errorf("credstore: postgres backend requires encryption (a Postgres store must not persist plaintext tokens)")
	}
	dsn, err := secretValue(ctx, deps.Client, deps.DefaultCredentialNamespace, spec.DSNSecretRef.Name, spec.DSNSecretRef.Key)
	if err != nil {
		return nil, fmt.Errorf("credstore: load postgres DSN: %w", err)
	}
	sealer, err := buildSealer(ctx, spec.Encryption, deps)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", string(dsn))
	if err != nil {
		return nil, fmt.Errorf("credstore: open postgres: %w", err)
	}
	dbpool.Apply(db, "CREDSTORE_MAX_OPEN_CONNS", 5) // F-8: bound the pool (ADR 0097)
	store, err := credpostgres.NewStore(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return credpostgres.NewBackend(credpostgres.BackendConfig{
		Storage:         store,
		Sealer:          sealer,
		Exchanger:       deps.Exchanger,
		AuthTypeIsOAuth: deps.AuthTypeIsOAuth,
		OrgCredential:   credresolve.NewOrgCredentialFunc(deps.Client, deps.DefaultCredentialNamespace, deps.IsOrgScoped),
		Audit:           deps.Audit,
	})
}

// buildSealer builds the envelope Sealer from the CredentialStore encryption config.
// localKEKSecretRef → LocalSealer (m27.4); kmsV2 → not yet built (m27.5).
func buildSealer(ctx context.Context, enc *agentsv1alpha1.EnvelopeEncryption, deps Deps) (credpostgres.Sealer, error) {
	switch {
	case enc.LocalKEKSecretRef != nil:
		kek, err := secretValue(ctx, deps.Client, deps.DefaultCredentialNamespace, enc.LocalKEKSecretRef.Name, enc.LocalKEKSecretRef.Key)
		if err != nil {
			return nil, fmt.Errorf("credstore: load local KEK: %w", err)
		}
		return credpostgres.NewLocalSealer(kek)
	case enc.OpenBaoTransit != nil:
		return buildTransitSealer(ctx, enc.OpenBaoTransit, deps)
	case enc.KMSv2 != nil:
		return credpostgres.NewKMSv2Sealer(enc.KMSv2.Endpoint)
	default:
		return nil, fmt.Errorf("credstore: postgres encryption has no KEK custodian")
	}
}

// buildTransitSealer builds an OpenBao transit Sealer: load the token (+ optional CA) from
// Secrets and construct the transit client.
func buildTransitSealer(ctx context.Context, cfg *agentsv1alpha1.OpenBaoTransitKMS, deps Deps) (credpostgres.Sealer, error) {
	token, err := secretValue(ctx, deps.Client, deps.DefaultCredentialNamespace, cfg.TokenSecretRef.Name, cfg.TokenSecretRef.Key)
	if err != nil {
		return nil, fmt.Errorf("credstore: load openbao token: %w", err)
	}
	httpClient := &http.Client{Timeout: remoteTimeout}
	if cfg.CASecretRef != nil {
		caPEM, cErr := secretValue(ctx, deps.Client, deps.DefaultCredentialNamespace, cfg.CASecretRef.Name, cfg.CASecretRef.Key)
		if cErr != nil {
			return nil, fmt.Errorf("credstore: load openbao CA: %w", cErr)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("credstore: openbao CA is not valid PEM")
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return credpostgres.NewTransitSealer(credpostgres.TransitSealerConfig{
		Address:   cfg.Address,
		MountPath: cfg.MountPath,
		Token:     string(token),
		KeyPrefix: cfg.KeyPrefix,
		HTTP:      httpClient,
	})
}
