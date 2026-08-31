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

package statelayer

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// tenantLabel is the AUTHORITATIVE namespace label the Tenant controller stamps
// (internal/controller/tenant_controller.go). The proxy resolves a namespace's
// tenant from THIS label — exactly as the controller's resolveTenantForNamespace
// does — so the proxy's tenant id is byte-identical to the launcher's injected
// TENANT_ID (the Valkey key dimension `tenant:{id}:*`), keeping both sides on the
// same quota accumulator.
//
// It is deliberately NOT re-derived from Tenant.spec.namespaces (ADR 0050
// Amendment 2, Correction 2a): the controller avoids that on purpose because the
// spec is desired-state and can name a namespace the controller has not actually
// reconciled (a race), or a namespace two Tenants both claim (a double-claim
// ambiguity). The stamped label is the reconciled truth, so it can never diverge
// from what the launcher was injected with.
//
// The value is the shared api/v1alpha1.TenantLabel — one source of truth with the
// controller that stamps it, so this security-critical key can never drift.
const tenantLabel = agentsv1alpha1.TenantLabel

// TenantResolver maps a namespace to the id of the tenant that owns it.
type TenantResolver interface {
	// TenantID returns the owning tenant's id, or "" when the namespace belongs to
	// no tenant. A non-nil error is an infrastructure failure (the cache/API is
	// unreachable) — never a "not found", which is the untenanted "" case.
	TenantID(ctx context.Context, namespace string) (string, error)
}

// labelTenantResolver reads the tenant label off a (cached) Namespace.
type labelTenantResolver struct {
	reader client.Reader
}

// NewLabelTenantResolver builds a TenantResolver backed by a namespace reader —
// in production a controller-runtime cache restricted to corev1.Namespace, so the
// hot path is a local read, not an API round-trip.
func NewLabelTenantResolver(reader client.Reader) TenantResolver {
	return &labelTenantResolver{reader: reader}
}

func (r *labelTenantResolver) TenantID(ctx context.Context, namespace string) (string, error) {
	if namespace == "" {
		return "", nil
	}
	var ns corev1.Namespace
	if err := r.reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil // an unknown namespace is untenanted, not an error
		}
		return "", err
	}
	return ns.Labels[tenantLabel], nil
}

// StartNamespaceResolver builds a controller-runtime cache restricted to
// corev1.Namespace, starts it on ctx (process lifetime), waits for the initial
// sync so the first lookup is never a cold miss, and returns a TenantResolver
// backed by it. ReaderFailOnMissingInformer keeps the cache from lazily spinning
// up informers for any other type — it only ever caches Namespaces, matching the
// proxy's get/list/watch-namespaces RBAC. Shared by the proxy (in-cluster) and by
// envtest.
func StartNamespaceResolver(ctx context.Context, restCfg *rest.Config, syncTimeout time.Duration) (TenantResolver, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	c, err := cache.New(restCfg, cache.Options{
		Scheme:                      scheme,
		ReaderFailOnMissingInformer: true,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Namespace{}: {},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build namespace cache: %w", err)
	}
	// Register the Namespace informer up front so WaitForCacheSync actually waits
	// for it (informers are otherwise created lazily on first read).
	if _, err := c.GetInformer(ctx, &corev1.Namespace{}); err != nil {
		return nil, fmt.Errorf("namespace informer: %w", err)
	}
	go func() {
		// A cache-start failure would otherwise be silent: the resolver would look
		// "ready" while every read returns ErrCacheNotStarted → quota unavailable
		// with no root-cause log. Surface it (logger derived from ctx — logcheck).
		if err := c.Start(ctx); err != nil && ctx.Err() == nil {
			logf.FromContext(ctx).Error(err, "statelayer namespace cache exited with error")
		}
	}()
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !c.WaitForCacheSync(syncCtx) {
		return nil, errors.New("namespace cache did not sync")
	}
	return NewLabelTenantResolver(c), nil
}
