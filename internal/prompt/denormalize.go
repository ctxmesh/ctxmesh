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

package prompt

import agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"

// ResolvedPromptAnnotation is the AgentDeployment annotation the BFF stamps with the DENORMALIZED
// PromptVersion pointer (ADR 0042, m40.3 — compose-and-denormalize). It lets the AgentDeployment
// controller read the prompt's git pointer INLINE instead of fetching the PromptVersion (which is
// moving off a CRD into Postgres, so the controller can't read it from etcd). When the annotation is
// ABSENT (an agent created before m40.3, or a direct `kubectl apply`) the controller falls back to
// fetching the PromptVersion CRD — behaviour unchanged.
//
// KNOWN GAP (Fable, ADR 0042): the pointer is a snapshot taken at agent create/update. If the source
// PromptVersion's git.ref later changes, this annotation goes stale and the agent won't re-roll until a
// re-compose trigger re-stamps dependents — a task-carded follow-on that restores the prompt-only-deploy
// invariant (ADR 0008). Until then, editing a PromptVersion does not auto-roll dependent agents.
const ResolvedPromptAnnotation = "agents.ctxmesh.ai/resolved-prompt"

// ResolvedPointer is the JSON value of ResolvedPromptAnnotation: the PromptVersion's identity + the git
// pointer, captured when the agent was composed.
type ResolvedPointer struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

// GitSource returns the pointer as the resolver's GitPromptSource.
func (p ResolvedPointer) GitSource() agentsv1alpha1.GitPromptSource {
	return agentsv1alpha1.GitPromptSource{Repo: p.Repo, Ref: p.Ref, Path: p.Path}
}
