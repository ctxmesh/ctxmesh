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
	"net/http"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
)

// envTokenServiceTokenPath is the sidecar's projected token for the token service (audience
// runcap.TokenServicePodAudience), mounted into this container only. The controller sets it.
const envTokenServiceTokenPath = "TOKEN_SERVICE_TOKEN_PATH"

// withTokenServiceCredential makes every request c sends to the token service carry this sidecar's
// projected token. The service refuses a caller without one (ADR 0154), so a sidecar without the
// path refuses to start rather than fail every tool call.
func withTokenServiceCredential(c *http.Client, getenv func(string) string) (*http.Client, error) {
	path := strings.TrimSpace(getenv(envTokenServiceTokenPath))
	if path == "" {
		return nil, errors.New(envTokenServiceTokenPath + " is required to call the token service (ADR 0154)")
	}
	out := *c
	out.Transport = credplane.NewBearerTransport(c.Transport, path)
	return &out, nil
}
