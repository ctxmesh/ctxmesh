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

package credresolve

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEndUserHash is the M137/EU1b domain-separation contract (ADR 0106 §5): an end-user identity keyed
// on (iss,sub) hashes to a value that can NEVER collide with a K8s-username UserHash, and distinct
// (iss,sub) pairs never collide with each other.
func TestEndUserHash(t *testing.T) {
	key := []byte("test-cluster-hmac-key")

	// Deterministic + the "eu-" prefix (distinct output space from UserHash's "u-").
	h := EndUserHash(key, "https://issuer.example.com", "sub-1")
	assert.Equal(t, h, EndUserHash(key, "https://issuer.example.com", "sub-1"), "deterministic")
	assert.True(t, strings.HasPrefix(h, "eu-"), "end-user subjects carry the eu- prefix")
	assert.True(t, strings.HasPrefix(UserHash(key, "anything"), "u-"), "K8s usernames carry u-")

	// Domain separation vs UserHash: an end-user hash can never equal a K8s-username hash — the prefixes
	// differ, so even a K8s username crafted as "oidc:..." or the raw joined form cannot collide.
	assert.NotEqual(t, h, UserHash(key, "https://issuer.example.com#sub-1"))
	assert.NotEqual(t, h, UserHash(key, "oidc:https://issuer.example.com#sub-1"))

	// Length-prefixed framing: the classic concatenation collision ("a"+"b" vs "ab"+"") must NOT hold.
	assert.NotEqual(t,
		EndUserHash(key, "https://a", "b"),
		EndUserHash(key, "https://ab", ""),
		"length-prefixed fields prevent a cross-boundary concatenation collision")

	// Distinct subjects and distinct issuers both yield distinct hashes.
	assert.NotEqual(t, EndUserHash(key, "https://i", "alice"), EndUserHash(key, "https://i", "bob"))
	assert.NotEqual(t, EndUserHash(key, "https://i1", "s"), EndUserHash(key, "https://i2", "s"))

	// The unsalted fallback (no key) still carries eu- and differs from the keyed hash.
	unsalted := EndUserHash(nil, "https://i", "s")
	assert.True(t, strings.HasPrefix(unsalted, "eu-"))
	assert.NotEqual(t, EndUserHash(key, "https://i", "s"), unsalted, "the HMAC key salts the hash")
}
