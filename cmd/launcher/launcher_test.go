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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// envMap returns a lookup function backed by a static map of env variables.
func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// ── loadConfig ────────────────────────────────────────────────────────────────

// TestLoadConfig covers the env-variable parsing logic for loadConfig.
func TestLoadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		wantArgv []string
		wantErr  bool
	}{
		{
			name:    "missing AGENT_ENTRYPOINT returns error",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "empty AGENT_ENTRYPOINT returns error",
			env:     map[string]string{"AGENT_ENTRYPOINT": ""},
			wantErr: true,
		},
		{
			name:     "entrypoint only — no args",
			env:      map[string]string{"AGENT_ENTRYPOINT": "/bin/myapp"},
			wantArgv: []string{"/bin/myapp"},
		},
		{
			name: "entrypoint with args",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "--port 8080",
			},
			wantArgv: []string{"/bin/myapp", "--port", "8080"},
		},
		{
			name: "args with extra whitespace are trimmed",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "  -v  --debug  ",
			},
			wantArgv: []string{"/bin/myapp", "-v", "--debug"},
		},
		{
			name: "args with tabs and multiple spaces",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "--host\t127.0.0.1   --port 9090",
			},
			wantArgv: []string{"/bin/myapp", "--host", "127.0.0.1", "--port", "9090"},
		},
		{
			name: "empty AGENT_ENTRYPOINT_ARGS is ignored",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "",
			},
			wantArgv: []string{"/bin/myapp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadConfig(envMap(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadConfig() expected an error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}
			if len(cfg.Argv) != len(tt.wantArgv) {
				t.Fatalf("argv len = %d, want %d; got %v", len(cfg.Argv), len(tt.wantArgv), cfg.Argv)
			}
			for i, v := range cfg.Argv {
				if v != tt.wantArgv[i] {
					t.Errorf("argv[%d] = %q, want %q", i, v, tt.wantArgv[i])
				}
			}
		})
	}
}

// TestLoadConfigPorts exercises the new port and OTel endpoint fields.
func TestLoadConfigPorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		env              map[string]string
		wantProxyPort    int
		wantUpstreamPort int
		wantOTLP         string
		wantErr          bool
	}{
		{
			name:             "all defaults apply",
			env:              map[string]string{"AGENT_ENTRYPOINT": "/bin/agent"},
			wantProxyPort:    8080,
			wantUpstreamPort: 8081,
			wantOTLP:         "localhost:4317",
		},
		{
			name: "explicit port overrides",
			env: map[string]string{
				"AGENT_ENTRYPOINT":            "/bin/agent",
				"AGENT_PORT":                  "9090",
				"AGENT_UPSTREAM_PORT":         "9091",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4317",
			},
			wantProxyPort:    9090,
			wantUpstreamPort: 9091,
			wantOTLP:         "collector:4317",
		},
		{
			name: "invalid proxy port returns error",
			env: map[string]string{
				"AGENT_ENTRYPOINT": "/bin/agent",
				"AGENT_PORT":       "notaport",
			},
			wantErr: true,
		},
		{
			name: "out-of-range upstream port returns error",
			env: map[string]string{
				"AGENT_ENTRYPOINT":    "/bin/agent",
				"AGENT_UPSTREAM_PORT": "99999",
			},
			wantErr: true,
		},
		{
			name: "span attributes parsed",
			env: map[string]string{
				"AGENT_ENTRYPOINT": "/bin/agent",
				"AGENT_NAME":       "my-agent",
				"AGENT_VERSION":    "v1.2.3",
				"AGENT_ROUTE":      "/invoke",
			},
			wantProxyPort:    8080,
			wantUpstreamPort: 8081,
			wantOTLP:         "localhost:4317",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadConfig(envMap(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ProxyPort != tt.wantProxyPort {
				t.Errorf("ProxyPort = %d, want %d", cfg.ProxyPort, tt.wantProxyPort)
			}
			if cfg.UpstreamPort != tt.wantUpstreamPort {
				t.Errorf("UpstreamPort = %d, want %d", cfg.UpstreamPort, tt.wantUpstreamPort)
			}
			if cfg.OTLPEndpoint != tt.wantOTLP {
				t.Errorf("OTLPEndpoint = %q, want %q", cfg.OTLPEndpoint, tt.wantOTLP)
			}
		})
	}
}

