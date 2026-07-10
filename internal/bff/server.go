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

package bff

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/go-logr/logr"
)

// defaultVersion is reported by /api/health when no version is injected at
// build time.
const defaultVersion = "dev"

// Server is the BFF HTTP server: it serves the static SPA build and the /api
// surface (behind the M11 auth). It composes narrow seams (an AgentReader for
// client-go, an Authenticator for M11 auth, optional Adapters for m12.5+) so
// each is independently testable.
type Server struct {
	reader   AgentReader
	auth     Authenticator
	adapters Adapters
	version  string
	log      logr.Logger

	// static is the filesystem serving the Vite build (dist/). Nil disables
	// static serving (api-only mode, useful in tests).
	static fs.FS
}

// Options configures a Server.
type Options struct {
	// Reader is the client-go-backed reader for the agent CRDs (required for the
	// /api/agents route). Typically the manager's client.Client.
	Reader AgentReader
	// Auth gates /api requests (the M11 control-plane auth seam). Required.
	Auth Authenticator
	// Adapters are the optional server-side adapters (Langfuse/Prometheus/invoke/
	// expand). Nil entries serve 501 for their routes (m12.5+ wires them).
	Adapters Adapters
	// Version is reported by /api/health.
	Version string
	// StaticDir is the directory of the built SPA (dist/). Empty disables static
	// serving; the SPA is then served elsewhere (e.g. an nginx sidecar).
	StaticDir string
	// Log is the structured logger.
	Log logr.Logger
}

// NewServer builds a Server from Options. It does not start listening; the
// caller mounts Handler() on an http.Server.
func NewServer(opts Options) *Server {
	s := &Server{
		reader:   opts.Reader,
		auth:     opts.Auth,
		adapters: opts.Adapters,
		version:  opts.Version,
		log:      opts.Log,
	}
	if s.version == "" {
		s.version = defaultVersion
	}
	if opts.StaticDir != "" {
		s.static = os.DirFS(opts.StaticDir)
	}
	return s
}

// Handler returns the fully-wired http.Handler: the /api mux (auth-gated) plus
// the SPA static handler as the fallback for everything else.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()

	// Health is unauthenticated (liveness/version probe; no cluster access).
	api.HandleFunc("GET /api/health", s.handleHealth)

	// Authenticated surface. /api/agents is the foundation proof (client-go).
	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/agents", s.handleListAgents)

	// Topology is client-go only (no external adapter) → always available.
	authed.HandleFunc("GET /api/topology", s.handleTopology)

	// Langfuse-backed dashboard routes (recent runs, cost/usage, trace link).
	// Wired when the Langfuse adapter is present; honest 501 otherwise so the
	// routes stay discoverable. Registering the real handler only when wired
	// keeps the nil-adapter seam clean.
	if s.adapters.Langfuse != nil {
		authed.HandleFunc("GET /api/runs", s.handleRuns)
		authed.HandleFunc("GET /api/cost", s.handleCost)
		authed.HandleFunc("GET /api/traces/{id}", s.handleTraceLink)
	} else {
		authed.Handle("GET /api/runs", notImplemented("Langfuse runs adapter"))
		authed.Handle("GET /api/cost", notImplemented("Langfuse cost adapter"))
		authed.Handle("GET /api/traces/", notImplemented("Langfuse trace adapter"))
	}

	// Remaining adapter seams for m12.6–m12.7: mounted now (discoverable) but
	// honest 501 until their adapter is wired.
	if s.adapters.Prometheus == nil {
		authed.Handle("GET /api/metrics/", notImplemented("Prometheus adapter"))
	}
	if s.adapters.Invoke == nil {
		authed.Handle("POST /api/invoke", notImplemented("Playground invoke"))
	}
	if s.adapters.Expand == nil {
		authed.Handle("POST /api/expand", notImplemented("config expand"))
	}

	api.Handle("/api/", s.requireAuth(authed))

	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/", s.spaHandler())
	return root
}

// spaHandler serves the static Vite build. Requests for real files are served
// verbatim; any other path (a client-side route like /agents) falls back to
// index.html so the React router can handle it (SPA history-mode routing).
// When no static FS is configured, it 404s (api-only mode).
func (s *Server) spaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.static == nil {
			http.NotFound(w, r)
			return
		}
		// Clean the request path to a filesystem path (io/fs uses no leading /).
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if f, err := s.static.Open(name); err == nil {
			// A directory request falls through to index.html (SPA route).
			if info, statErr := f.Stat(); statErr == nil && !info.IsDir() {
				_ = f.Close()
				http.FileServerFS(s.static).ServeHTTP(w, r)
				return
			}
			_ = f.Close()
		}
		// Not a real file → serve the SPA entrypoint for client-side routing.
		s.serveIndex(w)
	})
}

// serveIndex writes dist/index.html (the SPA shell). Used for client-side routes
// and when the requested asset does not exist on disk.
func (s *Server) serveIndex(w http.ResponseWriter) {
	data, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// index.html must never be cached so a new build's asset hashes are picked
	// up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
