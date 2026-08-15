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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// kbRosterEntry is the per-KB wire shape stamped into KNOWLEDGE_BASES and read by the launcher
// knowledgeProxy roster gate (ADR 0061 Fork 3). Matches the JSON the launcher parses in
// parseKnowledgeRoster (cmd/launcher/knowledge.go).
type kbRosterEntry struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	EmbeddingRoute string `json:"embeddingRoute"`
	// PerUser marks a corpus whose retrieval must be scoped to the invoking user's subject hash
	// (ADR 0061 Fork 3). omitempty so an org-wide KB serialises byte-identically to the pre-m80.4
	// roster — no structural-digest churn / fleet-wide roll on upgrade; only a pod referencing a
	// per-user KB carries the flag (and its behaviour genuinely changed, so a roll there is correct).
	PerUser bool `json:"perUser,omitempty"`
}

// kbResolveResult captures the outcome of resolving spec.knowledgeBases[].
type kbResolveResult struct {
	// env are the KNOWLEDGE_BASE_ENABLED + KNOWLEDGE_BASES vars to append to the agent container env.
	// Empty when no KBs are specified or none resolved.
	env []corev1.EnvVar
	// roster is the resolved entries, kept for the combinedBindingDigest.
	roster []kbRosterEntry
	// danglingNames lists the "<namespace>/<name>" refs that could not be resolved (NotFound).
	danglingNames []string
}

// resolveKnowledgeBases resolves spec.knowledgeBases[] → launcher env vars and a digest roster.
//
// For each ref the controller fetches the KnowledgeBase CR (defaulting namespace to the agent's
// namespace when unset) and extracts spec.embeddingRoute. The result env contains:
//
//	KNOWLEDGE_BASE_ENABLED=true    — activates the launcher knowledgeProxy (m68.7).
//	KNOWLEDGE_BASES=<JSON roster>  — [{name, namespace, embeddingRoute}] (the un-forgeable gate).
//
// Dangling (unresolvable) refs are SKIPPED and their names are returned in result.danglingNames —
// the caller surfaces a condition and continues reconciling with the remaining resolvable entries.
// Knowledge is an ADDITIVE capability (not fail-closed like guardrails), so a missing KB is a
// condition warning, not a hard reconcile error.
//
// Returns a zero result + nil error when spec.knowledgeBases is empty (proxy stays off; byte-
// compatible with pre-M68 behaviour). Returns a real error only for unexpected API failures
// (not NotFound — those become danglingNames).
func resolveKnowledgeBases(
	ctx context.Context,
	c client.Client,
	deploy *agentsv1alpha1.AgentDeployment,
) (kbResolveResult, error) {
	if len(deploy.Spec.KnowledgeBases) == 0 {
		return kbResolveResult{}, nil
	}

	var res kbResolveResult
	for _, ref := range deploy.Spec.KnowledgeBases {
		ns := ref.Namespace
		if ns == "" {
			ns = deploy.Namespace
		}
		var kb agentsv1beta1.KnowledgeBase
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &kb); err != nil {
			if apierrors.IsNotFound(err) {
				res.danglingNames = append(res.danglingNames, fmt.Sprintf("%s/%s", ns, ref.Name))
				continue
			}
			return kbResolveResult{}, fmt.Errorf("fetching KnowledgeBase %s/%s: %w", ns, ref.Name, err)
		}
		res.roster = append(res.roster, kbRosterEntry{
			Name:           ref.Name,
			Namespace:      ns,
			EmbeddingRoute: kb.Spec.EmbeddingRoute,
			PerUser:        kb.Spec.PerUser,
		})
	}

	// No resolvable KBs at all (all dangling) → do not inject the feature gate so the proxy stays off.
	// The caller will surface the dangling condition; the agent reconciles without KB support.
	if len(res.roster) == 0 {
		return res, nil
	}

	rosterJSON, err := json.Marshal(res.roster)
	if err != nil {
		return kbResolveResult{}, fmt.Errorf("marshalling KNOWLEDGE_BASES roster: %w", err)
	}
	res.env = []corev1.EnvVar{
		{Name: "KNOWLEDGE_BASE_ENABLED", Value: gatewaySyncValue},
		{Name: "KNOWLEDGE_BASES", Value: string(rosterJSON)},
	}
	return res, nil
}

// knowledgeBasesDigest returns a short hash of the resolved KB roster so that adding, removing, or
// changing a KnowledgeBase ref (or its embeddingRoute) rolls a new Knative revision — the same
// structural-change discipline used by the long-term-memory digest (memDigest) and guardrailDig.
// Returns "" when knowledgeBases is empty (byte-compatible with pre-M68).
func knowledgeBasesDigest(kbs []kbRosterEntry) string {
	if len(kbs) == 0 {
		return ""
	}
	b, err := json.Marshal(kbs)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:])[:8]
}
