package ctxmesh

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A fake launcher built from sdk/launcher-routes.json — the SAME fixture generated from
// cmd/launcher — that 404s anything it does not recognise.
//
// The previous fakes were permissive catch-alls written by the author of the client they test,
// so both encoded the same wrong assumption and every test passed while 8 of 10 routes failed
// against a real launcher. A fake that answers whatever it is asked cannot catch a wrong path;
// this one refuses, so a client calling `/memory/agent` (which the launcher does not serve)
// fails here exactly as it would in production.

type route struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Listener    string `json:"listener"`
	PortEnv     string `json:"portEnv"`
	DefaultPort int    `json:"defaultPort"`
}

func loadRoutes() ([]route, error) {
	// Walk up to the repo root: the fixture is shared by all six SDKs, so it lives beside them.
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "launcher-routes.json")
		if b, err := os.ReadFile(p); err == nil {
			var f struct {
				Routes []route `json:"routes"`
			}
			if err := json.Unmarshal(b, &f); err != nil {
				return nil, err
			}
			return f.Routes, nil
		}
		dir = filepath.Dir(dir)
	}
	return nil, os.ErrNotExist
}

// matches reports whether a concrete request path matches a route pattern, treating {param} as
// exactly one segment. `/memory/{conversationId}` must NOT match `/memory/agent/remember`.
func (r route) matches(method, path string) bool {
	if r.Method != "ANY" && r.Method != method {
		return false
	}
	want := strings.Split(strings.Trim(r.Path, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if strings.HasPrefix(want[i], "{") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// fakePlane records every request it accepts and refuses everything else.
type fakePlane struct {
	mu     sync.Mutex
	routes []route
	seen   map[string]bool // "METHOD /pattern" — the ROUTE matched, not the concrete path
	reqs   []string
	status int
}

func newFakePlane(routes []route) *fakePlane {
	return &fakePlane{routes: routes, seen: map[string]bool{}, status: 200}
}

func (f *fakePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	var hit *route
	for i := range f.routes {
		if f.routes[i].matches(r.Method, r.URL.Path) {
			hit = &f.routes[i]
			break
		}
	}
	if hit != nil {
		f.seen[hit.Method+" "+hit.Path] = true
	}
	st := f.status
	f.mu.Unlock()

	if hit == nil {
		// Exactly what a real launcher does, and the whole point of this fake.
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_, _ = w.Write([]byte(bodyFor(hit.Path)))
}

// bodyFor mirrors the shapes the real handlers return — verified against cmd/launcher, not
// against what the client happens to expect.
func bodyFor(pattern string) string {
	switch pattern {
	case "/memory/agent/search":
		return `{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}`
	case "/knowledge/search":
		return `{"results":[{"content":"c","documentRef":"d","score":0.5}]}`
	case "/skills":
		return `{"skills":[{"name":"s","description":"d"}]}`
	case "/skills/load":
		return `{"body":"the skill body"}` // "body", not "content"
	case "/delegate":
		return `{"ok":true,"subAgent":"sub","subRun":"r1","answer":"42"}`
	case "/handoff":
		return `{"ok":true,"runId":"r1","handedOffTo":"other"}`
	case "/memory/{conversationId}", "/memory/{conversationId}/search":
		return `[{"role":"user","content":"hi"}]`
	default:
		return `{}`
	}
}

func (f *fakePlane) covered(pattern, method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[method+" "+pattern]
}

func (f *fakePlane) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}
