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

// dev_replay.go implements the `dev --replay` lifecycle (ADR 0071 §3a): bring the stack up with
// the gateway swapped for the replay-serve mock, verify image/CLI parity, run the agent to
// completion, GET /replay/report, render it, and map the verdict to the exit code (0 pass / 1
// agent-error / 2 structural). The load-bearing replay logic lives in internal/replay (server.go,
// unit-tested docker-free); this file is the docker-compose lifecycle wrapper.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ctxmesh/agentry/internal/replay"
)

// fixtureToolNames returns the deduped, order-stable tool names recorded in a fixture — what the
// discovery manifest advertises in replay mode so the agent's tool calls resolve to the replay
// mock's /mcp channel.
func fixtureToolNames(f *replay.Fixture) []string {
	seen := map[string]bool{}
	var names []string
	for _, t := range f.Tools {
		if t.ToolName == "" || seen[t.ToolName] {
			continue
		}
		seen[t.ToolName] = true
		names = append(names, t.ToolName)
	}
	return names
}

// stageReplayFixture copies the operator's fixture (a single JSON file OR a directory of *.json
// blobs) into the compose work dir as ./fixture, matching the relative bind-mount the compose
// render uses. Copying (rather than mounting an absolute host path) keeps the compose file
// relocatable + mirrors how writePlanAssets stages the other assets. The staged copy is read-only
// to the container.
func stageReplayFixture(src, workDir string) error {
	dst := filepath.Join(workDir, "fixture")
	info, err := os.Stat(src)
	if err != nil {
		return parseErr("staging fixture: stat %q: %v", src, err)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return fmt.Errorf("staging fixture dir: %w", err)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("staging fixture dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(src, dst)
}

// copyFile copies src → dst with 0600 perms (fixtures are sensitive-by-default, ADR 0071 C4).
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator-supplied local fixture path (a CLI arg)
	if err != nil {
		return fmt.Errorf("staging fixture: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("staging fixture: create %q: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("staging fixture: copy: %w", err)
	}
	return nil
}

// runReplay is the replay-mode lifecycle. It brings the stack up detached, verifies the
// replay-serve image matches the CLI, waits for the agent container to exit, fetches + renders the
// divergence report, tears down, and returns a replayExitError carrying the mapped exit code so
// the command wrapper can os.Exit with it (ADR 0071 §3a).
func runReplay(
	ctx context.Context, fixture *replay.Fixture, workDir string, out, errOut io.Writer,
) error {
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	statusf(out, "agentry dev --replay: run=%q agent=%q — %d model + %d tool interaction(s)\n",
		fixture.RunID, fixture.Agent, len(fixture.Model), len(fixture.Tools))
	statusf(out, "  gateway swapped for %s (both channels served from the fixture)\n", replayImageRef())

	teardown := func() {
		statusf(out, "\nagentry dev --replay: tearing down…\n")
		down := composeCommand(context.Background(), workDir, nil, "down", "--volumes", "--remove-orphans")
		down.Stdout, down.Stderr = errOut, errOut
		if derr := down.Run(); derr != nil {
			statusf(errOut, "agentry dev --replay: teardown warning: %v\n", derr)
		}
	}
	defer teardown()

	up := composeCommand(runCtx, workDir, nil, "up", "-d")
	up.Stdout, up.Stderr = out, errOut
	if err := up.Run(); err != nil {
		return fmt.Errorf("docker compose up failed: %w", err)
	}

	// Fail fast + clear if the running replay image does not match the CLI driving it
	// (a stale image would mis-replay). Parity is the CLI version echoed at /replay/version.
	if err := verifyReplayParity(runCtx, out, errOut); err != nil {
		return err
	}

	// Block until the agent container stops; `docker compose wait` returns its exit code.
	agentExit, waitErr := waitForAgentExit(runCtx, workDir)
	if waitErr != nil {
		return waitErr
	}

	rep, err := fetchReplayReport(runCtx)
	if err != nil {
		return err
	}

	code := replay.RenderReport(out, rep, fixture, agentExit != 0)
	return &replayExitError{code: code}
}

// replayExitError carries the replay verdict's process exit code out of runReplay so the command
// layer can os.Exit(code) — 0/1/2 per ADR 0071 §3a. It is NOT a validation/parse expandError; the
// dev command maps it separately.
type replayExitError struct{ code int }

func (e *replayExitError) Error() string {
	return fmt.Sprintf("replay finished with exit code %d", e.code)
}

// isReplayExitError type-asserts err to a *replayExitError.
func isReplayExitError(err error) (*replayExitError, bool) {
	re, ok := err.(*replayExitError) //nolint:errorlint // sentinel wrapper, not wrapped
	return re, ok
}

// replayReportBaseURL is the host URL the CLI reaches the swapped gateway (replay-serve) at, via
// the port it publishes in replay mode.
func replayReportBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", replayReportHostPort)
}

// verifyReplayParity polls /replay/version until the replay-serve container answers, then asserts
// its reported CLI version matches this CLI (devVersion). A mismatch is a hard, clear error (a
// stale replay image would silently mis-replay, ADR 0071 §3a).
func verifyReplayParity(ctx context.Context, out, errOut io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(devReadyTimeout)
	url := replayReportBaseURL() + "/replay/version"
	statusf(out, "agentry dev --replay: waiting for the replay server…\n")

	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			var vi replay.VersionInfo
			derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&vi)
			_ = resp.Body.Close()
			if derr != nil {
				lastErr = derr
			} else if vi.CLIVersion != devVersion {
				return fmt.Errorf(
					"replay image/CLI version mismatch: the replay-serve container reports %q but this CLI is %q"+
						" — rebuild the replay image (make docker-build-replay) or use a matching CLI",
					vi.CLIVersion, devVersion)
			} else {
				statusf(out, "agentry dev --replay: replay server ready (version %s, schema %d)\n",
					vi.CLIVersion, vi.SchemaVersion)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	statusf(errOut, "agentry dev --replay: replay server did not become ready in %s\n", devReadyTimeout)
	return fmt.Errorf("replay server readiness failed: last error: %v", lastErr)
}

// waitForAgentExit blocks until the agent service container stops, returning its exit code. It
// shells out to `docker compose wait agent`, which prints the exit code on stdout.
func waitForAgentExit(ctx context.Context, workDir string) (int, error) {
	wait := composeCommand(ctx, workDir, nil, "wait", "agent")
	stdout, err := wait.Output()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("waiting for the agent to finish (docker compose wait agent): %w", err)
	}
	code, perr := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if perr != nil {
		// A non-numeric line is unexpected; treat as an infra error rather than guessing.
		return 0, fmt.Errorf("could not parse the agent exit code %q: %w", strings.TrimSpace(string(stdout)), perr)
	}
	return code, nil
}

// fetchReplayReport GETs /replay/report from the swapped gateway and decodes the verdict.
func fetchReplayReport(ctx context.Context) (replay.Report, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := replayReportBaseURL() + "/replay/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return replay.Report{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return replay.Report{}, fmt.Errorf("fetching the replay report from %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return replay.Report{}, fmt.Errorf("replay report endpoint returned %d", resp.StatusCode)
	}
	var rep replay.Report
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rep); err != nil {
		return replay.Report{}, fmt.Errorf("decoding the replay report: %w", err)
	}
	return rep, nil
}
