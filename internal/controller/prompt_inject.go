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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/prompt"
)

const (
	// promptConfigMapSuffix names the per-agent ConfigMap that materialises the
	// resolved prompt (<agent>-prompt). The controller resolves the PromptVersion
	// git pointer, writes the content here, and mounts it read-only into the user
	// container. git remains the source of truth; this is a per-revision cache.
	promptConfigMapSuffix = "-prompt"

	// promptConfigMapKey is the data key inside the <agent>-prompt ConfigMap.
	promptConfigMapKey = "prompt.txt"

	// promptMountPath is where the resolved prompt is mounted in the user
	// container. The launcher reads PROMPT_FILE (this path + key) for the content
	// and surfaces PROMPT_VERSION as the prompt.version trace attribute.
	promptMountPath = "/etc/agent/prompt"

	// promptVolumeName is the pod volume name for the mounted prompt ConfigMap.
	promptVolumeName = "agent-prompt"

	// envPromptFile is the in-container path to the resolved prompt file. Static
	// env (no valueFrom — the Knative ksvc landmine, m5.7).
	envPromptFile = "PROMPT_FILE"

	// envPromptVersion is the deterministic, display-only prompt identifier the
	// launcher stamps as the prompt.version span attribute. Static env.
	envPromptVersion = "PROMPT_VERSION"
)

// promptConfigMapName returns the CONTENT-ADDRESSED prompt ConfigMap name
// (<agent>-prompt-<digest8>, audit FUNC-3). Content-addressing keeps each revision's
// prompt in its OWN immutable CM: a new/blocked candidate's prompt lands in a NEW CM and
// never overwrites the one the currently-serving revision mounts (the old per-agent
// <agent>-prompt was mutated in place before the eval-gate decision, leaking a blocked
// candidate's prompt into the live old revision).
func promptConfigMapName(agentName, digest string) string {
	return agentName + promptConfigMapSuffix + "-" + digest
}

// promptAgentLabel groups an agent's content-addressed prompt ConfigMaps so superseded
// ones can be listed + GC'd (audit FUNC-3).
const promptAgentLabel = "agents.ctxmesh.ai/prompt-agent"

// resolvedPrompt is the controller-side result of resolving an agent's promptRef:
// the resolved content + version and the derived digest component. hasPrompt is
// false when the agent has no promptRef (the image-bundled prompt is used and the
// deploy is byte-compatible with the pre-M9 path).
type resolvedPrompt struct {
	hasPrompt bool
	content   string
	version   string
	// digest is the prompt COMPONENT of combinedBindingDigest (p=<digest8>): 8 hex
	// chars over the pointer + resolved version, "" when hasPrompt is false. It
	// rolls the Knative revision on a prompt swap WITHOUT touching the image.
	digest string
}

// promptResolveError wraps a user-facing prompt-resolution failure (missing
// PromptVersion, bad git ref/path) so the caller sets Ready=False and STOPS
// cleanly — the old revision keeps serving, no half-applied prompt swap, no noisy
// requeue on user input. Non-promptResolveError errors from resolvePrompt are
// genuine infra failures (API read errors) and requeue normally.
type promptResolveError struct {
	reason string
	msg    string
}

func (e *promptResolveError) Error() string { return e.msg }

