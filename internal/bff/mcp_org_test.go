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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// (The org-promote happy path + the RBAC-gate crux are covered store-backed by
// TestSetOrgCredential_RetirePromotesInStore + …_RetireUpdateDeniedNoCredential in
// mcp_org_retire_test.go.)

// TestSetOrgCredentialRejectsMissingFields proves required-field validation.
func TestSetOrgCredentialRejectsMissingFields(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod"}) // no credential
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
