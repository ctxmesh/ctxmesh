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

package v1alpha1

// PromptVersion has been RETIRED as a CRD (ADR 0044) — it lives only in Postgres, managed via the BFF API.
// The PromptVersion / PromptVersionSpec / PromptVersionStatus / PromptVersionList types + their scheme
// registration were removed. GitPromptSource stays: it's a plain (non-CRD) value struct still used by the
// prompt resolver (internal/prompt) and the AgentDeployment reconciler's prompt injection (prompt_inject.go).

// GitPromptSource locates a prompt artifact in a git repository.
// The ref must be an immutable pin (a tag or full SHA, not a branch name)
// so that a resolved prompt is fully reproducible.
type GitPromptSource struct {
	// repo is the URL of the git repository containing the prompt, e.g.
	// "https://github.com/example/prompts.git". Required.
	Repo string `json:"repo"`

	// ref is the immutable git ref that pins the prompt version — a full commit
	// SHA or an annotated tag. Branch names are discouraged because they are
	// mutable; the controller resolves ref → content at reconcile time and the
	// result must be reproducible. Required.
	Ref string `json:"ref"`

	// path is the path within the repository to the prompt file, e.g.
	// "prompts/my-agent/system.txt". Required.
	Path string `json:"path"`
}
