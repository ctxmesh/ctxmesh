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
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// launcherVerifiers builds every run-capability verifier the launcher has, from the process env, as it
// does at startup.
func launcherVerifiers(t *testing.T) map[string]*runcap.Verifier {
	t.Helper()
	tracer := noop.NewTracerProvider().Tracer("test")
	amp, err := capabilityVerifierFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	lt := newLongTermProxy(t.Logf, tracer)
	kb := newKnowledgeProxy(t.Logf, tracer)
	if lt == nil || kb == nil {
		t.Fatal("the long-term memory and knowledge proxies are not enabled")
	}
	return map[string]*runcap.Verifier{
		"AMP admission":                 amp,
		"model gateway (quota, cancel)": buildCapVerifier(t.Logf),
		"per-user long-term memory":     lt.verifier,
		"per-user knowledge bases":      kb.verifier,
		"per-user session memory":       sessionCapVerifier(memoryConfig{PerUser: true}),
		"a session memory not per-user": sessionCapVerifier(memoryConfig{}),
		"a shared-scope session memory": sessionCapVerifier(memoryConfig{PerUser: true, Scope: memoryScopeShared}),
	}
}

// m184.37i: with MCP_CAPABILITY_AUDIENCE unset, as the chart leaves it, the BFF mints for
// runcap.DefaultAudience; a verifier that defaulted to an empty audience refused every capability, so
// per-user memory, knowledge bases and quota refused under the default install. Each verifier accepts
// what the BFF mints and still refuses another audience, and an audience the install sets is honoured.
func TestEveryCapabilityVerifierAcceptsWhatTheBFFMints(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	mint := func(audience string) string {
		tok, err := runcap.NewSigner(priv, audience, nil).Mint(runcap.MintRequest{
			User: "u-hash", Agent: "team-a/agent", RunID: "run-1", TTL: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	byDefault, byInstall := mint(runcap.DefaultAudience), mint("an-installs-plane")

	t.Setenv("MCP_CAPABILITY_PUBLIC_KEY", runcap.EncodePublicKey(pub))
	t.Setenv("MEMORY_LONGTERM_ENABLED", enabledTrue)
	t.Setenv("KNOWLEDGE_BASE_ENABLED", enabledTrue)
	t.Setenv("TOKEN_SERVICE_URL", "http://token-service")
	t.Setenv("MCP_CAPABILITY_AUDIENCE", "")
	if err := os.Unsetenv("MCP_CAPABILITY_AUDIENCE"); err != nil {
		t.Fatal(err)
	}

	check := func(accepts, refuses string) {
		t.Helper()
		for name, v := range launcherVerifiers(t) {
			if strings.HasPrefix(name, "a ") {
				if v != nil {
					t.Errorf("%s built a verifier it does not use", name)
				}
				continue
			}
			if v == nil {
				t.Errorf("%s: no verifier", name)
				continue
			}
			if _, err := v.Verify(accepts); err != nil {
				t.Errorf("%s refused a capability for its audience: %v", name, err)
			}
			if _, err := v.Verify(refuses); !errors.Is(err, runcap.ErrWrongAudience) {
				t.Errorf("%s: a capability for another audience gave %v, want ErrWrongAudience", name, err)
			}
		}
	}
	check(byDefault, byInstall)

	t.Setenv("MCP_CAPABILITY_AUDIENCE", "an-installs-plane")
	check(byInstall, byDefault)
}

// capabilityVerifierFromEnv is the one place the launcher builds a run-capability verifier, so a new
// path cannot build one with its own reading of the audience.
func TestOneCapabilityVerifierConstructor(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		enclosing := "(package level)"
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				enclosing = fn.Name.Name
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewVerifier" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "runcap" {
					callers = append(callers, name+":"+enclosing)
				}
			}
			return true
		})
	}
	if !slices.Equal(callers, []string{"capability_verifier.go:capabilityVerifierFromEnv"}) {
		t.Errorf("runcap.NewVerifier is called from %v; build verifiers with capabilityVerifierFromEnv", callers)
	}
}
