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

// Command sdk-conformance drives every SDK, through its public API, against a real launcher built
// from this tree, with only the launcher's upstreams stubbed. It prints one verdict line per
// (language, route) and exits non-zero if any line is FAIL.
//
// A missing toolchain is a FAIL naming the toolchain, never a skip: the four plane-client SDKs once
// shipped with 8 of 10 routes broken because the one check that would have caught it was optional.
//
// Usage:
//
//	go run ./hack/sdk-conformance [-lang go,ruby] [-python PATH] [-ruby PATH] [-node PATH]
package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ctxmesh/ctxmesh/test/sdkconformance"
)

// Fixed ports. The Python SDK posts /delegate and /handoff to 127.0.0.1:2994 whatever DELEGATE_PORT
// says, and the launcher always binds tool discovery on 2999, so the run must own both.
const (
	fixedDelegatePort  = 2994
	fixedDiscoveryPort = 2999
	delegatePortEnv    = "DELEGATE_PORT"
)

// The listeners a tap fronts, by the port env each one's SDK-facing port goes into. Discovery is
// not tapped: the launcher binds its fixed port itself.
var tappedListeners = []struct{ name, portEnv string }{
	{"memory", "MEMORY_PORT"}, {"amp", "A2A_PORT"}, {"delegate", delegatePortEnv}, {"feedback", "FEEDBACK_PORT"},
}

type options struct {
	langs  []string
	python string
	ruby   string
	node   string
}

func main() {
	var o options
	langs := flag.String("lang", strings.Join(sdkconformance.Languages, ","), "comma-separated languages to probe")
	flag.StringVar(&o.python, "python", "", "Python interpreter (default: sdk/python/.venv/bin/python, else python3)")
	flag.StringVar(&o.ruby, "ruby", "", "Ruby interpreter (default: the first ruby >= 3.1 found)")
	flag.StringVar(&o.node, "node", "", "Node.js binary (default: node on PATH)")
	flag.Parse()
	for l := range strings.SplitSeq(*langs, ",") {
		if l = strings.TrimSpace(l); l != "" {
			if !slices.Contains(sdkconformance.Languages, l) {
				fatalf("unknown language %q (have %s)", l, strings.Join(sdkconformance.Languages, ", "))
			}
			o.langs = append(o.langs, l)
		}
	}
	os.Exit(run(o))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sdk-conformance: "+format+"\n", args...)
	os.Exit(2)
}

