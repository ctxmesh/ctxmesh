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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ctxmesh/ctxmesh/internal/bff"
)

// An install without an async backend must still pass its health check: the publisher it hands the
// server has to be a nil interface, not a nil *JetStreamBus wearing one.
func TestNoAsyncBackendKeepsTheBFFHealthy(t *testing.T) {
	if p := asyncPublisherFor(nil); p != nil {
		t.Fatalf("asyncPublisherFor(nil) = %#v, want a nil interface", p)
	}

	srv := bff.NewServer(bff.Options{AsyncPublisher: asyncPublisherFor(nil)})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health with no async backend = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
