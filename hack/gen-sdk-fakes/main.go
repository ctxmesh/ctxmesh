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

// Command gen-sdk-fakes writes every SDK's test fake of the launcher from sdk/launcher-routes.json.
//
// Each fake serves exactly the contract's routes, answers with the golden responses cmd/launcher
// proved true, and records every request that breaks a route's schema. Hand-written fakes were
// written by the author of the client they tested, so both could be wrong the same way; a generated
// one can only be as wrong as the contract, and the contract is checked against the real launcher.
//
// Usage:
//
//	go run ./hack/gen-sdk-fakes          rewrite all six fakes
//	go run ./hack/gen-sdk-fakes -check   fail, naming the file, if a committed fake is stale
//	go run ./hack/gen-sdk-fakes -lang go,ruby
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ctxmesh/ctxmesh/test/sdkconformance"
)

//go:embed templates/*.tmpl
var templates embed.FS

// target is one SDK's generated fake: its template and where the output lands.
type target struct {
	lang    string
	out     string
	literal func(contractJSON string) (string, error)
}

var targets = []target{
	{"go", "sdk/go/zz_generated_fakelauncher_test.go", goLiteral},
	{"python", "sdk/python/tests/_generated_fake_launcher.py", pythonLiteral},
	{"typescript", "sdk/typescript/test/generated/fake-launcher.ts", typescriptLiteral},
	{"java", "sdk/java/src/test/java/ai/ctxmesh/GeneratedFakeLauncher.java", javaLiteral},
	{"rust", "sdk/rust/tests/fake_launcher/mod.rs", rustLiteral},
	{"ruby", "sdk/ruby/test/generated_fake_launcher.rb", rubyLiteral},
}

const placeholder = "__CONTRACT__"

func main() {
	check := flag.Bool("check", false, "fail if a committed fake differs from what the contract generates")
	langs := flag.String("lang", "", "comma-separated languages (default: all)")
	flag.Parse()

	wd, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	contract, root, err := sdkconformance.LoadFromRepo(wd)
	if err != nil {
		fail(err)
	}
	if problems := contract.Check(); len(problems) > 0 {
		fail(fmt.Errorf("the contract disagrees with itself; fix it before generating fakes from it:\n  %s",
			strings.Join(problems, "\n  ")))
	}
	want := map[string]bool{}
	for l := range strings.SplitSeq(*langs, ",") {
		if l = strings.TrimSpace(l); l != "" {
			want[l] = true
		}
	}
	stale := 0
	for _, tg := range targets {
		if len(want) > 0 && !want[tg.lang] {
			continue
		}
		content, err := render(contract, root, tg)
		if err != nil {
			fail(fmt.Errorf("%s: %w", tg.lang, err))
		}
		path := filepath.Join(root, tg.out)
		if *check {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, content) {
				fmt.Fprintf(os.Stderr, "FAIL: %s is stale or missing; run make sdk-fakes\n", tg.out)
				stale++
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // a source file, world-readable
			fail(err)
		}
		fmt.Printf("wrote %s\n", tg.out)
	}
	if stale > 0 {
		os.Exit(1)
	}
	if *check {
		fmt.Println("PASS: every SDK's fake matches sdk/launcher-routes.json")
	}
}

func render(c *sdkconformance.Contract, root string, tg target) ([]byte, error) {
	tmpl, err := templates.ReadFile("templates/" + tg.lang + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("no template: %w", err)
	}
	if !bytes.Contains(tmpl, []byte(placeholder)) {
		return nil, fmt.Errorf("template has no %s placeholder", placeholder)
	}
	tier, err := sdkconformance.SDKTier(root, tg.lang)
	if err != nil {
		return nil, err
	}
	data, err := fakeContract(c, tg.lang, tier)
	if err != nil {
		return nil, err
	}
	lit, err := tg.literal(data)
	if err != nil {
		return nil, err
	}
	return bytes.Replace(tmpl, []byte(placeholder), []byte(lit), 1), nil
}

