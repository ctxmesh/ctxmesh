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

package sdkconformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// World describes the environment the goldens are true in. The launcher is real; Valkey, the token
// service, the BFF, Langfuse and the AMP peer are stubbed at the network edge with canned answers.
type World struct {
	Description string `json:"description,omitempty"`
	// Capability is the run capability the goldens and probes present.
	Capability string `json:"capability"`
	// ConversationID is the CONVERSATION_ID the SDKs run with.
	ConversationID string `json:"conversationId"`
	// LauncherEnv is the launcher's environment, minus the listener ports. ${name} placeholders name
	// the stubs: redis, tokenService, bff, langfuse, skillDir, podToken.
	LauncherEnv map[string]string `json:"launcherEnv"`
	// Skills are the mounted skill bodies, by name.
	Skills map[string]string `json:"skills"`
	// Tools is the mounted tool manifest; ToolsAfterChange replaces it when a golden needs a change.
	Tools            json.RawMessage `json:"tools"`
	ToolsAfterChange json.RawMessage `json:"toolsAfterChange"`
	// SessionMemory seeds Valkey before every golden and every probe run, by conversation id.
	SessionMemory map[string][]json.RawMessage `json:"sessionMemory"`
	// Upstreams are the canned answers of each stub, keyed "METHOD /path". ${peer} is the peer's URL.
	Upstreams map[string]map[string]Canned `json:"upstreams"`
}

// Canned is one stubbed upstream answer.
type Canned struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Upstream names, as the contract's world.upstreams keys them.
const (
	UpstreamTokenService = "tokenService"
	UpstreamBFF          = "bff"
	UpstreamLangfuse     = "langfuse"
	UpstreamPeer         = "peer"
)

// Stubs is a running World.
type Stubs struct {
	spec   World
	redis  *miniredis.Miniredis
	dir    string
	urls   map[string]string
	stubs  map[string]*stub
	closed sync.Once
}

