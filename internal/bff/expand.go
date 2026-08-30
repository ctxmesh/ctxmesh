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

package bff

import (
	"context"

	"github.com/ctxmesh/agentry/internal/expand"
)

// coreExpandAdapter is the concrete ExpandAdapter (m12.6). It reuses the SAME
// internal/expand mapping the `agentry expand` CLI uses — the config-builder
// form → agent.yaml → CRD round-trip goes through exactly one mapping, so the
// server-side preview is byte-identical to the CLI (proven by the equivalence
// test in internal/expand). It holds no credentials and no cluster access: it is
// a pure transform, safe to call for an unauthenticated preview shape check —
// though the route sits behind the M11 auth like the rest of /api.
type coreExpandAdapter struct{}

// NewExpandAdapter returns the config-builder's server-side expand adapter.
func NewExpandAdapter() ExpandAdapter { return coreExpandAdapter{} }

// Expand renders a simplified agent.yaml into the CRD manifest set. A parse or
// validation failure is returned as an *expand.Error (Kind-tagged) so the
// handler maps it to 400 rather than swallowing it or reporting 500.
func (coreExpandAdapter) Expand(_ context.Context, agentYAML []byte) ([]byte, error) {
	return expand.Expand(agentYAML)
}
