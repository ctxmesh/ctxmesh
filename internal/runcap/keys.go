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

package runcap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// The platform capability keypair (ADR 0030 §5): the BFF holds the PRIVATE key (from a
// per-cluster platform Secret) and signs run capabilities; the sidecar / central token
// service hold the PUBLIC key and verify. Keys are carried as base64 in Secret data /
// env — the 32-byte Ed25519 seed for the private key, the 32-byte public key for
// verifiers. A prod cluster provisions the keypair at install (a documented prerequisite,
// like MCP_GRANT_HMAC_KEY); the dev bring-up generates one.

// GenerateKeyPair creates a fresh Ed25519 platform keypair.
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("runcap: generate keypair: %w", err)
	}
	return pub, priv, nil
}

// EncodePrivateSeed returns the base64 (std, padded) of the private key's 32-byte seed —
// what a platform Secret stores for the BFF. The seed (not the full 64-byte private key)
// is the canonical minimal secret; DecodePrivateSeed reconstructs the full key from it.
func EncodePrivateSeed(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Seed())
}

// EncodePublicKey returns the base64 (std, padded) of the 32-byte public key — what
// verifiers (the sidecar / central service) load.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// DecodePrivateSeed reconstructs an Ed25519 private key from a base64-encoded 32-byte
// seed (as produced by EncodePrivateSeed). It rejects a wrong-sized seed rather than
// silently producing a broken key.
func DecodePrivateSeed(encoded string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("runcap: decode private seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("runcap: private seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// DecodePublicKey reconstructs an Ed25519 public key from its base64 encoding, rejecting
// a wrong-sized key.
func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("runcap: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("runcap: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
