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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/test/sdkconformance"
)

// tap sits between an SDK and one launcher listener and records what crossed it, so a probe line
// can PASS only when the launcher actually received the call and the request met the contract. It
// forwards every byte unchanged: the launcher's answer is what the SDK sees.
type tap struct {
	listener string
	contract *sdkconformance.Contract
	srv      *http.Server
	port     int

	mu   sync.Mutex
	seen []observed
}

// observed is one request the tap forwarded.
type observed struct {
	route      string // contract key, or "" for a path the contract does not know
	method     string
	path       string
	status     int
	violations []string
}

func startTap(listener string, port int, upstreamPort int, c *sdkconformance.Contract) (*tap, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", upstreamPort)}
	t := &tap{listener: listener, contract: c, port: ln.Addr().(*net.TCPAddr).Port}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	t.srv = &http.Server{Handler: t.record(proxy), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := t.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "sdk-conformance: %s tap: %v\n", listener, err)
		}
	}()
	return t, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *tap) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		ob := observed{method: r.Method, path: r.URL.Path}
		if rt := t.contract.Route(r.Method, r.URL.Path); rt != nil {
			ob.route = rt.Key()
			if rt.Listener != t.listener {
				ob.violations = append(ob.violations, fmt.Sprintf("sent to the %s listener; the route is served by %s",
					t.listener, rt.Listener))
			}
			if rt.Method == sdkconformance.AnyMethod && r.Method != rt.ServedMethod() {
				ob.violations = append(ob.violations, "the route serves "+rt.ServedMethod()+", not "+r.Method)
			}
			query := map[string]string{}
			for k, v := range r.URL.Query() {
				query[k] = strings.Join(v, ",")
			}
			headers := map[string]string{}
			for k := range r.Header {
				headers[k] = r.Header.Get(k)
			}
			ob.violations = append(ob.violations, rt.RequestViolations(r.URL.Path, query, headers, body)...)
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		ob.status = sw.status
		t.mu.Lock()
		t.seen = append(t.seen, ob)
		t.mu.Unlock()
	})
}

func (t *tap) drain() []observed {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.seen
	t.seen = nil
	return out
}

func (t *tap) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = t.srv.Shutdown(ctx)
}
