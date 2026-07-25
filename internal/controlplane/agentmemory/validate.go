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

package agentmemory

import (
	"fmt"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// validate enforces the ADR 0045 invariants before a memory is stored: a valid partition (scope enum +
// the subject/scope pairing that guarantees per-user isolation) and self-consistent embedding provenance
// (a non-empty model, a positive dimension, and a vector whose length matches that dimension — a mismatch
// would make every later cosine search silently wrong).
func validate(m AgentMemory) error {
	if strings.TrimSpace(m.Namespace) == "" {
		return fmt.Errorf("%w: namespace is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(m.AgentName) == "" {
		return fmt.Errorf("%w: agentName is required", controlplane.ErrInvalid)
	}
	switch m.Scope {
	case ScopeAgent:
		if m.Subject != "" {
			return fmt.Errorf("%w: scope %q must have an empty subject (agent-wide)", controlplane.ErrInvalid, m.Scope)
		}
	case ScopeAgentUser:
		if strings.TrimSpace(m.Subject) == "" {
			return fmt.Errorf("%w: scope %q requires a subject (the user id)", controlplane.ErrInvalid, m.Scope)
		}
	default:
		return fmt.Errorf("%w: scope %q is not one of %q/%q", controlplane.ErrInvalid, m.Scope, ScopeAgent, ScopeAgentUser)
	}
	if strings.TrimSpace(m.Content) == "" {
		return fmt.Errorf("%w: content is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(m.EmbeddingModel) == "" {
		return fmt.Errorf("%w: embeddingModel is required (provenance)", controlplane.ErrInvalid)
	}
	if m.EmbeddingDim <= 0 {
		return fmt.Errorf("%w: embeddingDim must be positive", controlplane.ErrInvalid)
	}
	if len(m.Embedding) != m.EmbeddingDim {
		return fmt.Errorf("%w: embedding length %d does not match embeddingDim %d",
			controlplane.ErrInvalid, len(m.Embedding), m.EmbeddingDim)
	}
	return nil
}
