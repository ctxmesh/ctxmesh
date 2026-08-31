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

// replay_serve.go implements `ctxmesh replay-serve <fixture>` — the thin compose-container
// wrapper around the pure-Go ReplaySession (ADR 0071 §3a). It loads + merges the fixture,
// builds a ReplaySession, and serves its handler on a port. `dev --replay` swaps the LiteLLM
// gateway service for a container running THIS subcommand (Dockerfile.replay); the load-bearing
// logic lives in internal/replay (server.go), unit-tested docker-free via httptest.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ctxmesh/ctxmesh/internal/replay"
)

// newReplayServeCmd builds the cobra `replay-serve` command. It is intentionally minimal: the
// divergence policy + both-channel routing are in internal/replay; this only wires a fixture path
// to an http.Server.
func newReplayServeCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "replay-serve <fixture-path>",
		Short: "Serve a recorded fixture as the both-channel replay mock (internal — used by dev --replay)",
		Long: `replay-serve loads a recorded fixture (a single merged fixture JSON file OR a
directory of partial *.json blobs it reads + merges) and serves the deterministic
replay mock on a port:

  /v1/*           the MODEL channel (OpenAI-shape) — re-serves the Nth recorded model
                  response verbatim (index-matched; byte-lenient, shape-strict).
  /mcp, /mcp/*    the TOOL channel — a minimal MCP streamable-http mock re-serving
                  recorded tool results (matched by call-id / name+args).
  GET /replay/report   the accumulated divergence verdict as JSON.

This is the container that ` + "`ctxmesh dev --replay`" + ` swaps in for the mock
gateway; it is not a day-to-day command. It never re-executes tools or calls a
model — every response comes from the fixture, so replay is fully deterministic
with zero cluster deps (ADR 0071 §3a).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReplayServe(cmd.Context(), args[0], port, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 4000, "port to serve the replay mock on")
	return cmd
}

// runReplayServe loads the fixture, builds the session, and serves until signalled. It binds
// 0.0.0.0 (the container is reached over the compose network by service name — NOT loopback), so
// there is no Linux-CI host-gateway trap (ADR 0071 §3a rejects the host.docker.internal topology).
func runReplayServe(ctx context.Context, fixturePath string, port int, out, errOut interface {
	Write([]byte) (int, error)
},
) error {
	fx, err := replay.LoadFixturePath(fixturePath)
	if err != nil {
		return err
	}
	session := replay.NewReplaySession(fx)

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", port),
		Handler:           session.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	statusf(out, "ctxmesh replay-serve: run=%q agent=%q model=%d tool(s)=%d — serving on :%d\n",
		fx.RunID, fx.Agent, len(fx.Model), len(fx.Tools), port)

	lisErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			lisErr <- err
			return
		}
		lisErr <- nil
	}()

	select {
	case err := <-lisErr:
		if err != nil {
			return fmt.Errorf("replay-serve: listen on :%d: %w", port, err)
		}
		return nil
	case <-runCtx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		statusf(errOut, "ctxmesh replay-serve: shut down\n")
		return nil
	}
}
