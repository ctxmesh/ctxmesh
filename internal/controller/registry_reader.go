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

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// RegistryReader loads a ToolRegistry by (namespace, name) for binding validation
// (ADR 0043). It is the seam that lets the binding controller read its referenced
// registries from the K8s API today and — after the M43 read-switch + backfill —
// from the control-plane Postgres store, without touching validateBinding.
//
// The not-found contract is load-bearing: a genuine miss returns
// controlplane.ErrNotFound (the caller reports the binding RegistryNotFound, a
// declarative "the registry doesn't exist"), while ANY OTHER error is a real
// read failure and is returned verbatim so resolveAgentBindings requeues instead
// of silently downgrading valid bindings to RegistryNotFound during a transient
// store outage. This distinction is exactly why the CRD stays authoritative in
// M42 and the store read only becomes the default in M43 (a best-effort,
// unbackfilled mirror must never be trusted as the reconcile read source).
type RegistryReader interface {
	GetRegistry(ctx context.Context, namespace, name string) (*agentsv1alpha1.ToolRegistry, error)
}

// NewCRDRegistryReader reads ToolRegistries from the K8s API (the M42 default,
// and the envtest path — no DB). reader is typically the reconciler's cached
// client.
func NewCRDRegistryReader(reader client.Reader) RegistryReader {
	return crdRegistryReader{reader: reader}
}

type crdRegistryReader struct{ reader client.Reader }

func (c crdRegistryReader) GetRegistry(ctx context.Context, namespace, name string) (*agentsv1alpha1.ToolRegistry, error) {
	var reg agentsv1alpha1.ToolRegistry
	if err := c.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &reg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, controlplane.ErrNotFound
		}
		return nil, err
	}
	return &reg, nil
}

// NewPostgresRegistryReader reads ToolRegistries from the control-plane store
// (ADR 0042/0043). Built + tested now as the forward seam; it becomes the
// controller's default only at the M43 read-switch (gated on a complete backfill
// + an authoritative, non-best-effort write path).
func NewPostgresRegistryReader(store toolregistry.Store) RegistryReader {
	return pgRegistryReader{store: store}
}

type pgRegistryReader struct{ store toolregistry.Store }

func (p pgRegistryReader) GetRegistry(ctx context.Context, namespace, name string) (*agentsv1alpha1.ToolRegistry, error) {
	r, err := p.store.Get(ctx, namespace, name)
	if err != nil {
		return nil, err // includes controlplane.ErrNotFound — the caller distinguishes
	}
	return storeRegistryToCRD(r), nil
}

// storeRegistryToCRD projects a control-plane store row back into the CRD shape
// that validateBinding / registryInputSchema / resolveAgentBindings consume — the
// inverse of the BFF's mirrorToolRegistry (m41.2). Only the fields the binding
// path reads are load-bearing (tool Name/Image/URL/InputSchema + the auth-type
// annotation); the rest are carried for fidelity.
//
// The store's Version/CreatedAt/UpdatedAt are deliberately dropped: the binding
// path reads none of them, and the store's OCC Version is not the CRD's
// ObjectMeta.ResourceVersion (different semantics — conflating them would mislead
// the M43 read-switch).
func storeRegistryToCRD(r *toolregistry.ToolRegistry) *agentsv1alpha1.ToolRegistry {
	out := &agentsv1alpha1.ToolRegistry{}
	out.Namespace = r.Namespace
	out.Name = r.Name
	if len(r.Annotations) > 0 {
		out.Annotations = r.Annotations
	}
	if len(r.Labels) > 0 {
		out.Labels = r.Labels
	}
	if len(r.Tools) > 0 {
		out.Spec.Tools = make([]agentsv1alpha1.ToolEntry, len(r.Tools))
		for i := range r.Tools {
			e := r.Tools[i]
			te := agentsv1alpha1.ToolEntry{
				Name:           e.Name,
				Image:          e.Image,
				URL:            e.URL,
				Description:    e.Description,
				Source:         e.Source,
				ApprovalStatus: e.ApprovalStatus,
			}
			if len(e.InputSchema) > 0 {
				te.InputSchema = &runtime.RawExtension{Raw: e.InputSchema}
			}
			out.Spec.Tools[i] = te
		}
	}
	return out
}