// ── validateEntrypoint ────────────────────────────────────────────────────────

// TestValidateEntrypoint covers the filesystem-validation logic.
func TestValidateEntrypoint(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("launcher targets Linux containers; skipping on Windows")
	}

	t.Run("valid executable passes", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/bin/sh"}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error for /bin/sh: %v", err)
		}
	})

	t.Run("extra argv elements do not affect validation", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/bin/sh", "-c", "echo hello"}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error when extra args present: %v", err)
		}
	})

	t.Run("nonexistent binary returns error", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/nonexistent/binary-does-not-exist"}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error for nonexistent binary, got nil")
		}
	})

	t.Run("directory returns error", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{t.TempDir()}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error when entrypoint is a directory, got nil")
		}
	})

	t.Run("non-executable file returns error", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "noexec-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.Name(), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Argv: []string{f.Name()}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error for non-executable file, got nil")
		}
	})

	t.Run("executable file passes", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "exec-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.Name(), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Argv: []string{f.Name()}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error for executable file: %v", err)
		}
	})
}

// ── shouldSpan ────────────────────────────────────────────────────────────────

// TestShouldSpan asserts the path→span decision table.
func TestShouldSpan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want bool
	}{
		{"/invoke", true},
		{"/", true},
		{"/api/v1/anything", true},
		{"/healthz", false},
		{"/readyz", false},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			if got := shouldSpan(c.path); got != c.want {
				t.Errorf("shouldSpan(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// ── buildChildEnv ─────────────────────────────────────────────────────────────

// TestBuildChildEnv verifies that AGENT_PORT is overridden to UpstreamPort.
func TestBuildChildEnv(t *testing.T) {
	t.Parallel()

	cfg := Config{UpstreamPort: 8081}

	t.Run("overrides existing AGENT_PORT", func(t *testing.T) {
		t.Parallel()
		env := []string{"AGENT_PORT=8080", "FOO=bar"}
		got := buildChildEnv(cfg, env)
		assertEnvContains(t, got, "AGENT_PORT=8081")
		assertEnvContains(t, got, "FOO=bar")
		assertEnvNotContains(t, got, "AGENT_PORT=8080")
	})

	t.Run("appends AGENT_PORT when absent", func(t *testing.T) {
		t.Parallel()
		env := []string{"FOO=bar"}
		got := buildChildEnv(cfg, env)
		assertEnvContains(t, got, "AGENT_PORT=8081")
		assertEnvContains(t, got, "FOO=bar")
	})

	t.Run("other vars are preserved unchanged", func(t *testing.T) {
		t.Parallel()
		env := []string{"AGENT_PORT=9999", "X=1", "Y=2"}
		got := buildChildEnv(cfg, env)
		assertEnvContains(t, got, "X=1")
		assertEnvContains(t, got, "Y=2")
		assertEnvNotContains(t, got, "AGENT_PORT=9999")
	})
}

func assertEnvContains(t *testing.T, env []string, kv string) {
	t.Helper()
	if !slices.Contains(env, kv) {
		t.Errorf("env missing %q; got %v", kv, env)
	}
}

func assertEnvNotContains(t *testing.T, env []string, kv string) {
	t.Helper()
	if slices.Contains(env, kv) {
		t.Errorf("env should not contain %q but does; got %v", kv, env)
	}
}

// ── proxy routing ─────────────────────────────────────────────────────────────

// newTestTracer returns a TracerProvider backed by an in-memory span recorder
// and the corresponding tracer. Spans are always sampled.
func newTestTracer(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

// TestProxyRouting uses an httptest upstream to verify forwarding and context
// propagation behaviour.
func TestProxyRouting(t *testing.T) {
	t.Parallel()

	// ── shared upstream that echoes the headers it received ───────────────
	type upstreamCapture struct {
		header http.Header
	}
	capturedCh := make(chan upstreamCapture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCh <- upstreamCapture{header: r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	// ── tracer + propagator ───────────────────────────────────────────────
	rec, tp := newTestTracer(t)
	tracer := tp.Tracer(tracerName)
	prop := propagation.TraceContext{}

	cfg := Config{
		AgentName:    "test-agent",
		AgentVersion: "v0.0.1",
		AgentRoute:   "/invoke",
	}
	handler := buildHandler(tracer, prop, upstreamURL, cfg, nil, nil)

	// ── /invoke: request is forwarded and traceparent is injected ─────────
	t.Run("/invoke forwarded with traceparent injected", func(t *testing.T) {
		// NOTE: not run in parallel — shares capturedCh
		req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", rr.Code)
		}

		cap := <-capturedCh
		tp := cap.header.Get("Traceparent")
		if tp == "" {
			t.Error("upstream did not receive traceparent header on /invoke")
		}

		// The span must have been recorded.
		spans := rec.Ended()
		if len(spans) == 0 {
			t.Fatal("no spans recorded for /invoke")
		}
		found := false
		for _, s := range spans {
			if s.Name() == "agent.invoke" {
				found = true
			}
		}
		if !found {
			t.Errorf("no 'agent.invoke' span found in recorded spans: %v", spans)
		}
	})

	// ── A2A messageId → X-Message-Id forwarded to the user container (m33.4) ──
	t.Run("A2A envelope messageId is forwarded as X-Message-Id", func(t *testing.T) {
		// NOTE: not run in parallel — shares capturedCh
		req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
		req.Header.Set(a2aEnvelopeHeader, `{"messageId":"m-hop-7","conversationId":"c1"}`)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		cap := <-capturedCh
		if got := cap.header.Get("X-Message-Id"); got != "m-hop-7" {
			t.Errorf("X-Message-Id forwarded = %q, want m-hop-7 (the A2A hop's id)", got)
		}
	})

	// A top-level /invoke (no A2A envelope) forwards NO X-Message-Id — the memory endpoint mints one.
	t.Run("non-A2A /invoke forwards no X-Message-Id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		cap := <-capturedCh
		if got := cap.header.Get("X-Message-Id"); got != "" {
			t.Errorf("X-Message-Id = %q, want empty on a non-A2A /invoke", got)
		}
	})

	// ── /healthz: request forwarded, NO span emitted ──────────────────────
	t.Run("/healthz forwarded without span", func(t *testing.T) {
		before := len(rec.Ended())

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", rr.Code)
		}

		<-capturedCh // drain — request was forwarded

		after := len(rec.Ended())
		if after != before {
			t.Errorf("span emitted for /healthz: before=%d after=%d (want no new spans)", before, after)
		}
	})

	// ── /readyz: same as /healthz ──────────────────────────────────────────
	t.Run("/readyz forwarded without span", func(t *testing.T) {
		before := len(rec.Ended())

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", rr.Code)
		}

		<-capturedCh

		after := len(rec.Ended())
		if after != before {
			t.Errorf("span emitted for /readyz: before=%d after=%d (want no new spans)", before, after)
		}
	})

	// ── /invoke: incoming traceparent is continued (not overwritten) ───────
	t.Run("/invoke continues incoming trace context", func(t *testing.T) {
		// A caller's traceparent — valid W3C format.
		callerTraceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

		req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
		req.Header.Set("Traceparent", callerTraceparent)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		cap := <-capturedCh
		fwdTP := cap.header.Get("Traceparent")
		if fwdTP == "" {
			t.Fatal("upstream did not receive traceparent")
		}
		// The forwarded traceparent must carry the same trace-id as the
		// caller's (first 32 hex chars after "00-").
		callerTraceID := strings.Split(callerTraceparent, "-")[1]
		fwdTraceID := strings.Split(fwdTP, "-")[1]
		if fwdTraceID != callerTraceID {
			t.Errorf("trace-id changed: caller=%q forwarded=%q", callerTraceID, fwdTraceID)
		}
	})
}

// TestLoadConfig_PromptVersion: PROMPT_VERSION is parsed into cfg.PromptVersion
// (the M9 prompt-only-deploy display identifier); absent → empty.
func TestLoadConfig_PromptVersion(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(envMap(map[string]string{
		"AGENT_ENTRYPOINT": "/bin/agent",
		"PROMPT_VERSION":   "abc123def456",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PromptVersion != "abc123def456" {
		t.Errorf("PromptVersion = %q, want %q", cfg.PromptVersion, "abc123def456")
	}

	cfg, err = loadConfig(envMap(map[string]string{"AGENT_ENTRYPOINT": "/bin/agent"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PromptVersion != "" {
		t.Errorf("PromptVersion = %q, want empty when PROMPT_VERSION unset", cfg.PromptVersion)
	}
}

// TestProxy_PromptVersionSpanAttribute: the agent.invoke span carries the
// prompt.version attribute (display-only; git is the source of truth) exactly
// when the agent has a resolved prompt (cfg.PromptVersion set), and NOT when it
// has none (image-bundled prompt — no empty attribute).
func TestProxy_PromptVersionSpanAttribute(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	prop := propagation.TraceContext{}

	promptVersionOf := func(cfg Config) (string, bool) {
		t.Helper()
		rec, tp := newTestTracer(t)
		handler := buildHandler(tp.Tracer(tracerName), prop, upstreamURL, cfg, nil, nil)
		req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)

		var invoke sdktrace.ReadOnlySpan
		for _, s := range rec.Ended() {
			if s.Name() == "agent.invoke" {
				invoke = s
			}
		}
		if invoke == nil {
			t.Fatal("no agent.invoke span recorded")
		}
		for _, kv := range invoke.Attributes() {
			if string(kv.Key) == "prompt.version" {
				return kv.Value.AsString(), true
			}
		}
		return "", false
	}

	// With a resolved prompt → the attribute is present and carries the version.
	got, present := promptVersionOf(Config{AgentName: "a", PromptVersion: "deadbeef1234"})
	if !present {
		t.Fatal("prompt.version attribute missing when PromptVersion is set")
	}
	if got != "deadbeef1234" {
		t.Errorf("prompt.version = %q, want %q", got, "deadbeef1234")
	}

	// Without a prompt (image-bundled) → no prompt.version attribute at all.
	if _, present := promptVersionOf(Config{AgentName: "a"}); present {
		t.Error("prompt.version attribute present when the agent has no promptRef")
	}
}

// TestLoadConfig_PodNamespace: POD_NAMESPACE parses into cfg.AgentNamespace (the
// trace-identity namespace); absent → empty.
func TestLoadConfig_PodNamespace(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(envMap(map[string]string{
		"AGENT_ENTRYPOINT": "/bin/agent",
		"POD_NAMESPACE":    "team-a",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AgentNamespace != "team-a" {
		t.Errorf("AgentNamespace = %q, want %q", cfg.AgentNamespace, "team-a")
	}

	cfg, err = loadConfig(envMap(map[string]string{"AGENT_ENTRYPOINT": "/bin/agent"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AgentNamespace != "" {
		t.Errorf("AgentNamespace = %q, want empty when POD_NAMESPACE unset", cfg.AgentNamespace)
	}
}

// invokeSpanAttrs drives one /invoke through the proxy for cfg and returns the
// agent.invoke span's attributes as a key→string map (string-slice values joined
// with a sentinel), so a test can assert the trace-level identity tag AND that the
// existing agent.name/version/route attributes are unchanged.
func invokeSpanAttrs(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec, tp := newTestTracer(t)
	handler := buildHandler(tp.Tracer(tracerName), propagation.TraceContext{}, upstreamURL, cfg, nil, nil)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/invoke", nil))

	var invoke sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "agent.invoke" {
			invoke = s
		}
	}
	if invoke == nil {
		t.Fatal("no agent.invoke span recorded")
	}
	attrs := map[string]string{}
	for _, kv := range invoke.Attributes() {
		if kv.Value.Type() == attribute.STRINGSLICE {
			attrs[string(kv.Key)] = strings.Join(kv.Value.AsStringSlice(), ",")
			continue
		}
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	return attrs
}

// TestProxy_AgentIdentityTraceTag: the agent.invoke span carries the trace-level
// `langfuse.trace.tags` attribute `agent:<ns>/<name>` (the UNAMBIGUOUS per-agent
// filter key the BFF per-agent run list uses) AND leaves the existing
// agent.name/version/route span attributes intact — the tag is ADDITIVE, so
// RecentRuns / CostUsage / the run inspector are unaffected.
func TestProxy_AgentIdentityTraceTag(t *testing.T) {
	t.Parallel()

	attrs := invokeSpanAttrs(t, Config{
		AgentName:      "foo",
		AgentNamespace: "team-a",
		AgentVersion:   "v0.0.1",
		AgentRoute:     "/invoke",
	})

	// The trace-level identity tag is present and namespace-qualified.
	if got, want := attrs[langfuseTraceTagsAttr], "agent:team-a/foo"; got != want {
		t.Errorf("%s = %q, want %q", langfuseTraceTagsAttr, got, want)
	}
	// The existing attributes are UNCHANGED (non-breaking).
	if attrs["agent.name"] != "foo" {
		t.Errorf("agent.name = %q, want %q (existing attribute must be intact)", attrs["agent.name"], "foo")
	}
	if attrs["agent.version"] != "v0.0.1" {
		t.Errorf("agent.version = %q, want %q", attrs["agent.version"], "v0.0.1")
	}
	if attrs["agent.route"] != "/invoke" {
		t.Errorf("agent.route = %q, want %q", attrs["agent.route"], "/invoke")
	}
}

// TestProxy_AgentTraceName: the agent.invoke span carries `langfuse.trace.name`
// so the trace has a human-readable name in the runs list. The trace ROOT is the
// BFF's never-exported seed span (invoke.go mints a traceparent), so WITHOUT this
// the trace is unnamed and invisible in the runs list. Name = `<ns>/<name>`,
// degrading to the bare name, then to "agent.invoke" — NEVER empty.
func TestProxy_AgentTraceName(t *testing.T) {
	t.Parallel()

	// Namespaced agent → `<ns>/<name>`.
	nsAttrs := invokeSpanAttrs(t, Config{AgentName: "foo", AgentNamespace: "team-a"})
	if got, want := nsAttrs[langfuseTraceNameAttr], "team-a/foo"; got != want {
		t.Errorf("%s = %q, want %q", langfuseTraceNameAttr, got, want)
	}
	// Namespace absent → bare name.
	if got, want := invokeSpanAttrs(t, Config{AgentName: "foo"})[langfuseTraceNameAttr], "foo"; got != want {
		t.Errorf("namespace-absent %s = %q, want %q", langfuseTraceNameAttr, got, want)
	}
	// Unnamed agent → the "agent.invoke" fallback, NEVER empty (still a visible run).
	if got, want := invokeSpanAttrs(t, Config{})[langfuseTraceNameAttr], "agent.invoke"; got != want {
		t.Errorf("unnamed-agent %s = %q, want %q (must never be empty)", langfuseTraceNameAttr, got, want)
	}
}

// TestProxy_AgentIdentityTagCrossNamespaceDistinct: two agents that share a bare
// NAME in different namespaces get DISTINCT identity tags — the property that keeps
// the BFF per-agent run list from mixing default/foo with other/foo.
func TestProxy_AgentIdentityTagCrossNamespaceDistinct(t *testing.T) {
	t.Parallel()

	a := invokeSpanAttrs(t, Config{AgentName: "foo", AgentNamespace: "default"})[langfuseTraceTagsAttr]
	b := invokeSpanAttrs(t, Config{AgentName: "foo", AgentNamespace: "other"})[langfuseTraceTagsAttr]
	if a == b {
		t.Fatalf("same-named agents in different namespaces got the SAME tag %q — cross-namespace runs would mix", a)
	}
	if a != "agent:default/foo" || b != "agent:other/foo" {
		t.Errorf("tags = %q / %q, want agent:default/foo / agent:other/foo", a, b)
	}
}

// TestProxy_AgentIdentityTagDegradesWithoutNamespace: a mis-injected launcher with
// no POD_NAMESPACE degrades to the bare `agent:<name>` (never an empty-namespace
// `agent:/name`); an UNNAMED agent emits NO tag at all (nothing to filter on —
// honest, not a crash).
func TestProxy_AgentIdentityTagDegradesWithoutNamespace(t *testing.T) {
	t.Parallel()

	// Namespace absent → bare name, no empty segment.
	if got, want := invokeSpanAttrs(t, Config{AgentName: "foo"})[langfuseTraceTagsAttr], "agent:foo"; got != want {
		t.Errorf("namespace-absent tag = %q, want %q", got, want)
	}

	// Unnamed agent → no identity tag stamped.
	if _, present := invokeSpanAttrs(t, Config{})[langfuseTraceTagsAttr]; present {
		t.Errorf("%s stamped for an unnamed agent — nothing to filter on, so it must be absent", langfuseTraceTagsAttr)
	}
}
