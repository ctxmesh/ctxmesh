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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/ctxmesh/test/sdkconformance"
)

const (
	buildTimeout = 10 * time.Minute
	probeTimeout = 3 * time.Minute
	probesDir    = "test/sdkconformance/probes"
)

// Verdicts. WAIVED is a recorded gap with a card, reported so it stays visible; EXEMPT is a route
// the SDK's tier does not owe. Neither fails the run, and neither is ever counted as PASS.
const (
	pass   = "PASS"
	fail   = "FAIL"
	waived = "WAIVED"
	// exempt is a route the SDK's tier does not owe (ADR 0139).
	exempt = "EXEMPT"
	// absent is what a probe prints for a route its SDK has no call for.
	absent = "MISSING"
)

type verdict struct {
	route, status, reason string
}

func (v verdict) line(lang string) string {
	if v.reason == "" {
		return fmt.Sprintf("%-6s %-10s %s", v.status, lang, v.route)
	}
	return fmt.Sprintf("%-6s %-10s %s: %s", v.status, lang, v.route, v.reason)
}

// probeLine is one line a probe printed: "PASS|FAIL|MISSING <METHOD> <path>[: reason]".
type probeLine struct {
	kind, reason string
}

// toolchainError is a toolchain or prerequisite the probe needs and the machine lacks.
type toolchainError struct{ msg string }

func (e toolchainError) Error() string { return e.msg }

func missing(format string, args ...any) error {
	return toolchainError{"toolchain missing: " + fmt.Sprintf(format, args...)}
}

