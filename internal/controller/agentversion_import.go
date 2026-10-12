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
	"encoding/json"
	"fmt"

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

// kindAgentDeployment is the kind an AgentVersion's controller owner reference names.
const kindAgentDeployment = "AgentDeployment"

// importPageSize bounds each list call of the import, and so the objects held in memory at once.
const importPageSize = 250

// ImportAgentVersions copies the AgentVersion objects of an upgraded cluster into the version store,
// oldest first, once per database. It must run before the controller records anything, or an
// imported version would be ordered after the version serving now. Objects whose owning
// AgentDeployment no longer exists (or was recreated) are skipped, as owner-reference garbage
// collection would have removed them. A cluster without the CRD imports nothing and is marked done.
//
// The objects are streamed a page at a time into the store, which orders them: an install that
// accumulated many versions must not hold them all in the controller's memory before it starts.
func ImportAgentVersions(ctx context.Context, reader client.Reader, store agentversion.Store) (agentversion.ImportResult, error) {
	return store.ImportOnce(ctx, func(ctx context.Context, emit func(agentversion.Version) error) (int, error) {
		return streamAgentVersions(ctx, reader, emit)
	})
}

func streamAgentVersions(ctx context.Context, reader client.Reader, emit func(agentversion.Version) error) (int, error) {
	owners := map[client.ObjectKey]string{} // live AgentDeployment → its UID ("" when gone)
	skipped := 0
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
			return 0, nil // never installed, or already deleted: nothing to import
		}
		if err != nil {
			return 0, fmt.Errorf("listing AgentVersion objects: %w", err)
		}
		for i := range page.Items {
			v, ok, err := importedVersion(ctx, reader, owners, &page.Items[i])
			if err != nil {
				return 0, err
			}
			if !ok {
				skipped++
				continue
			}
			if err := emit(v); err != nil {
				return 0, err
			}
		}
		if continueToken = page.GetContinue(); continueToken == "" {
			return skipped, nil
		}
	}
}

// importedVersion converts one legacy object. ok is false for an object that is not imported (no
// owner, an owner that is gone or was recreated, an unreadable snapshot); err is an API failure.
func importedVersion(
	ctx context.Context, reader client.Reader, owners map[client.ObjectKey]string, obj *unstructured.Unstructured,
) (agentversion.Version, bool, error) {
	log := logf.FromContext(ctx)
	ref, ok := controllerOwner(obj)
	if !ok {
		log.Info("Not importing an AgentVersion without an owning AgentDeployment",
			"namespace", obj.GetNamespace(), "name", obj.GetName())
		return agentversion.Version{}, false, nil
	}
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: ref.name}
	liveUID, seen := owners[key]
	if !seen {
		var deploy agentsv1beta1.AgentDeployment
		err := reader.Get(ctx, key, &deploy)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return agentversion.Version{}, false, fmt.Errorf("reading AgentDeployment %s: %w", key, err)
		default:
			liveUID = string(deploy.UID)
		}
		owners[key] = liveUID
	}
	if liveUID == "" || liveUID != ref.uid {
		log.Info("Not importing an AgentVersion whose AgentDeployment no longer exists",
			"namespace", obj.GetNamespace(), "name", obj.GetName(), "agent", ref.name)
		return agentversion.Version{}, false, nil
	}
	snapshot, err := legacySnapshot(obj)
	if err != nil {
		log.Error(err, "Not importing an AgentVersion whose snapshot cannot be read",
			"namespace", obj.GetNamespace(), "name", obj.GetName())
		return agentversion.Version{}, false, nil
	}
	return agentversion.Version{
		Namespace: obj.GetNamespace(),
		Agent:     ref.name,
		AgentUID:  liveUID,
		Name:      obj.GetName(),
		Snapshot:  snapshot,
		CreatedAt: obj.GetCreationTimestamp().UTC(),
	}, true, nil
}

// legacyOwner is the AgentDeployment a legacy AgentVersion was recorded for.
type legacyOwner struct{ name, uid string }

// controllerOwner returns the AgentDeployment controller reference of a legacy AgentVersion.
func controllerOwner(obj *unstructured.Unstructured) (legacyOwner, bool) {
	for _, ref := range obj.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != agentsv1beta1.GroupVersion.Group || ref.Kind != kindAgentDeployment {
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
