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

// Package credstore selects the credential backend for an OBO grant from the
// CredentialStore / ClusterCredentialStore CRDs (ADR 0032, spec credential-store-spi),
// and routes each (namespace, server, user) resolution to the chosen backend. It is the
// config layer over internal/credresolve: the backend is a config choice, not a rebuild.
//
// The default (no CRD present) is the built-in kubernetes backend, so existing installs
// are unchanged. Backends are memoized by their resolved configuration so every namespace
// on the same config shares ONE backend instance — preserving the global cache +
// singleflight the central token-service relies on (ADR 0030 §1).
package credstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// DefaultStoreName is the conventional name of the namespaced CredentialStore that
// overrides the cluster default for its namespace, and of the cluster-wide default
// ClusterCredentialStore. Modeled on a "default store" convention (ESO references a store
// by name; here the consuming grant carries no explicit ref, so selection is by name).
const DefaultStoreName = "default"

// defaultSelectionTTL bounds how long a namespace→backend selection is cached, keeping the
// CredentialStore lookup off the per-resolve hot path while letting a store change
// propagate within the window.
const defaultSelectionTTL = 60 * time.Second

// ErrProviderNotImplemented is returned when a CredentialStore names a backend that this
// build does not yet implement (postgres → m27.4, grpc → m27.3). It fails closed: no
// credential is resolved, never a wrong or blank one.
var ErrProviderNotImplemented = errors.New("credstore: credential provider not implemented in this build")

// Deps carries the shared, provider-independent collaborators a backend needs. The router
// injects these into whichever backend a CredentialStore selects.
type Deps struct {
	// Client reads/writes grant material and (for the kubernetes backend) the locked
	// credential namespace.
	Client client.Client
	// DefaultCredentialNamespace is the locked namespace used when a kubernetes provider
	// does not override it.
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
}

// ImplicitKubernetesSpec is the store used when no CredentialStore/ClusterCredentialStore
// exists — the zero-dependency default, so existing installs behave exactly as before.
func ImplicitKubernetesSpec() agentsv1alpha1.CredentialStoreSpec {
	return agentsv1alpha1.CredentialStoreSpec{
		Provider: agentsv1alpha1.CredentialStoreProvider{
			Kubernetes: &agentsv1alpha1.CredentialProviderKubernetes{},
		},
	}
}

// SelectSpec resolves the effective credential-store spec for a source namespace:
//  1. a CredentialStore named DefaultStoreName in that namespace (per-namespace override);
//  2. else the ClusterCredentialStore named DefaultStoreName (cluster default);
//  3. else the implicit kubernetes backend (existing installs unchanged).
//
// The returned source string names which store won, for logging/audit.
func SelectSpec(ctx context.Context, reader client.Reader, ns string) (agentsv1alpha1.CredentialStoreSpec, string, error) {
	var cs agentsv1alpha1.CredentialStore
	err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: DefaultStoreName}, &cs)
	switch {
	case err == nil:
		return cs.Spec, fmt.Sprintf("credentialstore/%s/%s", ns, DefaultStoreName), nil
	case !apierrors.IsNotFound(err):
		return agentsv1alpha1.CredentialStoreSpec{}, "", fmt.Errorf("credstore: get CredentialStore %s/%s: %w", ns, DefaultStoreName, err)
	}

	var ccs agentsv1alpha1.ClusterCredentialStore
	err = reader.Get(ctx, client.ObjectKey{Name: DefaultStoreName}, &ccs)
	switch {
	case err == nil:
		return ccs.Spec, fmt.Sprintf("clustercredentialstore/%s", DefaultStoreName), nil
	case !apierrors.IsNotFound(err):
		return agentsv1alpha1.CredentialStoreSpec{}, "", fmt.Errorf("credstore: get ClusterCredentialStore %s: %w", DefaultStoreName, err)
	}

	return ImplicitKubernetesSpec(), "implicit-kubernetes", nil
}

