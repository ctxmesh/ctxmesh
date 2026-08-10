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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOrgScopedFromLabels is the "survives the relabel" proof at the unit layer (ADR 0067 §4): the
// shared-credential directive must resolve across the entire dual-write window — a legacy scope-only
// row (pre-migration), a migrated credential-source-only row (post-migration, legacy scope dropped),
// AND a both-labels row (during the dual-write window). Non-shared servers must NOT resolve the
// shared credential (fail-closed to per-user consent).
func TestOrgScopedFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		// Shared-credential servers — MUST all resolve the shared credential.
		{
			name:   "legacy scope=org only (pre-migration)",
			labels: map[string]string{"mcp.ctxmesh.ai/scope": "org"},
			want:   true,
		},
		{
			name:   "migrated credential-source=shared only (legacy scope dropped)",
			labels: map[string]string{"mcp.ctxmesh.ai/credential-source": "shared"},
			want:   true,
		},
		{
			name: "dual-write window: both scope=org and credential-source=shared",
			labels: map[string]string{
				"mcp.ctxmesh.ai/scope":             "org",
				"mcp.ctxmesh.ai/credential-source": "shared",
			},
			want: true,
		},
		// Non-shared servers — MUST fall through to per-user consent.
		{
			name:   "legacy scope=personal",
			labels: map[string]string{"mcp.ctxmesh.ai/scope": "personal"},
			want:   false,
		},
		{
			name:   "migrated credential-source=byo-oauth",
			labels: map[string]string{"mcp.ctxmesh.ai/credential-source": "byo-oauth"},
			want:   false,
		},
		{
			name: "two-axis visibility=team, credential-source=none (no-auth server)",
			labels: map[string]string{
				"mcp.ctxmesh.ai/visibility":        "team",
				"mcp.ctxmesh.ai/credential-source": "none",
			},
			want: false,
		},
		{
			name:   "no labels at all (grandfathered — not org for resolution)",
			labels: nil,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, orgScopedFromLabels(tc.labels))
		})
	}
}
