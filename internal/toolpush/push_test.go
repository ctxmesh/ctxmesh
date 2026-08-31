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

package toolpush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
)

func testManifest() toolmanifest.Manifest {
	return toolmanifest.Normalize(toolmanifest.Manifest{Tools: []toolmanifest.Tool{
		{Name: "word-count", Mode: "remote", Endpoint: "http://wc.svc/mcp", Transport: toolmanifest.Transport},
	}})
}

// TestPushSuccess: a 204 sidecar receives the exact manifest body.
func TestPushSuccess(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/control" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := &Pusher{Client: srv.Client()}
	m := testManifest()
	if err := p.Push(context.Background(), srv.URL+"/control", m); err != nil {
		t.Fatalf("Push returned error on 204: %v", err)
	}

	var sent toolmanifest.Manifest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body was not valid manifest JSON: %v", err)
	}
	if sent.Version != m.Version || len(sent.Tools) != 1 {
		t.Errorf("sidecar received %+v, want %+v", sent, m)
	}
}

// TestPushNon200Retries: a persistently 500 sidecar → Push returns an error,
// and the handler is hit maxAttempts times (retry happened).
func TestPushNon200Retries(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &Pusher{Client: srv.Client()}
	err := p.Push(context.Background(), srv.URL+"/control", testManifest())
	if err == nil {
		t.Fatal("Push must return an error when the sidecar returns 500")
	}
	if got := hits.Load(); got != maxAttempts {
		t.Errorf("handler hit %d times, want %d (retry on non-2xx)", got, maxAttempts)
	}
}

// TestPushTimeout: a sidecar that stalls past the client timeout → Push returns
// an error (deadline exceeded), not a hang. The handler waits ONLY on its
// request context, which the client cancels when it times out — so the handler
// returns promptly and srv.Close() never deadlocks (no external block channel
// whose close would have to be ordered against Close).
func TestPushTimeout(t *testing.T) {
	t.Parallel()

	// The handler sleeps LONGER than the client timeout but returns on its own
	// (bounded), so srv.Close() never waits on an externally-ordered channel and
	// cannot deadlock — a subtlety that bit an earlier version of this test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Client with a very short timeout so the test is fast (bounds the real
	// 2s pushTimeout for CI speed). 150ms < the 500ms handler sleep → the client
	// times out first, which is what we assert.
	p := &Pusher{Client: &http.Client{Timeout: 150 * time.Millisecond}}

	start := time.Now()
	err := p.Push(context.Background(), srv.URL+"/control", testManifest())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Push must return an error when the sidecar never responds")
	}
	// Two attempts × ~150ms; comfortably under a second — proves we bounded it.
	if elapsed > 2*time.Second {
		t.Errorf("Push took %v — timeout not enforced", elapsed)
	}
}

// TestPushURL formats the control URL for a pod IP.
func TestPushURL(t *testing.T) {
	t.Parallel()
	if got, want := PushURL("10.1.2.3"), "http://10.1.2.3:2999/control"; got != want {
		t.Errorf("PushURL = %q, want %q", got, want)
	}
}