// asPromptResolveError extracts a *promptResolveError from an error chain.
func asPromptResolveError(err error) (*promptResolveError, bool) {
	var pe *promptResolveError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// promptResolver returns the reconciler's Resolver, defaulting to the offline
// fixture-backed resolver when unset. Production wires a real (e.g. go-git)
// Resolver at the construction site; dev / envtest / e2e use the fixture, which
// resolves deterministically with no network (ADR 0004, mock-first).
func (r *AgentDeploymentReconciler) promptResolver() prompt.Resolver {
	if r.PromptResolver != nil {
		return r.PromptResolver
	}
	return prompt.NewFixtureResolver()
}

// resolvePrompt resolves the agent's spec.promptRef (if any) into prompt content,
// a display version, and the digest component. It returns:
//
//   - hasPrompt=false, no error         → no promptRef (image-bundled prompt path).
//   - a *promptResolveError             → user error (missing PromptVersion / bad
//     git ref/path); the caller sets Ready=False and keeps the old revision.
//   - any other error                   → an infra read failure (requeue).
//
// The digest folds the pointer AND the resolved version so a promptRef swap OR a
// PromptVersion.spec.git.ref swap both roll the revision — the mechanism the
// prompt-only-deploy invariant relies on. The image is never touched here.
func (r *AgentDeploymentReconciler) resolvePrompt(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (resolvedPrompt, error) {
	if deploy.Spec.PromptRef == "" {
		return resolvedPrompt{}, nil
	}

	src, name, perr := r.promptPointer(ctx, deploy)
	if perr != nil {
		return resolvedPrompt{}, perr
	}

	res, err := r.promptResolver().Resolve(ctx, src)
	if errors.Is(err, prompt.ErrNotFound) {
		// A bad git ref / missing path is USER input, not an infra failure: surface
		// it on status and keep the old revision serving (no half-applied swap).
		return resolvedPrompt{}, &promptResolveError{
			reason: "PromptUnresolvable",
			msg: fmt.Sprintf("PromptVersion %q git pointer does not resolve (repo=%q ref=%q path=%q): %v",
				name, src.Repo, src.Ref, src.Path, err),
		}
	}
	if err != nil {
		return resolvedPrompt{}, fmt.Errorf("resolving PromptVersion %q: %w", name, err)
	}

	return resolvedPrompt{
		hasPrompt: true,
		content:   res.Content,
		version:   res.Version,
		digest:    promptDigest(src, res.Version),
	}, nil
}

// promptPointer returns the agent's prompt git pointer + a display name from the DENORMALIZED annotation
// the BFF stamps at create/update (ADR 0042, m40.3 — compose-and-denormalize). PromptVersion is retired to
// Postgres (ADR 0044) — there is no CRD to read — so the annotation is the ONLY source: a stamped agent
// reconciles self-contained. An agent without the stamp (e.g. a raw kubectl apply) surfaces a user-facing
// status error rather than reading a store the controller intentionally doesn't depend on.
func (r *AgentDeploymentReconciler) promptPointer(
	_ context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (agentsv1alpha1.GitPromptSource, string, error) {
	raw := deploy.Annotations[prompt.ResolvedPromptAnnotation]
	if raw == "" {
		return agentsv1alpha1.GitPromptSource{}, "", &promptResolveError{
			reason: "PromptPointerMissing",
			msg: fmt.Sprintf("agent %q has no resolved prompt pointer (annotation %s) for promptRef %q — create or edit the agent via the API so its PromptVersion pointer is resolved (PromptVersion is no longer a CRD, ADR 0044)",
				deploy.Name, prompt.ResolvedPromptAnnotation, deploy.Spec.PromptRef),
		}
	}
	var p prompt.ResolvedPointer
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		// A corrupt stamp is a platform error, not user input — surface it on status.
		return agentsv1alpha1.GitPromptSource{}, "", &promptResolveError{
			reason: "PromptPointerInvalid",
			msg:    fmt.Sprintf("the %s annotation on agent %q is not valid JSON: %v", prompt.ResolvedPromptAnnotation, deploy.Name, err),
		}
	}
	return p.GitSource(), p.Name, nil
}

// promptDigest returns the prompt COMPONENT of combinedBindingDigest: 8 hex chars
// over the git pointer + resolved version. It folds into the revision-name digest
// like the M8 budget component (g=<w>), so a prompt swap rolls a NEW Knative
// revision (the new prompt takes effect on a clean rollout) while the container
// image — which lives in spec.Image and is never touched by the prompt path —
// keeps an UNCHANGED digest. Returns "" for the empty inputs (no prompt),
// symmetric with the tool/memory/registry/budget components.
func promptDigest(src agentsv1alpha1.GitPromptSource, version string) string {
	if version == "" {
		return ""
	}
	payload := fmt.Sprintf("repo=%s;ref=%s;path=%s;ver=%s", src.Repo, src.Ref, src.Path, version)
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h[:])[:8]
}

// reconcilePromptConfigMap materialises the resolved prompt into the per-agent
// <agent>-prompt ConfigMap (owner-ref'd so it GCs with the AgentDeployment) and
// returns the volume + mount + static env the user container needs. It is a
// no-op returning zero values when the agent has no prompt.
//
// The prompt is delivered via a mounted ConfigMap (like the collector config),
// NOT env, because prompt content can exceed the per-var env limit and a mounted
// file is the natural launcher read. The env carries only the FILE PATH and the
// display VERSION — both static (no valueFrom; the m5.7 Knative ksvc landmine).
func (r *AgentDeploymentReconciler) reconcilePromptConfigMap(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	rp resolvedPrompt,
) (vol *corev1.Volume, mount *corev1.VolumeMount, env []corev1.EnvVar, err error) {
	if !rp.hasPrompt {
		return nil, nil, nil, nil
	}

	cmName := promptConfigMapName(deploy.Name, rp.digest)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err = ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[promptConfigMapKey] = rp.content
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[promptAgentLabel] = deploy.Name
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("upserting prompt ConfigMap: %w", err)
	}

	// GC superseded prompt CMs for this agent: content-addressing means each prompt swap
	// leaves the old CM behind (owner-ref'd, so it only GCs when the AgentDeployment is
	// deleted). Prune the ones this agent no longer references — SAFELY: keep the current
	// CM and any still mounted by a live ksvc Revision (a blocked candidate / a rollback
	// target), so pruning never breaks a servable revision.
	if err = r.pruneOldPromptConfigMaps(ctx, deploy, cmName); err != nil {
		return nil, nil, nil, err
	}

	v := corev1.Volume{
		Name: promptVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			},
		},
	}
	m := corev1.VolumeMount{Name: promptVolumeName, MountPath: promptMountPath, ReadOnly: true}
	e := []corev1.EnvVar{
		{Name: envPromptFile, Value: promptMountPath + "/" + promptConfigMapKey},
		{Name: envPromptVersion, Value: rp.version},
	}
	return &v, &m, e, nil
}

