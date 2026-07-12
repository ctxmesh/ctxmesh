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

// dev.go implements `agent-engine dev` — the local inner-loop command (spec §22).
// It renders a Docker Compose stack (see dev_plan.go for the pure planning) that
// runs the SAME launcher + user agent production runs, wired with the full
// localhost runtime contract and a mock gateway (MOCK_OK) by default, then blocks
// until Ctrl-C and tears the stack down cleanly.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/bff"
)

// devReadyTimeout bounds how long `dev` waits for /invoke to first answer
// MOCK_OK after `compose up`. LiteLLM (Python) is the slow service; it can take
// well over a minute to become ready on a cold pull.
const devReadyTimeout = 3 * time.Minute

// statusf writes a best-effort status line to w. The write error is intentionally
// ignored: these are progress messages on stdout/stderr and a broken pipe there
// must not fail the run (the same convention expand.go uses with `_, _ =`).
func statusf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// devFlagValues holds the raw cobra flag values before validation.
type devFlagValues struct {
	file      string
	port      int
	provider  string
	realBase  string
	realModel string
	keyEnv    string
	noWait    bool
	ui        bool
	uiPort    int
	uiDist    string
}

// newDevCmd builds the cobra `dev` command.
func newDevCmd() *cobra.Command {
	var fv devFlagValues

	cmd := &cobra.Command{
		Use:   "dev [flags]",
		Short: "Run the agent locally with the full runtime contract + a mock gateway",
		Long: `dev runs the SAME launcher + your agent locally on Docker Compose with the
full runtime contract (the traced proxy on AGENT_PORT, memory :2998, feedback
:2995, and the tool-discovery sidecar :2999), so your inner loop matches
production. The model gateway points at a deterministic MOCK provider by default
(every completion is "MOCK_OK…"); pass --provider real to use a real provider.

It reads the same simplified agent.yaml the ` + "`expand`" + ` command consumes
(name + image required; model.route optional). On startup it prints the local
/invoke URL; press Ctrl-C to tear the stack down.

Prerequisites (built once via the engine Makefile):
  - the agent image referenced by agent.yaml (e.g. make docker-build-example)
  - the discovery sidecar image: make docker-build-discovery

Provider modes:
  --provider mock   (default) gateway returns MOCK_OK — no key, deterministic, fast.
  --provider real   gateway calls a real upstream; requires --model and a key env
                    var (see --key-env). The key is injected into the gateway
                    container only — never the agent, never written to disk.

Examples:
  agent-engine dev -f examples/echo-agent/agent.yaml
  agent-engine dev -f agent.yaml --port 9090
  agent-engine dev -f agent.yaml --provider real \
      --model openai/gpt-4o-mini --key-env OPENAI_API_KEY

Exit codes: 0 = ok; 1 = validation error; 2 = file or parse error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runDev(cmd.Context(), fv, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
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

	cmd.Flags().StringVarP(&fv.file, "file", "f", "agent.yaml", "path to the agent.yaml to run")
	cmd.Flags().IntVarP(&fv.port, "port", "p", 8080, "host port to publish the agent's /invoke on")
	cmd.Flags().StringVar(&fv.provider, "provider", "mock", "gateway backend: mock (deterministic MOCK_OK) or real")
	cmd.Flags().StringVar(&fv.realModel, "model", "", "upstream model id for --provider real (e.g. openai/gpt-4o-mini)")
	cmd.Flags().StringVar(&fv.realBase, "base-url", "",
		"upstream base URL for --provider real (optional; provider default otherwise)")
	cmd.Flags().StringVar(&fv.keyEnv, "key-env", "", "name of the host env var holding the API key for --provider real")
	cmd.Flags().BoolVar(&fv.noWait, "no-wait", false, "render + up the stack but do not block or run the readiness smoke")
	cmd.Flags().BoolVar(&fv.ui, "ui", false,
		"also serve the console UI (dev-mode BFF: config-preview + the local /invoke, no cluster, no login)")
	cmd.Flags().IntVar(&fv.uiPort, "ui-port", 8888, "host port for the console UI when --ui is set")
	cmd.Flags().StringVar(&fv.uiDist, "ui-dist", "ui/dist",
		"directory of the built SPA (dist/) to serve with --ui; run `make build-ui` first")

	return cmd
}

// resolveDevFlags validates raw flag values into a typed devFlags. The API key
// for --provider real is read from the named host env var (never a literal on the
// command line, never committed).
func resolveDevFlags(fv devFlagValues) (devFlags, error) {
	if fv.port < 1 || fv.port > 65535 {
		return devFlags{}, validationErr("--port %d out of range (1–65535)", fv.port)
	}

	mode := providerMode(strings.ToLower(strings.TrimSpace(fv.provider)))
	flags := devFlags{
		File:        fv.file,
		Port:        fv.port,
		Provider:    mode,
		RealBaseURL: strings.TrimSpace(fv.realBase),
		RealModel:   strings.TrimSpace(fv.realModel),
	}

	switch mode {
	case providerMock:
		// Nothing else required.
	case providerReal:
		if flags.RealModel == "" {
			return devFlags{}, validationErr("--provider real requires --model")
		}
		if strings.TrimSpace(fv.keyEnv) == "" {
			return devFlags{}, validationErr("--provider real requires --key-env NAME (the host env var holding the API key)")
		}
		key := os.Getenv(fv.keyEnv)
		if key == "" {
			return devFlags{}, validationErr("env var %q (from --key-env) is empty — export your API key first", fv.keyEnv)
		}
		flags.RealAPIKey = key
	default:
		return devFlags{}, validationErr("--provider %q invalid — use mock or real", fv.provider)
	}
	return flags, nil
}

// runDev is the command body: validate flags, parse agent.yaml, build the plan,
// materialize the Compose assets, bring the stack up, run the readiness smoke,
// then block until Ctrl-C and tear down.
func runDev(ctx context.Context, fv devFlagValues, out, errOut io.Writer) error {
	flags, err := resolveDevFlags(fv)
	if err != nil {
		return err
	}
	// --ui needs the process to stay alive to serve the console; --no-wait exits
	// immediately after `up`, so the two are mutually exclusive. Fail early, clearly.
	if fv.ui && fv.noWait {
		return validationErr("--ui cannot be combined with --no-wait (the UI needs the run to stay up)")
	}

	raw, err := os.ReadFile(flags.File)
	if err != nil {
		return parseErr("reading %q: %v", flags.File, err)
	}
	dy, err := parseDevYAML(raw)
	if err != nil {
		return err
	}
	plan, err := buildDevPlan(dy, flags)
	if err != nil {
		return err
	}

	if err := preflightDocker(); err != nil {
		return err
	}

	workDir, cleanupDir, err := writePlanAssets(plan)
	if err != nil {
		return err
	}
	// In --no-wait mode the stack is left running for the caller to manage, so the
	// rendered assets (the compose file `down` needs) must persist; otherwise the
	// temp dir is removed on return.
	if !fv.noWait {
		defer cleanupDir()
	}

	// Signal-driven context: Ctrl-C (SIGINT) / SIGTERM cancels the run and triggers
	// teardown. Layered under the caller's ctx so a parent cancel also tears down.
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	statusf(out, "agent-engine dev: starting stack for %q (provider=%s, route=%s)\n",
		plan.AgentName, plan.Provider, plan.Route)
	statusf(out, "  image:   %s\n", plan.Image)
	statusf(out, "  gateway: %s (mock=MOCK_OK)\n", litellmImage)

	env := composeEnv(flags)
	teardown := func() {
		statusf(out, "\nagent-engine dev: tearing down…\n")
		down := composeCommand(context.Background(), workDir, env, "down", "--volumes", "--remove-orphans")
		down.Stdout, down.Stderr = errOut, errOut
		if derr := down.Run(); derr != nil {
			statusf(errOut, "agent-engine dev: teardown warning: %v\n", derr)
		}
	}
	// Teardown runs on any exit of a blocking run (smoke failure, Ctrl-C, error).
	// In --no-wait mode the caller owns the lifecycle, so we do NOT tear down.
	if !fv.noWait {
		defer teardown()
	}

	// Bring the stack up detached so we can poll readiness and stay responsive to
	// Ctrl-C. A non-zero exit here is a hard error (bad image, port clash, …).
	up := composeCommand(runCtx, workDir, env, "up", "-d")
	up.Stdout, up.Stderr = out, errOut
	if err := up.Run(); err != nil {
		return fmt.Errorf("docker compose up failed: %w", err)
	}

	if fv.noWait {
		statusf(out, "agent-engine dev: stack up (--no-wait); /invoke at %s\n", plan.InvokeURL())
		statusf(out, "  tear down with: docker compose -f %s down --volumes --remove-orphans\n",
			filepath.Join(workDir, "docker-compose.yaml"))
		return nil
	}

	// Readiness smoke: poll /invoke until it answers (MOCK_OK in mock mode).
	if err := waitForInvoke(runCtx, plan, out, errOut); err != nil {
		return err
	}

	statusf(out, "\nagent-engine dev: READY — POST to %s\n", plan.InvokeURL())
	statusf(out, "  example: curl -s -X POST -d 'hello' %s\n", plan.InvokeURL())

	// --ui: serve the console alongside the loop (ADR 0021). The dev-mode BFF is a
	// local substrate — no cluster, no login — so only the K8s-free surfaces are live.
	if fv.ui {
		shutdownUI, err := serveDevUI(fv.uiPort, fv.uiDist, plan, out, errOut)
		if err != nil {
			return err
		}
		defer shutdownUI()
	}

	statusf(out, "  press Ctrl-C to stop.\n")

	// Block until Ctrl-C / SIGTERM (or parent-ctx cancel). Teardown runs via defer.
	<-runCtx.Done()
	return nil
}

// serveDevUI starts the dev-mode console BFF (ADR 0021) on 127.0.0.1:uiPort and
// serves the built SPA from uiDist. It is the LOCAL substrate: no cluster
// (CallerClients nil → every cluster-backed endpoint answers an honest 501) and no
// login wall (AllowAll). Only the K8s-free surfaces are live — config-preview
// (/api/expand) and the local /invoke run — matching the `dev` inner loop. It
// returns a shutdown func the caller defers; a nil error means the listener is up.
func serveDevUI(uiPort int, uiDist string, plan *devPlan, out, errOut io.Writer) (func(), error) {
	index := filepath.Join(uiDist, "index.html")
	if _, err := os.Stat(index); err != nil {
		return nil, validationErr(
			"console UI build not found at %s (run `make build-ui` first, or pass --ui-dist): %v", uiDist, err,
		)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding agents to scheme: %w", err)
	}

	// The local agent's /invoke is published on the host at plan.HostPort; the BFF
	// (same host) reaches its BASE at http://localhost:<port> (the adapter appends
	// /invoke). This is what the dev Playground run targets — no cluster resolution.
	devInvokeBase := fmt.Sprintf("http://localhost:%d", plan.HostPort)

	srv := bff.NewServer(bff.Options{
		DevMode:           true,
		DevInvokeEndpoint: devInvokeBase,
		Auth:              bff.AllowAll{}, // single local developer — no login wall
		CallerClients:     nil,            // no cluster → cluster endpoints answer honest 501
		Scheme:            scheme,
		StaticDir:         uiDist,
		Version:           "dev",
		Adapters: bff.Adapters{
			Expand: bff.NewExpandAdapter(),                          // config-preview (K8s-free)
			Invoke: bff.NewInvokeAdapter(bff.InvokeAdapterConfig{}), // Playground → local /invoke
		},
		Log: logr.Discard(),
	})

	addr := fmt.Sprintf("127.0.0.1:%d", uiPort)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	lisErr := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			statusf(errOut, "agent-engine dev: console UI server error: %v\n", err)
			lisErr <- err
			return
		}
		lisErr <- nil
	}()

	// Give ListenAndServe a beat to fail fast on a bind error (e.g. a port clash) so
	// we surface it as a startup error rather than a silent background death.
	select {
	case err := <-lisErr:
		if err != nil {
			return nil, fmt.Errorf("console UI failed to start on %s: %w", addr, err)
		}
	case <-time.After(150 * time.Millisecond):
	}

	statusf(out, "\nagent-engine dev: console UI at http://%s\n", addr)
	statusf(out, "  dev mode — no login; cluster surfaces disabled. Config-preview is live; "+
		"the agent answers at %s\n", plan.InvokeURL())

	shutdown := func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}
	return shutdown, nil
}

// composeEnv builds the extra environment passed to `docker compose` — the real
// provider key when in real mode (injected into the gateway container by the
// compose file's ${DEV_PROVIDER_KEY} reference). In mock mode it is empty.
func composeEnv(flags devFlags) []string {
	if flags.Provider == providerReal && flags.RealAPIKey != "" {
		return []string{"DEV_PROVIDER_KEY=" + flags.RealAPIKey}
	}
	return nil
}

// composeCommand builds a `docker compose -f <workDir>/docker-compose.yaml <args>`
// exec.Cmd, inheriting the current environment plus any extra entries.
func composeCommand(ctx context.Context, workDir string, extraEnv []string, args ...string) *exec.Cmd {
	full := append([]string{"compose", "-f", filepath.Join(workDir, "docker-compose.yaml")}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd
}

// preflightDocker checks that `docker compose` is available before rendering.
func preflightDocker() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found on PATH: %w", err)
	}
	out, err := exec.Command("docker", "compose", "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("`docker compose` unavailable (Compose v2 required): %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writePlanAssets materializes the rendered compose file, the gateway config, and
// tools.json into a fresh temp dir. Returns the dir and a cleanup func.
func writePlanAssets(plan *devPlan) (string, func(), error) {
	dir, err := os.MkdirTemp("", "agent-engine-dev-")
	if err != nil {
		return "", nil, fmt.Errorf("creating work dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	files := map[string]string{
		"docker-compose.yaml": plan.ComposeYAML,
		"gateway-config.yaml": plan.GatewayConfigYAML,
		"tools.json":          plan.ToolsJSON,
	}
	for name, content := range files {
		if werr := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); werr != nil {
			cleanup()
			return "", nil, fmt.Errorf("writing %s: %w", name, werr)
		}
	}
	return dir, cleanup, nil
}

// waitForInvoke polls the agent's /invoke until it answers. In mock mode it
// requires the MOCK_OK marker; in real mode any 2xx is sufficient (the real
// completion text is non-deterministic). It respects ctx cancellation (Ctrl-C).
func waitForInvoke(ctx context.Context, plan *devPlan, out, errOut io.Writer) error {
	deadline := time.Now().Add(devReadyTimeout)
	client := &http.Client{Timeout: 10 * time.Second}
	statusf(out, "agent-engine dev: waiting for %s to answer…\n", plan.InvokeURL())

	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		body, err := postInvoke(ctx, client, plan.InvokeURL())
		if err != nil {
			lastErr = err
		} else if plan.Provider == providerMock && !strings.Contains(body, "MOCK_OK") {
			lastErr = fmt.Errorf("agent answered but without MOCK_OK marker: %s", truncate(body, 200))
		} else {
			statusf(out, "agent-engine dev: /invoke answered: %s\n", truncate(body, 200))
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	statusf(errOut, "agent-engine dev: agent did not become ready in %s\n", devReadyTimeout)
	return fmt.Errorf("readiness smoke failed: last error: %v", lastErr)
}

// postInvoke POSTs a fixed probe body to the /invoke URL and returns the response
// body on a 2xx, or an error otherwise.
func postInvoke(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("dev readiness probe"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("/invoke returned %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return string(data), nil
}

// truncate shortens s to at most n runes, appending an ellipsis when clipped.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