func probe(
	ctx context.Context, lang string, o options, root, work string,
	c *sdkconformance.Contract, world *sdkconformance.Stubs, l *launcher,
) []verdict {
	cmd, err := prepare(ctx, lang, o, root, work)
	if err != nil {
		return failAll(c, err.Error())
	}
	env, err := world.Env()
	if err != nil {
		return failAll(c, err.Error())
	}
	env["CONVERSATION_ID"] = c.World.ConversationID
	env["CTXMESH_PROBE_CONTRACT"] = filepath.Join(root, sdkconformance.File)
	env["CTXMESH_PROBE_CAPABILITY"] = c.World.Capability
	maps.Copy(env, l.sdkPorts)
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	cmd.Env = append(base, envList(env)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	runErr := runWithContext(runCtx, cmd)
	lines := parseProbe(stdout.String())
	obs := l.drainTaps()

	tier, err := sdkconformance.SDKTier(root, lang)
	if err != nil {
		return failAll(c, err.Error())
	}
	var out []verdict
	for _, r := range c.Routes {
		out = append(out, judge(lang, tier, r, lines[r.Key()], obs, l, runErr, stderr.String()))
	}
	for key := range lines {
		if c.RouteByKey(key) == nil {
			fmt.Printf("NOTE   %-10s the probe reported %q, which the contract does not declare\n", lang, key)
		}
	}
	for _, tl := range tappedListeners {
		for _, ob := range obs[tl.name] {
			if ob.route == "" {
				fmt.Printf("NOTE   %-10s the SDK called %s %s, which the launcher does not serve (%d)\n",
					lang, ob.method, ob.path, ob.status)
			}
		}
	}
	return out
}

func failAll(c *sdkconformance.Contract, reason string) []verdict {
	out := make([]verdict, 0, len(c.Routes))
	for _, r := range c.Routes {
		out = append(out, verdict{r.Key(), fail, reason})
	}
	return out
}

func judge(
	lang, tier string, r sdkconformance.Route, pl *probeLine, obs map[string][]observed, l *launcher,
	runErr error, stderr string,
) verdict {
	key := r.Key()
	why, isWaived := r.SDKWaivers[lang]
	if !isWaived && !r.OwedBy(tier) {
		if pl != nil && pl.kind == absent {
			reason, _ := r.Exemption(lang, tier)
			return verdict{key, exempt, reason}
		}
	}
	switch {
	case pl == nil:
		reason := "the probe printed no result for this route"
		if runErr != nil {
			reason += fmt.Sprintf(" (it exited: %v; %s)", runErr, oneLine(lastLines(stderr, 3)))
		}
		return verdict{key, fail, reason}
	case pl.kind == absent && isWaived:
		return verdict{key, waived, why}
	case pl.kind == absent:
		return verdict{key, fail, "no public SDK call reaches this route (" + pl.reason + ")"}
	case isWaived:
		return verdict{key, fail, "the SDK reaches this route now, so its waiver is stale: delete it from the contract"}
	case pl.kind != pass:
		return verdict{key, fail, pl.reason + launcherSaw(r, obs)}
	}
	if _, tapped := l.taps[r.Listener]; !tapped {
		return verdict{key, pass, ""}
	}
	var seen []observed
	for _, tl := range tappedListeners {
		for _, ob := range obs[tl.name] {
			if ob.route == key {
				seen = append(seen, ob)
			}
		}
	}
	if len(seen) == 0 {
		return verdict{key, fail, "the SDK reported success but the launcher never received " + key + launcherSaw(r, obs)}
	}
	want := r.Goldens[0].Response.Status
	ok := false
	for _, ob := range seen {
		if len(ob.violations) > 0 {
			return verdict{key, fail, "the request broke the contract: " + strings.Join(ob.violations, "; ")}
		}
		ok = ok || ob.status == want
	}
	if !ok {
		return verdict{key, fail, fmt.Sprintf("the launcher never answered %d%s", want, launcherSaw(r, obs))}
	}
	return verdict{key, pass, ""}
}

// launcherSaw describes what reached the launcher for a route, for a failure message.
func launcherSaw(r sdkconformance.Route, obs map[string][]observed) string {
	var parts []string
	for _, tl := range tappedListeners {
		for _, ob := range obs[tl.name] {
			if ob.route == r.Key() || (ob.route == "" && r.MatchesPath(ob.path)) {
				parts = append(parts, fmt.Sprintf("%s %s -> %d", ob.method, ob.path, ob.status))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " [launcher saw: " + strings.Join(parts, ", ") + "]"
}

func parseProbe(stdout string) map[string]*probeLine {
	out := map[string]*probeLine{}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		kind, rest, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || (kind != pass && kind != fail && kind != absent) {
			continue
		}
		key, reason, _ := strings.Cut(rest, ": ")
		key = strings.TrimSpace(key)
		if prev, dup := out[key]; dup && prev.kind != pass {
			continue // the first failure for a route is the one worth reading
		}
		out[key] = &probeLine{kind: kind, reason: reason}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func runWithContext(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", probeTimeout)
	}
}

// prepare builds what a language's probe needs and returns the command that runs it.
func prepare(ctx context.Context, lang string, o options, root, work string) (*exec.Cmd, error) {
	dir := filepath.Join(root, probesDir, lang)
	switch lang {
	case "go":
		return prepareGo(ctx, dir, work)
	case "python":
		return preparePython(o, root, dir)
	case "typescript":
		return prepareTypeScript(o, root, dir)
	case "java":
		return prepareJava(ctx, root, dir, work)
	case "rust":
		return prepareRust(ctx, dir)
	case "ruby":
		return prepareRuby(o, root, dir)
	}
	return nil, fmt.Errorf("no probe for %s", lang)
}

func build(ctx context.Context, cmd *exec.Cmd) error {
	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := runWithContext(bctx, cmd); err != nil {
		return fmt.Errorf("the probe did not build: %v: %s", err, oneLine(lastLines(out.String(), 6)))
	}
	return nil
}

func prepareGo(ctx context.Context, dir, work string) (*exec.Cmd, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, missing("go (not on PATH)")
	}
	bin := filepath.Join(work, "go-probe")
	b := exec.Command("go", "build", "-o", bin, ".")
	b.Dir = dir
	b.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if err := build(ctx, b); err != nil {
		return nil, err
	}
	return exec.Command(bin), nil //nolint:gosec // the probe this run just built
}

func preparePython(o options, root, dir string) (*exec.Cmd, error) {
	py := o.python
	if py == "" {
		py = "python3"
		if venv := filepath.Join(root, "sdk/python/.venv/bin/python"); fileExists(venv) {
			py = venv
		}
	}
	if _, err := exec.LookPath(py); err != nil {
		return nil, missing("python >= 3.9 (%s not found)", py)
	}
	ver, err := output(py, "-c", "import sys; print('%d.%d' % sys.version_info[:2])")
	if err != nil || !atLeast(ver, 3, 9) {
		return nil, missing("python >= 3.9 (%s reports %q)", py, ver)
	}
	src := filepath.Join(root, "sdk/python/src")
	imp := exec.Command(py, "-c", "import ctxmesh") //nolint:gosec // the interpreter the caller chose
	imp.Env = append(os.Environ(), "PYTHONPATH="+src)
	if out, err := imp.CombinedOutput(); err != nil {
		return nil, toolchainError{"the Python SDK does not import with " + py + " (run make py-venv): " +
			oneLine(lastLines(string(out), 2))}
	}
	cmd := exec.Command(py, filepath.Join(dir, "probe.py")) //nolint:gosec // the interpreter the caller chose
	cmd.Env = append(os.Environ(), "PYTHONPATH="+src)
	return cmd, nil
}

func prepareTypeScript(o options, root, dir string) (*exec.Cmd, error) {
	node := o.node
	if node == "" {
		node = "node"
	}
	if _, err := exec.LookPath(node); err != nil {
		return nil, missing("node >= 20 (%s not found on PATH)", node)
	}
	ver, err := output(node, "-p", "process.versions.node")
	if err != nil || !atLeast(ver, 20, 0) {
		return nil, missing("node >= 20 (%s reports %q)", node, ver)
	}
	dist := filepath.Join(root, "sdk/typescript/dist/esm/index.js")
	if _, err := os.Stat(dist); err != nil {
		return nil, toolchainError{"sdk/typescript/dist is not built: run make sdk-ts-build"}
	}
	// tsc -b rewrites only the outputs whose sources changed, so compare against the newest output,
	// not index.js, which a build that did not touch index.ts leaves old.
	ts := filepath.Join(root, "sdk/typescript")
	if newestUnder(filepath.Join(ts, "src")).After(newestUnder(filepath.Join(ts, "dist"))) {
		return nil, toolchainError{"sdk/typescript/dist is older than src, so the probe would test stale code: " +
			"run make sdk-ts-build"}
	}
	return exec.Command(node, filepath.Join(dir, "probe.mjs")), nil //nolint:gosec // the binary the caller chose
}

func prepareJava(ctx context.Context, root, dir, work string) (*exec.Cmd, error) {
	for _, tool := range []string{"javac", "java"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, missing("%s >= 17 (not on PATH)", tool)
		}
	}
	ver, err := output("javac", "-version")
	ver = strings.TrimPrefix(strings.TrimSpace(ver), "javac ")
	if err != nil || !atLeast(ver, 17, 0) {
		return nil, missing("javac >= 17 (javac reports %q)", ver)
	}
	var sources []string
	_ = filepath.WalkDir(filepath.Join(root, "sdk/java/src/main/java"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".java") {
			sources = append(sources, p)
		}
		return nil
	})
	sources = append(sources, filepath.Join(dir, "ConformanceProbe.java"))
	classes := filepath.Join(work, "java-classes")
	args := append([]string{"-d", classes, "-encoding", "UTF-8", "--release", "17"}, sources...)
	if err := build(ctx, exec.Command("javac", args...)); err != nil {
		return nil, err
	}
	return exec.Command("java", "-cp", classes, "ai.ctxmesh.ConformanceProbe"), nil
}

func prepareRust(ctx context.Context, dir string) (*exec.Cmd, error) {
	if _, err := exec.LookPath("cargo"); err != nil {
		return nil, missing("cargo (not on PATH or in ~/.cargo/bin)")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	target := filepath.Join(cache, "ctxmesh-sdk-conformance", "rust")
	b := exec.Command("cargo", "build", "--quiet", "--manifest-path", filepath.Join(dir, "Cargo.toml"))
	b.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
	if err := build(ctx, b); err != nil {
		return nil, err
	}
	return exec.Command(filepath.Join(target, "debug", "conformance-probe")), nil //nolint:gosec // just built
}

func prepareRuby(o options, root, dir string) (*exec.Cmd, error) {
	candidates := []string{o.ruby}
	if o.ruby == "" {
		candidates = []string{"ruby", "/opt/homebrew/opt/ruby/bin/ruby", "/usr/local/opt/ruby/bin/ruby"}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".rbenv/shims/ruby"))
		}
	}
	var found []string
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		ver, err := output(path, "-e", "print RUBY_VERSION")
		if err == nil && atLeast(ver, 3, 1) {
			return exec.Command(path, "-I", filepath.Join(root, "sdk/ruby/lib"), filepath.Join(dir, "probe.rb")), nil
		}
		found = append(found, fmt.Sprintf("%s at %s", ver, path))
	}
	if len(found) == 0 {
		return nil, missing("ruby >= 3.1, which ctxmesh.gemspec requires (no ruby found)")
	}
	return nil, missing("ruby >= 3.1, which ctxmesh.gemspec requires (found only %s; pass -ruby)",
		strings.Join(found, ", "))
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // toolchain probes
	return strings.TrimSpace(string(out)), err
}