// pruneOldPromptConfigMaps deletes this agent's content-addressed prompt ConfigMaps that
// are neither the current one nor mounted by a live ksvc Revision (audit FUNC-3). Keeping
// in-use CMs means pruning can never break a servable revision (a held candidate or a
// rollback target). Best-effort within the reconcile: a prune failure surfaces so the
// reconcile requeues, but the correctness (no prompt overwrite) is the content-addressing,
// not the GC.
func (r *AgentDeploymentReconciler) pruneOldPromptConfigMaps(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment, currentCM string) error {
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{promptAgentLabel: deploy.Name}); err != nil {
		return fmt.Errorf("listing prompt ConfigMaps for GC: %w", err)
	}
	if len(cms.Items) <= 1 {
		return nil // only the current CM (or none) — nothing to prune
	}
	inUse, err := r.promptConfigMapsInUse(ctx, deploy)
	if err != nil {
		return err
	}
	inUse[currentCM] = true
	for i := range cms.Items {
		cm := &cms.Items[i]
		if inUse[cm.Name] {
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pruning superseded prompt ConfigMap %s: %w", cm.Name, err)
		}
	}
	return nil
}

// promptConfigMapsInUse returns the set of prompt-ConfigMap names still mounted by a live
// Revision of the agent's ksvc, so they are never pruned.
func (r *AgentDeploymentReconciler) promptConfigMapsInUse(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment) (map[string]bool, error) {
	inUse := map[string]bool{}
	var revs servingv1.RevisionList
	if err := r.List(ctx, &revs,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{knativeServiceLabel: deploy.Name}); err != nil {
		return nil, fmt.Errorf("listing Revisions for prompt GC: %w", err)
	}
	for i := range revs.Items {
		for _, v := range revs.Items[i].Spec.Volumes {
			if v.Name == promptVolumeName && v.ConfigMap != nil {
				inUse[v.ConfigMap.Name] = true
			}
		}
	}
	return inUse, nil
}
