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
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
	"github.com/ctxmesh/ctxmesh/internal/statelayer"
)

// The token service's caller-authentication configuration (ADR 0154).
const (
	envCapabilityPublicKey = "MCP_CAPABILITY_PUBLIC_KEY"
	envCapabilityAudience  = "MCP_CAPABILITY_AUDIENCE"
	envOBORequired         = "MCP_OBO_REQUIRED"
	envPodNamespace        = "POD_NAMESPACE"
	// envPlatformServiceAccounts names, comma-separated, the ServiceAccounts in the service's own
	// namespace that may store and revoke grants: the BFF's.
	envPlatformServiceAccounts = "TOKEN_SERVICE_PLATFORM_SERVICE_ACCOUNTS"
)

// reviewVerifier adapts the state-layer TokenReview authenticator, which checks the audience and caches
// each token until it expires, to the credential plane.
type reviewVerifier struct{ auth statelayer.PodAuthenticator }

func (v reviewVerifier) Verify(ctx context.Context, token string) (credplane.CallerIdentity, error) {
	id, err := v.auth.Identity(ctx, token)
	if errors.Is(err, statelayer.ErrTokenRejected) {
		return credplane.CallerIdentity{}, credplane.ErrCallerRejected
	}
	if err != nil {
		return credplane.CallerIdentity{}, err
	}
	return credplane.CallerIdentity{Namespace: id.Namespace, ServiceAccount: id.ServiceAccount}, nil
}

// callerAuth builds the service's caller authentication. There is no mode without it.
//
// The capability public key arrives from the bff-capability Secret, which a fresh Helm install creates in
// a post-install hook that runs only once the release is Ready. Refusing to start without it would hold the
// install that creates it, so the service starts and refuses what needs a capability with 503 until the
// hook restarts it. When OBO is required it refuses to start, as the BFF does.
func callerAuth(log logr.Logger, cs kubernetes.Interface, getenv func(string) string) (credplane.CallerAuth, error) {
	auth := credplane.CallerAuth{
		Sidecar: reviewVerifier{statelayer.NewTokenReviewAuthenticator(cs, runcap.TokenServicePodAudience, nil)},
		Agent:   reviewVerifier{statelayer.NewTokenReviewAuthenticator(cs, runcap.TokenServiceAgentAudience, nil)},
	}

	switch pubB64 := strings.TrimSpace(getenv(envCapabilityPublicKey)); {
	case pubB64 != "":
		pub, err := runcap.DecodePublicKey(pubB64)
		if err != nil {
			return credplane.CallerAuth{}, fmt.Errorf("decode %s: %w", envCapabilityPublicKey, err)
		}
		audience := strings.TrimSpace(getenv(envCapabilityAudience))
		if audience == "" {
			audience = runcap.DefaultAudience
		}
		auth.Capabilities = runcap.NewVerifier(pub, audience, nil)
	case isTruthy(getenv(envOBORequired)):
		return credplane.CallerAuth{}, fmt.Errorf("%s=true but %s is unset: the token service could not verify "+
			"any run capability, so no tool credential would ever resolve", envOBORequired, envCapabilityPublicKey)
	default:
		log.Error(errors.New(envCapabilityPublicKey+" is unset"), "credential resolution and per-user memory "+
			"are refused until the capability key exists and the token service restarts")
	}

	podNS := strings.TrimSpace(getenv(envPodNamespace))
	for sa := range strings.SplitSeq(getenv(envPlatformServiceAccounts), ",") {
		if sa = strings.TrimSpace(sa); sa == "" {
			continue
		}
		if podNS == "" {
			return credplane.CallerAuth{}, fmt.Errorf("%s needs %s (the service's own namespace)",
				envPlatformServiceAccounts, envPodNamespace)
		}
		auth.PlatformCallers = append(auth.PlatformCallers, credplane.CallerIdentity{Namespace: podNS, ServiceAccount: sa})
	}
	if len(auth.PlatformCallers) == 0 {
		log.Error(errors.New(envPlatformServiceAccounts+" is unset"), "no caller may store or revoke a grant")
	}
	return auth, nil
}

// isTruthy reports whether v spells true, the way envTrue reads a variable.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
