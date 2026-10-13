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
	"fmt"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// capabilityVerifierFromEnv builds the verifier every launcher path checks run capabilities and AMP
// attestations with: the platform public key (MCP_CAPABILITY_PUBLIC_KEY) and the audience the BFF mints
// for (MCP_CAPABILITY_AUDIENCE, or runcap.DefaultAudience when the install sets none). It is the only
// place a launcher builds one (TestOneCapabilityVerifierConstructor): a verifier left with an empty
// audience refuses every capability the BFF mints.
//
// nil, nil when the pod has no key; an error when the key is malformed. Each caller decides what that
// means for its path.
func capabilityVerifierFromEnv(lookup func(string) string) (*runcap.Verifier, error) {
	pubB64 := strings.TrimSpace(lookup("MCP_CAPABILITY_PUBLIC_KEY"))
	if pubB64 == "" {
		return nil, nil
	}
	pub, err := runcap.DecodePublicKey(pubB64)
	if err != nil {
		return nil, fmt.Errorf("MCP_CAPABILITY_PUBLIC_KEY is malformed: %w", err)
	}
	aud := strings.TrimSpace(lookup("MCP_CAPABILITY_AUDIENCE"))
	if aud == "" {
		aud = runcap.DefaultAudience
	}
	return runcap.NewVerifier(pub, aud, nil), nil
}
