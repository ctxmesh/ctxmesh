//go:build integration

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

package toolregistry

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// TestPgStore_ListCatalog_RealPostgres exercises the pgstore ListCatalog SQL path against a live
// Postgres — the real JSONB `labels->>` predicate + `namespace = ANY(COALESCE($1::text[], '{}'))`
// array binding — which the memstore twin and the BFF handler tests cannot cover. m73.2 and m73.3
// both shipped array-SQL bugs (an `<> ALL(NULL)` empty-set trap) that ONLY a real-Postgres run
// caught, so this path is integration-tested here rather than trusted to the Go twin.
//
// Gated on CONTROLPLANE_TEST_DSN (skips when unset). OpenDB runs the goose migrations; the table is
// truncated first for isolation. Requires the pgvector extension (migration 0003) — start
// pgvector/pgvector:pg16, not plain postgres:16.
func TestPgStore_ListCatalog_RealPostgres(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping the real-Postgres ListCatalog SQL-path test")
	}
	ctx := context.Background()
	db, err := controlplane.OpenDB(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE tool_registries`)
	require.NoError(t, err)

	s := NewPostgresStore(db)

	const (
		callerNS  = "ns-caller"  // the caller's own namespace
		memberNS  = "ns-member"  // a sibling namespace in the same tenant as callerNS
		outsideNS = "ns-outside" // a namespace in NO shared tenant with the caller
	)
	// members = the caller's tenant membership set (callerNS + its sibling). outsideNS is deliberately
	// excluded so the org-clause NON-member branch is exercised.
	members := []string{callerNS, memberNS}

	// mcp seeds a managed-by=ctxmesh-mcp catalog row with the given visibility.
	mcp := func(ns, name, visibility string) {
		t.Helper()
		_, err := s.Upsert(ctx, ToolRegistry{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				labelManagedByKey:  labelManagedByMCP,
				labelVisibilityKey: visibility,
			},
		})
		require.NoError(t, err)
	}

	// One row per branch of the WHERE.
	mcp(memberNS, "org-in-member", visOrg)         // org in a member ns          → RETURNED
	mcp(outsideNS, "org-in-outside", visOrg)       // org in a NON-member ns       → NOT returned
	mcp(outsideNS, "public-in-outside", visPublic) // public anywhere           → RETURNED
	mcp(callerNS, "private-in-own", visPrivate)    // private in the caller's ns   → NEVER returned
	mcp(callerNS, "team-in-own", visTeam)          // team in the caller's own ns  → RETURNED
	mcp(memberNS, "team-in-member", visTeam)       // team in a sibling ns         → NOT returned (team is own-ns only)

	// A non-MCP-managed row that would otherwise match on visibility must be excluded by the
	// managed-by filter.
	_, err = s.Upsert(ctx, ToolRegistry{
		Namespace: outsideNS,
		Name:      "not-managed-by-mcp",
		Labels:    map[string]string{labelVisibilityKey: visPublic},
	})
	require.NoError(t, err)

	// names extracts the (unique-enough) names from a catalog result for set comparison.
	names := func(rows []ToolRegistry) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}

	t.Run("full member set covers every branch", func(t *testing.T) {
		rows, err := s.ListCatalog(ctx, callerNS, members)
		require.NoError(t, err)
		// Expected, ordered by (namespace, name): ns-caller/team-in-own, ns-member/org-in-member,
		// ns-outside/public-in-outside.
		assert.Equal(t, []string{"team-in-own", "org-in-member", "public-in-outside"}, names(rows),
			"only org-in-member, public-in-outside, and own-ns team-in-own may appear")

		got := names(rows)
		assert.NotContains(t, got, "org-in-outside", "org in a NON-member ns must not leak")
		assert.NotContains(t, got, "private-in-own", "private must NEVER be returned, even in the caller's own ns")
		assert.NotContains(t, got, "team-in-member", "team in a sibling ns must not leak (team is own-ns only)")
		assert.NotContains(t, got, "not-managed-by-mcp", "non-MCP-managed rows must be excluded")
	})

	t.Run("empty member set is the COALESCE nil-array path", func(t *testing.T) {
		// The m73.3-class trap: a nil members slice binds as SQL NULL; without COALESCE the org clause
		// (`namespace = ANY(NULL)`) is NULL for every row and the whole OR could misbehave. Assert it
		// does not error and returns exactly the public + own-ns team rows (no org rows — none are in
		// an empty member set).
		rows, err := s.ListCatalog(ctx, callerNS, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"team-in-own", "public-in-outside"}, names(rows),
			"empty members → only public (anywhere) + own-ns team; NO org rows and NO error")

		got := names(rows)
		assert.NotContains(t, got, "org-in-member", "org rows require a non-empty member set")
		assert.NotContains(t, got, "org-in-outside")
		assert.NotContains(t, got, "private-in-own")
	})
}
