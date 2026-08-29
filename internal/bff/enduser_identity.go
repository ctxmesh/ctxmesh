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
	"context"
	"fmt"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
)

// resolveEndUserIdentity resolves an agent namespace to its tenant's ENABLED end-user OIDC config
// (M137/EU1b, ADR 0106 §2 — route by the TARGET tenant, never by an unverified token claim), or
// (nil, nil) when the namespace has no enabled+complete end-user IdP. The nil result is the fail-CLOSED
// default: /chat then stays console-authenticated and never trusts an unconfigured issuer.
//
// It REFUSES an issuer equal to the console OIDC issuer (ADR 0106 §3b) — belt-and-suspenders over the
// structural K8s-path separation: an end-user token must never gain K8s trust, so a colliding issuer is
// dropped with a loud log, never served. (The cluster service-account issuer joins the refusal set in
// m137.3 with the unified caller resolver.)
func (s *Server) resolveEndUserIdentity(ctx context.Context, ns string) (*namespacetenant.EndUserIdentity, error) {
	if s.namespaceTenantStore == nil || strings.TrimSpace(ns) == "" {
		return nil, nil
	}
	cfg, ok, err := s.namespaceTenantStore.EndUserIdentityForNamespace(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok || !cfg.Enabled || strings.TrimSpace(cfg.Issuer) == "" || strings.TrimSpace(cfg.ClientID) == "" {
		return nil, nil
	}
	if s.forbiddenEndUserIssuer(cfg.Issuer) {
		s.log.Error(fmt.Errorf("end-user issuer %q collides with the console OIDC issuer", cfg.Issuer),
			"refusing end-user IdP config (an end-user token must never gain K8s trust)", "namespace", ns)
		return nil, nil
	}
	out := cfg
	return &out, nil
}

// forbiddenEndUserIssuer reports whether an end-user issuer collides with a K8s-trusted issuer — the
// console OIDC issuer (ADR 0106 §3b). Comparison is trailing-slash- and case-insensitive. An empty
// issuer is forbidden. (m137.3 extends this with the cluster service-account issuer.)
func (s *Server) forbiddenEndUserIssuer(issuer string) bool {
	iss := strings.TrimRight(strings.TrimSpace(issuer), "/")
	if iss == "" {
		return true
	}
	banned := strings.TrimRight(strings.TrimSpace(s.oidcIssuer), "/")
	return banned != "" && strings.EqualFold(iss, banned)
}
