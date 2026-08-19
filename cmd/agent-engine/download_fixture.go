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

// download_fixture.go implements `agent-engine download-fixture <run-id>` — the DX convenience that
// pulls a recorded run's replay fixture blob(s) from the durable object store, merges the per-channel
// partials (the launcher gateway's MODEL blob + the egress sidecar's TOOLS blob) into one fixture,
// and writes it to a local JSON file you can hand straight to `dev --replay <file>`. It replaces the
// manual `mc cp fixtures/<run>/… && merge` dance (ADR 0071 §3a). The list/download/merge logic is
// FixtureStore.GetRun (internal/replay, unit-tested); this file is the CLI + file-writing wrapper.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
	"github.com/ctxmesh/agent-engine/internal/replay"
)

// newDownloadFixtureCmd builds the cobra `download-fixture` command.
func newDownloadFixtureCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "download-fixture <run-id>",
		Short: "Download + merge a recorded run's replay fixture from the object store into a local file",
		Long: `download-fixture pulls every partial replay fixture blob a run recorded — the
model channel (captured at the launcher gateway) and the tool channel (captured
at the egress sidecar) — from the durable object store, merges them into one
fixture, and writes it to a local JSON file you can replay directly:

  agent-engine download-fixture run-abc123
  agent-engine dev --replay run-abc123.fixture.json

It replaces the manual "mc cp fixtures/<run>/… then merge" dance. The object
store is read from the environment, exactly as the recorder writes it:

  OBJECT_STORE_ADDR        host:port of the durable object store (required)
  OBJECT_STORE_ACCESS_KEY  access key
  OBJECT_STORE_SECRET_KEY  secret key

A fixture is SENSITIVE-BY-DEFAULT (full prompts + tool results, ADR 0071 C4) —
the file is written 0600; treat it accordingly and do not commit it.

Examples:
  agent-engine download-fixture run-abc123
  agent-engine download-fixture run-abc123 -o /tmp/run.fixture.json

Exit codes: 0 = ok; 1 = validation error (no run id / store unconfigured);
2 = the run has no recorded fixture, or a fetch/merge/write error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runDownloadFixture(cmd.Context(), args[0], out, cmd.OutOrStdout()); err != nil {
				var xe *expandError
				if isExpandError(err, &xe) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error:", xe.err)
					os.Exit(xe.code)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "",
		"output fixture file (default: <run-id>.fixture.json in the current directory)")
	return cmd
}

// runDownloadFixture builds the durable object store from the environment (mirroring the recorder's
// NewMinioStore gate) and delegates to downloadFixtureToFile. An unset OBJECT_STORE_ADDR is a clear
// validation error rather than an empty fixture — the store is where the recording lives.
func runDownloadFixture(ctx context.Context, runID, outPath string, out io.Writer) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return validationErr("download-fixture requires a non-empty <run-id>")
	}
	store, err := objectstore.NewMinioStore()
	if err != nil {
		return parseErr("download-fixture: build object store: %v", err)
	}
	if store == nil {
		// Concrete-type nil check BEFORE the interface conversion (same gotcha-avoidance the
		// recorder uses): NewMinioStore returns a typed nil when OBJECT_STORE_ADDR is unset.
		return validationErr(
			"download-fixture: OBJECT_STORE_ADDR is unset — point it at the durable object store that holds " +
				"the run fixtures (also set OBJECT_STORE_ACCESS_KEY / OBJECT_STORE_SECRET_KEY)")
	}
	fs, err := replay.NewFixtureStore(store)
	if err != nil {
		return parseErr("download-fixture: %v", err)
	}
	return downloadFixtureToFile(ctx, fs, runID, outPath, out)
}

// downloadFixtureToFile fetches + merges a run's fixture via FixtureStore.GetRun and writes it to
// outPath (defaulting to <run-id>.fixture.json in the cwd). It is the injectable seam the unit test
// drives over a MemObjectStore-backed FixtureStore. GetRun already validates every blob (schema
// version + the C4 no-credential invariant) and returns an honest error for a run with no recording.
func downloadFixtureToFile(
	ctx context.Context, fs *replay.FixtureStore, runID, outPath string, out io.Writer,
) error {
	fx, err := fs.GetRun(ctx, runID)
	if err != nil {
		// No recording / fetch error — a file/parse-class failure (exit 2), not a usage error.
		return parseErr("download-fixture: %v", err)
	}
	if strings.TrimSpace(outPath) == "" {
		outPath = replay.SanitizeRunID(runID) + ".fixture.json"
	}
	data, err := fx.MarshalJSON()
	if err != nil {
		return parseErr("download-fixture: marshal merged fixture for run %q: %v", runID, err)
	}
	// Fixtures are sensitive-by-default (ADR 0071 C4) — 0600, never world-readable.
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return parseErr("download-fixture: write %q: %v", outPath, err)
	}

	statusf(out, "agent-engine download-fixture: run=%q agent=%q — %d model + %d tool interaction(s)\n",
		fx.RunID, fx.Agent, len(fx.Model), len(fx.Tools))
	statusf(out, "  wrote %s (0600 — sensitive; do not commit)\n", outPath)
	statusf(out, "  replay it: agent-engine dev --replay %s\n", outPath)
	return nil
}
