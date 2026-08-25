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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
)

func main() {
	// C8 launcher-injection (ADR 0079): when run as the `launcher-inject` initContainer with
	// `--install <path>`, self-copy into the shared emptyDir and exit — BEFORE any config/OTel/proxy setup
	// (the initContainer has none of that env). A normal launcher invocation has no --install arg and falls
	// through to the runtime path below.
	if handled, code := maybeInstall(os.Args); handled {
		os.Exit(code)
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	if err := validateEntrypoint(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	// ── OTel setup (best-effort, non-blocking) ────────────────────────────
	// If setup fails, the global provider remains no-op and requests still
	// succeed — tracing is best-effort per spec §8.2 / observability.md.
	ctx := context.Background()
	otelShutdown, otelErr := setupOTel(ctx, cfg.OTLPEndpoint)
	if otelErr != nil {
		fmt.Fprintf(os.Stderr, "launcher: otel init warning (tracing disabled): %v\n", otelErr)
		otelShutdown = func(context.Context) error { return nil }
	}

	// ── Reverse proxy ─────────────────────────────────────────────────────
	upstreamURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", cfg.UpstreamPort),
	}
	// Use the global tracer/propagator set by setupOTel (or the noop
	// defaults if OTel init failed).
	tracer := otel.Tracer(tracerName)
	prop := otel.GetTextMapPropagator()

	// A2A inbound access control (callee side): only wired when the agent is a
	// registry member (AGENT_REGISTRY_ID injected). nil otherwise, so a
	// non-mesh agent's /invoke path is unchanged.
	var guard inboundGuard
	if cfg.A2AEnabled() {
		guard = newA2AGuard(cfg.A2A, tracer)
	}

	// Async A2A consumer (eventing path): wired when the agent is a registry
	// member (a Trigger can deliver CloudEvents to it). The seen-set reuses the
	// M5 Valkey (MEMORY_BACKEND_ADDR); when that is absent the consumer still
	// runs but dedupe is fail-open by construction (no store to consult). The
	// production invoker POSTs the decoded payload back through the launcher's own
	// proxy port so the agent.invoke span + user container see it as a call. nil
	// when the agent is not a registry member — its request path is unchanged.
	var consumer asyncHandler
	if cfg.A2AEnabled() {
		// The seen-set prefers the state-layer proxy (M53, ADR 0050 §6): it presents
		// the pod token and the proxy scopes the seen-key by namespace + holds the
		// Valkey credential. Falls back to the direct Valkey until the m53.7 cutover.
		var seen SeenSet
		switch {
		case cfg.Memory.ProxyURL != "":
			seen = newHTTPSeenSet(cfg.Memory.ProxyURL, resolvePodTokenPath(os.Getenv("STATELAYER_TOKEN_PATH")))
		case cfg.Memory.BackendAddr != "":
			seen = newRedisSeenSet(cfg.Memory.BackendAddr)
		}
		// Blob offload (m7.6b): wired when OBJECT_STORE_ADDR is injected. The
		// offloader rehydrates a $ref payload before the agent is invoked. A
		// construction error (bad addr) is logged and the consumer runs WITHOUT
		// offload — a producer without a store never emits a $ref, so a nil
		// offloader is safe (nothing to rehydrate). nil when disabled.
		var off *offloader
		if cfg.ObjectStoreEnabled() {
			store, oErr := newMinioStore(cfg.ObjectStore.Addr, cfg.ObjectStore.AccessKey, cfg.ObjectStore.SecretKey)
			if oErr != nil {
				fmt.Fprintf(os.Stderr, "launcher: object store (offload disabled): %v\n", oErr)
			} else {
				off = newOffloader(store)
			}
		}
		consumer = &asyncConsumer{
			cfg:     asyncConfig{DedupeAddr: cfg.Memory.BackendAddr, SelfName: cfg.AgentName},
			seen:    seen,
			tracer:  tracer,
			offload: off,
			invoke:  newProxyInvoker(cfg.ProxyPort, &http.Client{Timeout: a2aRequestTimeout, CheckRedirect: refuseRedirect}),
		}
	}
	handler := buildHandler(tracer, prop, upstreamURL, cfg, guard, consumer)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ProxyPort),
		Handler: handler,
	}

	// ── Memory endpoint (:2998) ───────────────────────────────────────────
	// Started ONLY when a backend was injected (the agent has spec.sessionMemory).
	// It runs as a second listener beside the proxy with the SAME lifecycle
	// discipline (goroutine ListenAndServe; graceful Shutdown on child exit;
	// the child-exit still decides the process exit code — the memory listener
	// never overrides it). nil when disabled.
	// Long-term memory (ADR 0045) + managed-RAG retrieval (ADR 0061 Fork 3) both proxy to the token-service;
	// loaded from env, nil when off. The memory listener starts when ANY of session memory / long-term memory /
	// knowledge retrieval is enabled (an agent may have any subset).
	ltLogf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	memSrv := buildMemoryHTTPServer(cfg, tracer, newLongTermProxy(ltLogf, tracer), newKnowledgeProxy(ltLogf, tracer))

	// ── A2A outbound endpoint (:2997) ─────────────────────────────────────
	// Started ONLY when the agent is a resolved AgentRegistry member
	// (AGENT_REGISTRY_ID injected). Same lifecycle discipline as the memory
	// listener: a goroutine ListenAndServe, graceful Shutdown on child exit,
	// and it NEVER overrides the child-driven process exit code. nil when the
	// agent is not in a registry.
	var a2aSrv *http.Server
	if cfg.A2AEnabled() {
		a2aSrv = &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.A2A.Port),
			Handler: newA2AServer(cfg.A2A, tracer, prop).handler(),
		}
	}

	// ── Delegate (sub-agent spawn) endpoint (:2994) ───────────────────────
	// Started ONLY for a team SUPERVISOR (DELEGATE_ENABLED injected by the controller) with the BFF
	// spawn edge + shared Valkey reachable (M64, ADR 0057). Same lifecycle discipline as the other
	// listeners: goroutine ListenAndServe, graceful Shutdown on child exit, never overrides the child
	// exit code. nil for a non-supervisor agent (unchanged).
	delSrv := loadDelegateConfig(os.Getenv).buildServer()

	// ── Outbound gateway proxy (:2996) ────────────────────────────────────
	// Started ONLY when spec.budget is set (GATEWAY_UPSTREAM_URL injected AND a
	// cap present). It sits between the agent and LiteLLM to enforce the cost
	// budget; MODEL_GATEWAY_URL is repointed here by the controller. Same
	// lifecycle discipline as the memory/A2A listeners: goroutine ListenAndServe,
	// graceful Shutdown on child exit, never overrides the child exit code. A
	// construction error (bad upstream URL / cap) is logged and the listener is
	// skipped — the agent's MODEL_GATEWAY_URL then 502s, a visible misconfig, not
	// a silent budget bypass. nil when disabled → unbudgeted agents are unchanged.
	gwSrv := buildGatewayServer(cfg, tracer)

	// ── Feedback ingest hook (:2995) ──────────────────────────────────────────
	// Started ONLY when the controller injected LANGFUSE_HOST (the agent has
	// been wired for feedback). Same lifecycle discipline as the other listeners:
	// goroutine ListenAndServe, graceful Shutdown on child exit, never overrides
	// the child exit code. A bind failure is logged; the /feedback endpoint then
	// returns ECONNREFUSED, a visible misconfig, not a silent drop. nil when
	// disabled → agents without feedback wiring are unchanged.
	fbSrv := buildFeedbackServer(cfg)

	// ── Child process ─────────────────────────────────────────────────────
	child, err := startChild(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	mainPID := child.Process.Pid

	// Forward SIGTERM / SIGINT to the child.
	go forwardSignals(child)

	// PID-1 reaping loop: Wait4(-1) reaps the direct child and any orphaned
	// grandchildren reparented to us. When mainPID exits, its code is sent
	// here. cmd.Wait() is intentionally NOT called (reapAll owns all waitpid
	// calls to avoid a race between two waitpid callers on the same PID).
	childExitCh := make(chan int, 1)
	go func() {
		childExitCh <- reapAll(mainPID)
	}()

	// Start proxy (non-fatal if the port is briefly unavailable at startup —
	// Knative will retry the health probe).
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "launcher: proxy: %v\n", err)
		}
	}()

	// Start the optional listeners best-effort (a bind failure is logged, never takes the agent down —
	// each is a best-effort side path). memory / A2A / delegate / gateway / feedback; nil ones are skipped.
	startBestEffort(memSrv, "memory")
	startBestEffort(a2aSrv, "a2a")
	startBestEffort(delSrv, "delegate")
	startBestEffort(gwSrv, "gateway")
	startBestEffort(fbSrv, "feedback")

	// ── Wait for child to exit, then shut down cleanly ────────────────────
	exitCode := <-childExitCh

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, listener := range []*http.Server{srv, memSrv, a2aSrv, delSrv, gwSrv, fbSrv} {
		if listener != nil {
			_ = listener.Shutdown(shutCtx)
		}
	}
	_ = otelShutdown(shutCtx)

	os.Exit(exitCode)
}

// startBestEffort runs an optional listener in a goroutine, logging a real bind failure without taking
// the agent down (each side listener — memory / A2A / delegate / gateway / feedback — is best-effort). A
// nil server is a no-op (the feature is disabled).
func startBestEffort(srv *http.Server, name string) {
	if srv == nil {
		return
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "launcher: %s: %v\n", name, err)
		}
	}()
}