func run(o options) int {
	wd, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	contract, root, err := sdkconformance.LoadFromRepo(wd)
	if err != nil {
		fatalf("%v", err)
	}
	if problems := contract.Check(); len(problems) > 0 {
		fatalf("the contract disagrees with itself:\n  %s", strings.Join(problems, "\n  "))
	}
	unlock := lock()
	defer unlock()
	extendPath()

	work, err := os.MkdirTemp("", "sdk-conformance-")
	if err != nil {
		fatalf("%v", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	world, err := sdkconformance.StartWorld(contract.World, filepath.Join(work, "world"))
	if err != nil {
		fatalf("%v", err)
	}
	defer world.Close()

	l, err := startLauncher(root, work, contract, world)
	if err != nil {
		fatalf("%v", err)
	}
	defer l.stop()

	results := map[string][]verdict{}
	for _, lang := range o.langs {
		if err := world.Reset(); err != nil {
			fatalf("%v", err)
		}
		l.drainTaps()
		results[lang] = probe(context.Background(), lang, o, root, work, contract, world, l)
		for _, v := range results[lang] {
			fmt.Println(v.line(lang))
		}
	}
	return summarise(o.langs, contract, results)
}

// lock serialises runs on one machine: two would fight over the fixed ports.
func lock() func() {
	path := filepath.Join(os.TempDir(), "ctxmesh-sdk-conformance.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fatalf("open %s: %v", path, err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		if time.Now().After(deadline) {
			fatalf("another sdk-conformance run has held %s for 10 minutes", path)
		}
		time.Sleep(time.Second)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// extendPath adds the places language toolchains install outside the default PATH on a developer
// machine, so a toolchain that is installed is found rather than reported missing.
func extendPath() {
	home, _ := os.UserHomeDir()
	extra := []string{filepath.Join(home, ".cargo", "bin"), "/opt/homebrew/opt/openjdk/bin", "/opt/homebrew/bin"}
	path := os.Getenv("PATH")
	for _, d := range extra {
		if st, err := os.Stat(d); err == nil && st.IsDir() && !slices.Contains(filepath.SplitList(path), d) {
			path += string(os.PathListSeparator) + d
		}
	}
	_ = os.Setenv("PATH", path)
}

// launcher is the real launcher process and the taps in front of its listeners.
type launcher struct {
	cmd  *exec.Cmd
	log  string
	taps map[string]*tap
	// sdkPorts are the port env values the SDKs get: each tap's port.
	sdkPorts map[string]string
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func startLauncher(root, work string, c *sdkconformance.Contract, world *sdkconformance.Stubs) (*launcher, error) {
	bin := filepath.Join(work, "launcher")
	build := exec.Command("go", "build", "-o", bin, "./cmd/launcher")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build cmd/launcher: %v\n%s", err, out)
	}
	agent := filepath.Join(work, "agent.sh")
	script := []byte("#!/bin/sh\nexec sleep 86400\n")
	if err := os.WriteFile(agent, script, 0o700); err != nil { //nolint:gosec // the launcher must exec it
		return nil, err
	}
	env, err := world.Env()
	if err != nil {
		return nil, err
	}
	ports := map[string]int{}
	portEnvs := []string{"MEMORY_PORT", "A2A_PORT", delegatePortEnv, "FEEDBACK_PORT", "AGENT_PORT", "AGENT_UPSTREAM_PORT"}
	for _, k := range portEnvs {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		ports[k] = p
		env[k] = fmt.Sprint(p)
	}
	env["AGENT_ENTRYPOINT"] = agent
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "127.0.0.1:1"
	l := &launcher{log: filepath.Join(work, "launcher.log"), taps: map[string]*tap{}, sdkPorts: map[string]string{}}
	logf, err := os.Create(l.log)
	if err != nil {
		return nil, err
	}
	l.cmd = exec.Command(bin) //nolint:gosec // the binary this run just built
	l.cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, envList(env)...)
	l.cmd.Stdout, l.cmd.Stderr = logf, logf
	l.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := l.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the launcher: %w", err)
	}
	if err := l.awaitReady(ports); err != nil {
		l.stop()
		return nil, err
	}
	for _, tl := range tappedListeners {
		want := 0
		if tl.portEnv == delegatePortEnv {
			want = fixedDelegatePort
		}
		t, err := startTap(tl.name, want, ports[tl.portEnv], c)
		if err != nil {
			l.stop()
			return nil, fmt.Errorf("the %s tap cannot bind 127.0.0.1:%d (the Python SDK hardcodes it): %w", tl.name, want, err)
		}
		l.taps[tl.name] = t
		l.sdkPorts[tl.portEnv] = fmt.Sprint(t.port)
	}
	return l, nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// awaitReady waits for every listener, including discovery on its fixed port. A launcher that lost
// the discovery port to another process logs the bind failure and carries on, so readiness checks
// that 2999 answers as this launcher's discovery listener.
func (l *launcher) awaitReady(ports map[string]int) error {
	urls := []string{
		fmt.Sprintf("http://127.0.0.1:%d/healthz", ports["MEMORY_PORT"]),
		fmt.Sprintf("http://127.0.0.1:%d/healthz", ports["A2A_PORT"]),
		fmt.Sprintf("http://127.0.0.1:%d/healthz", ports[delegatePortEnv]),
		fmt.Sprintf("http://127.0.0.1:%d/tools", fixedDiscoveryPort),
	}
	deadline := time.Now().Add(20 * time.Second)
	hc := &http.Client{Timeout: time.Second}
	for _, u := range urls {
		for {
			resp, err := hc.Get(u)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				tail, _ := os.ReadFile(l.log)
				return fmt.Errorf("the launcher never answered %s (is the port taken?):\n%s", u, lastLines(string(tail), 20))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ports["FEEDBACK_PORT"]), 2*time.Second)
	if err != nil {
		return fmt.Errorf("the feedback listener never opened: %w", err)
	}
	return conn.Close()
}

func (l *launcher) drainTaps() map[string][]observed {
	out := map[string][]observed{}
	for name, t := range l.taps {
		out[name] = t.drain()
	}
	return out
}

func (l *launcher) stop() {
	for _, t := range l.taps {
		t.close()
	}
	if l.cmd == nil || l.cmd.Process == nil {
		return
	}
	// The launcher forwards SIGTERM to its child and exits; the group kill covers a child that does not.
	_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = l.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