// atLeast reports whether a dotted version is at least major.minor.
func atLeast(ver string, major, minor int) bool {
	parts := strings.FieldsFunc(ver, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == ' ' })
	if len(parts) == 0 {
		return false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	mnr := 0
	if len(parts) > 1 {
		mnr, _ = strconv.Atoi(parts[1])
	}
	return maj > major || (maj == major && mnr >= minor)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newestUnder(dir string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

func summarise(langs []string, c *sdkconformance.Contract, results map[string][]verdict) int {
	fmt.Println()
	fmt.Printf("%-10s %5s %5s %7s %7s\n", "language", "PASS", "FAIL", "WAIVED", "EXEMPT")
	failures := 0
	for _, lang := range langs {
		counts := map[string]int{}
		for _, v := range results[lang] {
			counts[v.status]++
		}
		failures += counts[fail]
		fmt.Printf("%-10s %5d %5d %7d %7d\n", lang, counts[pass], counts[fail], counts[waived], counts[exempt])
	}
	total := len(langs) * len(c.Routes)
	if failures > 0 {
		fmt.Printf("\nRESULT: FAIL (%d of %d language-route pairs failed)\n", failures, total)
		return 1
	}
	if len(langs) == 0 {
		fmt.Println("\nRESULT: FAIL (no language was probed)")
		return 1
	}
	fmt.Printf("\nRESULT: PASS (%d language-route pairs, none failed)\n", total)
	return 0
}
