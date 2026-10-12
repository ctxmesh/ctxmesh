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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
)

// Reading the retired AgentVersion objects is all the import needs. It is kept for one release after
// the CRD's retirement, so an upgrade from a release that wrote them keeps its history.
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentversions,verbs=get;list

// legacyAgentVersionList is the list kind of the retired AgentVersion CRD. Helm keeps the CRD and its
// objects on upgrade (helm.sh/resource-policy: keep); a fresh install has neither.
var legacyAgentVersionList = schema.GroupVersionKind{
	Group: agentsv1beta1.GroupVersion.Group, Version: "v1beta1", Kind: "AgentVersionList",
}

// importPageSize bounds each list call of the import.
const importPageSize = 500

// ImportAgentVersions copies the AgentVersion objects of an upgraded cluster into the version store,
// oldest first, once per database. It must run before the controller records anything, or an
// imported version would be ordered after the version serving now. Objects whose owning
// AgentDeployment no longer exists (or was recreated) are skipped, as owner-reference garbage
// collection would have removed them. A cluster without the CRD imports nothing and is marked done.
func ImportAgentVersions(ctx context.Context, reader client.Reader, store agentversion.Store) (agentversion.ImportResult, error) {
	return store.ImportOnce(ctx, func(ctx context.Context) ([]agentversion.Version, int, error) {
		return loadAgentVersions(ctx, reader)
	})
}

func loadAgentVersions(ctx context.Context, reader client.Reader) ([]agentversion.Version, int, error) {
	log := logf.FromContext(ctx)
	var objs []unstructured.Unstructured
	continueToken := ""
	for {
		page := &unstructured.UnstructuredList{}
		page.SetGroupVersionKind(legacyAgentVersionList)
		opts := []client.ListOption{client.Limit(importPageSize)}
		if continueToken != "" {
			opts = append(opts, client.Continue(continueToken))
		}
		err := reader.List(ctx, page, opts...)
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil, 0, nil // never installed, or already deleted: nothing to import
		}
		if err != nil {
			return nil, 0, fmt.Errorf("listing AgentVersion objects: %w", err)
		}
		objs = append(objs, page.Items...)
		if continueToken = page.GetContinue(); continueToken == "" {
			break
		}
	}

	slices.SortFunc(objs, func(a, b unstructured.Unstructured) int {
		if c := a.GetCreationTimestamp().Compare(b.GetCreationTimestamp().Time); c != 0 {
			return c
		}
		if c := cmp.Compare(a.GetNamespace(), b.GetNamespace()); c != 0 {
			return c
		}
		return cmp.Compare(a.GetName(), b.GetName())
	})

	owners := map[client.ObjectKey]string{} // live AgentDeployment → its UID ("" when gone)
	var out []agentversion.Version
	skipped := 0
	for i := range objs {
		obj := &objs[i]
		ref, ok := controllerOwner(obj)
		if !ok {
			skipped++
			log.Info("Not importing an AgentVersion without an owning AgentDeployment",
				"namespace", obj.GetNamespace(), "name", obj.GetName())
			continue
		}
		key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: ref.name}
		liveUID, seen := owners[key]
		if !seen {
			var deploy agentsv1beta1.AgentDeployment
			err := reader.Get(ctx, key, &deploy)
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return nil, 0, fmt.Errorf("reading AgentDeployment %s: %w", key, err)
			default:
				liveUID = string(deploy.UID)
			}
			owners[key] = liveUID
		}
		if liveUID == "" || liveUID != ref.uid {
			skipped++
			log.Info("Not importing an AgentVersion whose AgentDeployment no longer exists",
				"namespace", obj.GetNamespace(), "name", obj.GetName(), "agent", ref.name)
			continue
		}
		snapshot, err := legacySnapshot(obj)
		if err != nil {
			skipped++
			log.Error(err, "Not importing an AgentVersion whose snapshot cannot be read",
				"namespace", obj.GetNamespace(), "name", obj.GetName())
			continue
		}
		out = append(out, agentversion.Version{
			Namespace: obj.GetNamespace(),
			Agent:     ref.name,
			AgentUID:  liveUID,
			Name:      obj.GetName(),
			Snapshot:  snapshot,
			CreatedAt: obj.GetCreationTimestamp().UTC(),
		})
	}
	return out, skipped, nil
}

// legacyOwner is the AgentDeployment a legacy AgentVersion was recorded for.
type legacyOwner struct{ name, uid string }

// controllerOwner returns the AgentDeployment controller reference of a legacy AgentVersion.
func controllerOwner(obj *unstructured.Unstructured) (legacyOwner, bool) {
	for _, ref := range obj.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != agentsv1beta1.GroupVersion.Group || ref.Kind != "AgentDeployment" {
			continue
		}
		if ref.Controller != nil && *ref.Controller {
			return legacyOwner{name: ref.Name, uid: string(ref.UID)}, true
		}
	}
	return legacyOwner{}, false
}

// legacySnapshot decodes spec.snapshot into today's spec type. Fields a later release removed are
// dropped, and the governance fields a version never carries are cleared (versionedSpec).
func legacySnapshot(obj *unstructured.Unstructured) (agentsv1beta1.AgentDeploymentSpec, error) {
	var spec agentsv1beta1.AgentDeploymentSpec
	raw, found, err := unstructured.NestedMap(obj.Object, "spec", "snapshot")
	if err != nil {
		return spec, fmt.Errorf("reading spec.snapshot: %w", err)
	}
	if !found {
		return spec, fmt.Errorf("spec.snapshot is missing")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return spec, fmt.Errorf("encoding spec.snapshot: %w", err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("decoding spec.snapshot: %w", err)
	}
	return versionedSpec(spec), nil
}