// StartWorld starts every stub the World names. dir holds the skill mount and the pod token.
func StartWorld(spec World, dir string) (*Stubs, error) {
	s := &Stubs{spec: spec, dir: dir, urls: map[string]string{}, stubs: map[string]*stub{}}
	r, err := miniredis.Run()
	if err != nil {
		return nil, fmt.Errorf("start the Valkey stub: %w", err)
	}
	s.redis = r
	// The peer starts first: the BFF's attestation answer carries its URL.
	for _, name := range []string{UpstreamPeer, UpstreamTokenService, UpstreamBFF, UpstreamLangfuse} {
		canned, ok := spec.Upstreams[name]
		if !ok {
			s.Close()
			return nil, fmt.Errorf("world.upstreams has no %q", name)
		}
		st, err := startStub(name, canned, map[string]string{"${peer}": s.urls[UpstreamPeer]})
		if err != nil {
			s.Close()
			return nil, err
		}
		s.stubs[name], s.urls[name] = st, st.url
	}
	if err := s.writeFiles(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.Reset(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Stubs) writeFiles() error {
	skillDir := filepath.Join(s.dir, "skills")
	for _, d := range []string{skillDir, filepath.Dir(s.toolsPath())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for name, body := range s.spec.Skills {
		if err := os.WriteFile(filepath.Join(skillDir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(s.toolsPath(), s.spec.Tools, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "pod-token"), []byte("conformance-pod-token\n"), 0o600)
}

func (s *Stubs) toolsPath() string { return filepath.Join(s.dir, "tools", "tools.json") }

// ChangeTools replaces the mounted tool manifest with ToolsAfterChange, the way a binding change
// reaches a running agent.
func (s *Stubs) ChangeTools() error {
	return os.WriteFile(s.toolsPath(), s.spec.ToolsAfterChange, 0o600)
}

// Env is the launcher's environment with every placeholder resolved. The listener ports are the
// caller's to add.
func (s *Stubs) Env() (map[string]string, error) {
	repl := map[string]string{
		"${redis}":        s.redis.Addr(),
		"${tokenService}": s.urls[UpstreamTokenService],
		"${bff}":          s.urls[UpstreamBFF],
		"${langfuse}":     s.urls[UpstreamLangfuse],
		"${skillDir}":     filepath.Join(s.dir, "skills"),
		"${podToken}":     filepath.Join(s.dir, "pod-token"),
		"${toolsJson}":    s.toolsPath(),
	}
	out := make(map[string]string, len(s.spec.LauncherEnv))
	for k, v := range s.spec.LauncherEnv {
		for from, to := range repl {
			v = strings.ReplaceAll(v, from, to)
		}
		if strings.Contains(v, "${") {
			return nil, fmt.Errorf("world.launcherEnv.%s has an unknown placeholder: %s", k, v)
		}
		out[k] = v
	}
	return out, nil
}

// Reset returns Valkey to the seeded state, so every golden and every probe run starts the same.
func (s *Stubs) Reset() error {
	s.redis.FlushAll()
	ns := s.spec.LauncherEnv["MEMORY_KEY_NAMESPACE"]
	if ns == "" {
		ns = s.spec.LauncherEnv["POD_NAMESPACE"]
	}
	agent := s.spec.LauncherEnv["AGENT_NAME"]
	for _, conv := range slices.Sorted(maps.Keys(s.spec.SessionMemory)) {
		key := fmt.Sprintf("mem:%s/%s:%s", ns, agent, conv)
		for _, e := range s.spec.SessionMemory[conv] {
			compact, err := compactJSON(e)
			if err != nil {
				return fmt.Errorf("world.sessionMemory.%s: %w", conv, err)
			}
			if _, err := s.redis.Push(key, compact); err != nil {
				return err
			}
		}
	}
	for _, st := range s.stubs {
		st.reset()
	}
	return os.WriteFile(s.toolsPath(), s.spec.Tools, 0o600)
}

// Received returns the requests a stub answered since the last Reset, as "METHOD /path".
func (s *Stubs) Received(upstream string) []string {
	if st := s.stubs[upstream]; st != nil {
		return st.received()
	}
	return nil
}

// Close stops every stub.
func (s *Stubs) Close() {
	s.closed.Do(func() {
		for _, st := range s.stubs {
			st.close()
		}
		if s.redis != nil {
			s.redis.Close()
		}
	})
}

type stub struct {
	name   string
	url    string
	srv    *http.Server
	canned map[string]Canned
	mu     sync.Mutex
	seen   []string
}

func startStub(name string, canned map[string]Canned, repl map[string]string) (*stub, error) {
	resolved := make(map[string]Canned, len(canned))
	for k, c := range canned {
		body := string(c.Body)
		for from, to := range repl {
			body = strings.ReplaceAll(body, from, to)
		}
		resolved[k] = Canned{Status: c.Status, Body: json.RawMessage(body)}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start the %s stub: %w", name, err)
	}
	st := &stub{name: name, url: "http://" + ln.Addr().String(), canned: resolved}
	st.srv = &http.Server{Handler: st, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := st.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "sdkconformance: %s stub: %v\n", name, err)
		}
	}()
	return st, nil
}

func (st *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	st.mu.Lock()
	st.seen = append(st.seen, r.Method+" "+r.URL.Path)
	st.mu.Unlock()
	for key, c := range st.canned {
		method, pattern, _ := strings.Cut(key, " ")
		if method == r.Method && (Route{Path: pattern}).MatchesPath(r.URL.Path) {
			if len(c.Body) > 0 {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(c.Status)
			_, _ = w.Write(c.Body)
			return
		}
	}
	http.Error(w, st.name+" stub: no canned answer for "+r.Method+" "+r.URL.Path, http.StatusNotFound)
}

func (st *stub) reset() {
	st.mu.Lock()
	st.seen = nil
	st.mu.Unlock()
}

func (st *stub) received() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.seen)
}

func (st *stub) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = st.srv.Shutdown(ctx)
}

func compactJSON(raw json.RawMessage) (string, error) {
	v, err := Decode(raw)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}