// BackendFor constructs a CredentialResolver for a resolved store spec. kubernetes (m27.2)
// and remote (m27.3) are built; the others fail closed with ErrProviderNotImplemented
// until their tasks land. ctx is used to load a remote backend's mTLS material.
func BackendFor(ctx context.Context, spec agentsv1alpha1.CredentialStoreSpec, deps Deps) (credresolve.CredentialResolver, error) {
	p := spec.Provider
	switch {
	case p.Kubernetes != nil:
		credNs := p.Kubernetes.CredentialNamespace
		if credNs == "" {
			credNs = deps.DefaultCredentialNamespace
		}
		return credresolve.NewK8sBackend(credresolve.K8sBackendConfig{
			Client:              deps.Client,
			CredentialNamespace: credNs,
			Exchanger:           deps.Exchanger,
			AuthTypeIsOAuth:     deps.AuthTypeIsOAuth,
			OrgCredential:       credresolve.NewOrgCredentialFunc(deps.Client, credNs, deps.IsOrgScoped),
			Audit:               deps.Audit,
		}), nil
	case p.Remote != nil:
		return buildRemoteBackend(ctx, p.Remote, deps)
	case p.Postgres != nil:
		return nil, fmt.Errorf("%w: postgres (m27.4)", ErrProviderNotImplemented)
	case p.OpenBao != nil:
		return nil, fmt.Errorf("%w: openbao", ErrProviderNotImplemented)
	default:
		return nil, errors.New("credstore: CredentialStore has no provider set")
	}
}

// backendKey is a stable identity for a resolved backend config, so identical configs
// share ONE backend instance (global cache + singleflight preserved). Distinct configs
// get distinct instances.
func backendKey(spec agentsv1alpha1.CredentialStoreSpec, deps Deps) string {
	p := spec.Provider
	switch {
	case p.Kubernetes != nil:
		credNs := p.Kubernetes.CredentialNamespace
		if credNs == "" {
			credNs = deps.DefaultCredentialNamespace
		}
		return "kubernetes:" + credNs
	case p.Postgres != nil:
		return "postgres:" + p.Postgres.DSNSecretRef.Name + "/" + p.Postgres.DSNSecretRef.Key
	case p.OpenBao != nil:
		return "openbao:" + p.OpenBao.Address
	case p.Remote != nil:
		return "remote:" + p.Remote.Endpoint
	default:
		return "none"
	}
}

// Router implements credresolve.CredentialResolver by resolving the effective
// CredentialStore per request namespace and delegating to the selected backend.
type Router struct {
	reader client.Reader
	deps   Deps
	ttl    time.Duration
	now    func() time.Time

	mu       sync.Mutex
	backends map[string]credresolve.CredentialResolver // key → memoized backend
	nsCache  map[string]nsSelection                    // namespace → cached selection
}

type nsSelection struct {
	backend credresolve.CredentialResolver
	expires time.Time
}

// NewRouter builds a config-selected router. reader reads the CredentialStore CRDs (a
// cached reader is preferable so selection is cheap); deps are the shared backend
// collaborators.
func NewRouter(reader client.Reader, deps Deps) *Router {
	return &Router{
		reader:   reader,
		deps:     deps,
		ttl:      defaultSelectionTTL,
		now:      time.Now,
		backends: map[string]credresolve.CredentialResolver{},
		nsCache:  map[string]nsSelection{},
	}
}

// Resolve routes to the backend selected for ns.
func (r *Router) Resolve(ctx context.Context, ns, server, userHash string) (credresolve.Credential, error) {
	b, err := r.backendFor(ctx, ns)
	if err != nil {
		return credresolve.Credential{}, err
	}
	return b.Resolve(ctx, ns, server, userHash)
}

// Revoke routes to the backend selected for ns.
func (r *Router) Revoke(ctx context.Context, ns, server, userHash string) error {
	b, err := r.backendFor(ctx, ns)
	if err != nil {
		return err
	}
	return b.Revoke(ctx, ns, server, userHash)
}

func (r *Router) backendFor(ctx context.Context, ns string) (credresolve.CredentialResolver, error) {
	now := r.now()
	r.mu.Lock()
	if sel, ok := r.nsCache[ns]; ok && now.Before(sel.expires) {
		r.mu.Unlock()
		return sel.backend, nil
	}
	r.mu.Unlock()

	// Selection reads happen outside the lock (an apiserver Get); TTL keeps them off the
	// per-resolve hot path.
	spec, _, err := SelectSpec(ctx, r.reader, ns)
	if err != nil {
		return nil, err
	}
	key := backendKey(spec, r.deps)

	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[key]
	if !ok {
		b, err = BackendFor(ctx, spec, r.deps)
		if err != nil {
			return nil, err
		}
		r.backends[key] = b
	}
	r.nsCache[ns] = nsSelection{backend: b, expires: now.Add(r.ttl)}
	return b, nil
}
