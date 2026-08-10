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

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_MCPVisibilityBackfill is the m73.2 milestone gate (ADR 0067 §4): it proves the 0011
// backfill relabels legacy tool_registries rows to the two-axis MCP taxonomy, reach-preservingly,
// against REAL Postgres. Critically, a legacy `scope=org` server ends up `credential-source=shared`
// so the token-service still resolves the admin-set SHARED credential after the relabel (paired with
// the orgScopedFromLabels unit proof).
//
// Gates on CONTROLPLANE_TEST_DSN — the same pattern as the toolregistry/alertstore conformance runs;
// CI without a DB skips cleanly. Point it at a throwaway pg16.
func TestMigration_MCPVisibilityBackfill(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping the 0011 MCP-visibility backfill integration test (set it to a throwaway pg16 DB)")
	}

	ctx := context.Background()
	// OpenDB applies every migration (0001..0011); on the empty table the backfill no-ops. We then
	// TRUNCATE, seed LEGACY rows (no new labels), and re-run the 0011 backfill statement directly —
	// goose has already recorded 0011 as applied, so re-running Migrate would not touch the fresh
	// rows; the statement is idempotent, so running it directly is equivalent to the migration.
	db, err := OpenDB(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, `TRUNCATE tool_registries`)
	require.NoError(t, err)

	// Seed one legacy row per scope, each carrying ONLY the legacy scope label (the pre-m73 shape) —
	// plus a control row that already has the new labels to prove idempotence (untouched).
	seedLegacy(ctx, t, db, "legacy-org", `{"mcp.ctxmesh.ai/scope":"org"}`)
	seedLegacy(ctx, t, db, "legacy-personal", `{"mcp.ctxmesh.ai/scope":"personal"}`)
	seedLegacy(ctx, t, db, "legacy-public", `{"mcp.ctxmesh.ai/scope":"public"}`)
	seedLegacy(ctx, t, db, "legacy-absent", `{}`)
	// Already-migrated control: its (private, byo-oauth) must survive the backfill unchanged (idempotence).
	seedLegacy(ctx, t, db, "already-migrated",
		`{"mcp.ctxmesh.ai/scope":"org","mcp.ctxmesh.ai/visibility":"private","mcp.ctxmesh.ai/credential-source":"byo-oauth"}`)

	// Run the REAL 0011 Up backfill statement against the seeded legacy rows.
	_, err = db.ExecContext(ctx, migrationUpStatement(t, "0011_mcp_visibility_backfill.sql"))
	require.NoError(t, err)

	// Reach-preserving mappings (ADR 0067 §4) — MUST match internal/bff.mcpVisibility EXACTLY.
	assertLabels(ctx, t, db, "legacy-org", "team", "shared") // the shared-credential survival gate
	assertLabels(ctx, t, db, "legacy-personal", "private", "byo-oauth")
	assertLabels(ctx, t, db, "legacy-public", "team", "none")   // NOT the new all-tenants public
	assertLabels(ctx, t, db, "legacy-absent", "team", "shared") // grandfathered org
	// Idempotence: the already-migrated row keeps its stamped values (backfill skips it).
	assertLabels(ctx, t, db, "already-migrated", "private", "byo-oauth")

	// The legacy scope label is left intact (kept for one release as a rollback aid, ADR 0067 §4).
	got := readLabels(ctx, t, db, "legacy-org")
	assert.Equal(t, "org", got["mcp.ctxmesh.ai/scope"])
}

// seedLegacy inserts a tool_registries row with the given raw JSONB labels, bypassing the store so we
// control the exact legacy label shape.
func seedLegacy(ctx context.Context, t *testing.T, db *sql.DB, name, labelsJSON string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO tool_registries (namespace, name, labels) VALUES ($1, $2, $3::jsonb)`,
		"default", name, labelsJSON)
	require.NoError(t, err)
}

// assertLabels asserts the row's derived visibility + credential-source labels after the backfill.
func assertLabels(ctx context.Context, t *testing.T, db *sql.DB, name, wantVis, wantCred string) {
	t.Helper()
	got := readLabels(ctx, t, db, name)
	assert.Equal(t, wantVis, got["mcp.ctxmesh.ai/visibility"], "%s visibility", name)
	assert.Equal(t, wantCred, got["mcp.ctxmesh.ai/credential-source"], "%s credential-source", name)
}

// readLabels reads a row's labels JSONB back as a map.
func readLabels(ctx context.Context, t *testing.T, db *sql.DB, name string) map[string]string {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(ctx,
		`SELECT labels FROM tool_registries WHERE namespace = $1 AND name = $2`, "default", name).Scan(&raw)
	require.NoError(t, err)
	out := map[string]string{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// migrationUpStatement extracts the single SQL statement inside the Up section's
// `-- +goose StatementBegin` / `-- +goose StatementEnd` fence of the named embedded migration. It
// runs the REAL migration SQL (not a hand-copied duplicate), so the test can drift only if the file does.
func migrationUpStatement(t *testing.T, file string) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + file)
	require.NoError(t, err)
	body := string(raw)

	// Bound the search to the Up section (before `-- +goose Down`) so the Down statement is never picked.
	if i := strings.Index(body, "-- +goose Down"); i >= 0 {
		body = body[:i]
	}
	const begin, end = "-- +goose StatementBegin", "-- +goose StatementEnd"
	b := strings.Index(body, begin)
	require.GreaterOrEqual(t, b, 0, "no StatementBegin fence in %s Up section", file)
	stmt := body[b+len(begin):]
	e := strings.Index(stmt, end)
	require.GreaterOrEqual(t, e, 0, "no StatementEnd fence in %s Up section", file)
	return strings.TrimSpace(stmt[:e])
}
