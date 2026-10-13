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
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ctxmesh/ctxmesh/test/sdkconformance"
)

// Env the world does not set but the handlers read. Cleared so a developer's shell cannot change
// what the goldens are checked against.
var contractClearedEnv = []string{
	"STATELAYER_PROXY_URL", "MCP_CAPABILITY_PUBLIC_KEY", "MEMORY_SCOPE", "MEMORY_PER_USER",
	"MEMORY_LONGTERM_SCOPE", "ASYNC_PUBLISH_URL", "OBJECT_STORE_ADDR", "EMBEDDING_ROUTE", "TENANT_ID",
}

// TestSDKContractGoldens runs every golden in sdk/launcher-routes.json against the launcher's real
// handlers, wired from the World's environment the way main wires them. The SDK fakes are generated
// from these goldens, so a golden that is not true here would teach six SDKs a wrong answer.
func TestSDKContractGoldens(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	contract, _, err := sdkconformance.LoadFromRepo(wd)
	if err != nil {
		t.Fatal(err)
	}
	if problems := contract.Check(); len(problems) > 0 {
		t.Fatalf("the contract disagrees with itself:\n  %s", strings.Join(problems, "\n  "))
	}
	world, err := sdkconformance.StartWorld(contract.World, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(world.Close)
	env, err := world.Env()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range contractClearedEnv {
		t.Setenv(k, "")
	}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		t.Setenv(k, env[k])
	}
	t.Setenv("AGENT_ENTRYPOINT", "/bin/true") // loadConfig requires one; nothing is started

	listeners, disc := contractListeners(t)
	for _, r := range contract.Routes {
		srv, ok := listeners[r.Listener]
		if !ok {
			t.Errorf("%s: no %q listener in this test; wire it in contractListeners", r.Key(), r.Listener)
			continue
		}
		for _, g := range r.Goldens {
			t.Run(r.Key()+"/"+g.Name, func(t *testing.T) {
				if err := world.Reset(); err != nil {
					t.Fatal(err)
				}
				disc.reload()
				for _, problem := range runGolden(t, srv.URL, r, g, world, disc) {
					t.Error(problem)
				}
			})
		}
	}
}

// contractListeners builds each listener's handler the way main does, and fails if the World's
// environment leaves one of them off.
func contractListeners(t *testing.T) (map[string]*httptest.Server, *toolDiscovery) {
	t.Helper()
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	tracer := noop.NewTracerProvider().Tracer("sdk-contract")
	quiet := func(string, ...any) {}
	mem := buildMemoryHTTPServer(cfg, tracer, newLongTermProxy(quiet, tracer), newKnowledgeProxy(quiet, tracer))
	del := loadDelegateConfig(os.Getenv).buildServer()
	fb := buildFeedbackServer(cfg)
	discSrv, disc := buildDiscoveryServer(os.Getenv, quiet)
	if mem == nil || del == nil || fb == nil || discSrv == nil || !cfg.AMPEnabled() {
		t.Fatalf("the world leaves a listener off: memory=%t delegate=%t feedback=%t discovery=%t amp=%t",
			mem != nil, del != nil, fb != nil, discSrv != nil, cfg.AMPEnabled())
	}
	handlers := map[string]http.Handler{
		"memory":    mem.Handler,
		"amp":       newAMPServer(cfg.AMP, tracer, propagation.TraceContext{}, nil).handler(),
		"delegate":  del.Handler,
		"feedback":  fb.Handler,
		"discovery": discSrv.Handler,
	}
	out := make(map[string]*httptest.Server, len(handlers))
	for name, h := range handlers {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		out[name] = srv
	}
	t.Cleanup(disc.stop)
	return out, disc
}

func runGolden(
	t *testing.T, base string, r sdkconformance.Route, g sdkconformance.Golden,
	world *sdkconformance.Stubs, disc *toolDiscovery,
) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := base + g.Request.Path
	if len(g.Request.Query) > 0 {
		q := url.Values{}
		for k, v := range g.Request.Query {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	var body io.Reader
	if raw := g.Request.RawBody(); raw != nil {
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, g.Method(r), u, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range g.Request.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []string{"request failed: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	want := g.Response
	var out []string
	if resp.StatusCode != want.Status {
		out = append(out, fmt.Sprintf("status %d, golden says %d", resp.StatusCode, want.Status))
	}
	for _, k := range slices.Sorted(maps.Keys(want.Headers)) {
		if got := resp.Header.Get(k); got != want.Headers[k] {
			out = append(out, fmt.Sprintf("header %s is %q, golden says %q", k, got, want.Headers[k]))
		}
	}
	if want.Stream {
		if err := world.ChangeTools(); err != nil {
			t.Fatal(err)
		}
		disc.reload()
		got := make([]byte, len(*want.BodyText))
		if _, err := io.ReadFull(resp.Body, got); err != nil {
			return append(out, "reading the stream: "+err.Error())
		}
		if string(got) != *want.BodyText {
			out = append(out, fmt.Sprintf("stream sent %q, golden says %q", got, *want.BodyText))
		}
		return out
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return append(out, "reading the body: "+err.Error())
	}
	return append(out, compareBody(want, raw)...)
}

func compareBody(want sdkconformance.GoldenResponse, raw []byte) []string {
	switch {
	case want.Body != nil:
		eq, err := sdkconformance.JSONEqual(want.Body, raw)
		if err != nil {
			return []string{fmt.Sprintf("body %q is not JSON: %v", raw, err)}
		}
		if !eq {
			return []string{fmt.Sprintf("body %s, golden says %s", bytes.TrimSpace(raw), want.Body)}
		}
	case want.BodyText != nil:
		if string(raw) != *want.BodyText {
			return []string{fmt.Sprintf("body %q, golden says %q", raw, *want.BodyText)}
		}
	case len(raw) > 0:
		return []string{fmt.Sprintf("body %q, golden says it is empty", raw)}
	}
	return nil
}