// fakeContract is what a fake needs from the contract, for one language: the routes with their
// schemas and goldens, descriptions dropped, and "waived" set where that SDK does not owe a route,
// by a waiver or by its tier.
func fakeContract(c *sdkconformance.Contract, lang, tier string) (string, error) {
	raw, err := json.Marshal(c.Routes)
	if err != nil {
		return "", err
	}
	var routes []map[string]any
	if err := json.Unmarshal(raw, &routes); err != nil {
		return "", err
	}
	for i, r := range routes {
		delete(r, "summary")
		delete(r, "portEnv")
		delete(r, "defaultPort")
		r["key"] = c.Routes[i].Key()
		r["servedMethod"] = c.Routes[i].ServedMethod()
		delete(r, "sdkWaivers")
		delete(r, "sdkResult")
		delete(r, "tier")
		if why, _ := c.Routes[i].Exemption(lang, tier); why != "" {
			r["waived"] = why
		}
		stripRequest(r["request"])
		if resps, ok := r["responses"].(map[string]any); ok {
			for _, v := range resps {
				if m, ok := v.(map[string]any); ok {
					delete(m, "description")
					stripSchema(m["schema"])
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"language": lang, "routes": routes}); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func stripRequest(v any) {
	req, ok := v.(map[string]any)
	if !ok {
		return
	}
	stripSchema(req["body"])
	for _, k := range []string{"params", "query"} {
		if m, ok := req[k].(map[string]any); ok {
			for _, s := range m {
				stripSchema(s)
			}
		}
	}
	if hs, ok := req["headers"].(map[string]any); ok {
		for _, h := range hs {
			if m, ok := h.(map[string]any); ok {
				delete(m, "description")
			}
		}
	}
}

// stripSchema drops the description keyword from a schema and its subschemas, never a property
// that happens to be named "description".
func stripSchema(v any) {
	s, ok := v.(map[string]any)
	if !ok {
		return
	}
	delete(s, "description")
	if props, ok := s["properties"].(map[string]any); ok {
		for _, p := range props {
			stripSchema(p)
		}
	}
	stripSchema(s["items"])
	stripSchema(s["additionalProperties"])
}

func goLiteral(j string) (string, error) {
	if strings.Contains(j, "`") {
		return "", fmt.Errorf("the contract contains a backtick, which a Go raw string cannot hold")
	}
	return "`" + j + "`", nil
}

func pythonLiteral(j string) (string, error) {
	if strings.Contains(j, "'''") {
		return "", fmt.Errorf("the contract contains ''' which a Python raw string cannot hold")
	}
	return "r'''" + j + "'''", nil
}

// typescriptLiteral is the JSON itself: JSON is a valid TypeScript expression.
func typescriptLiteral(j string) (string, error) { return j, nil }

// javaLiteral splits the JSON into text blocks under the class file's 64 KiB constant limit, with
// backslashes doubled because a text block interprets escapes.
func javaLiteral(j string) (string, error) {
	if strings.Contains(j, `"""`) {
		return "", fmt.Errorf(`the contract contains """ which a Java text block cannot hold`)
	}
	escaped := strings.ReplaceAll(j, `\`, `\\`)
	var parts []string
	var cur strings.Builder
	for line := range strings.SplitSeq(escaped, "\n") {
		if cur.Len()+len(line) > 30000 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
		cur.WriteString(line + "\n")
	}
	parts = append(parts, cur.String())
	blocks := make([]string, 0, len(parts))
	for _, p := range parts {
		blocks = append(blocks, "\"\"\"\n"+p+"\"\"\"")
	}
	return "String.join(\"\",\n" + strings.Join(blocks, ",\n") + ")", nil
}

func rustLiteral(j string) (string, error) {
	if strings.Contains(j, `"###`) {
		return "", fmt.Errorf(`the contract contains "### which a Rust raw string cannot hold`)
	}
	return `r###"` + j + `"###`, nil
}

func rubyLiteral(j string) (string, error) {
	if slices.ContainsFunc(strings.Split(j, "\n"), func(l string) bool { return strings.TrimSpace(l) == "CONTRACT" }) {
		return "", fmt.Errorf("the contract contains a line that would end the Ruby heredoc")
	}
	return "<<~'CONTRACT'\n" + j + "\nCONTRACT", nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen-sdk-fakes:", err)
	os.Exit(1)
}
