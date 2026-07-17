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
	"github.com/stretchr/testify/require"
)

func TestUserHash(t *testing.T) {
	const alice = "alice@example.com"
	const bob = "bob@example.com"

	t.Run("deterministic and prefixed", func(t *testing.T) {
		h1 := UserHash(nil, alice)
		h2 := UserHash(nil, alice)
		assert.Equal(t, h1, h2, "same user hashes stably")
		assert.True(t, strings.HasPrefix(h1, "u-"), "RFC-1123-safe prefix")
		assert.Len(t, h1, 42, "u- + 40 hex")
	})

	t.Run("distinct users never collide", func(t *testing.T) {
		assert.NotEqual(t, UserHash(nil, alice), UserHash(nil, bob))
	})

	t.Run("HMAC key changes the hash and blocks the unsalted value", func(t *testing.T) {
		unsalted := UserHash(nil, alice)
		salted := UserHash([]byte("per-cluster-key"), alice)
		assert.NotEqual(t, unsalted, salted, "the key must salt the hash")
		// A different key yields a different hash (re-key ⇒ re-consent).
		assert.NotEqual(t, salted, UserHash([]byte("other-key"), alice))
		// Same key is still deterministic.
		assert.Equal(t, salted, UserHash([]byte("per-cluster-key"), alice))
	})
}

func TestSecretNameDeterministicAndDistinct(t *testing.T) {
	aliceHash := UserHash(nil, "alice@example.com")
	bobHash := UserHash(nil, "bob@example.com")

	// Same (server, user) ⇒ same name (idempotent upsert on re-consent).
	assert.Equal(t, SecretName("weather", aliceHash), SecretName("weather", aliceHash))
	// Different user ⇒ different name.
	assert.NotEqual(t, SecretName("weather", aliceHash), SecretName("weather", bobHash))
	// Different server ⇒ different name.
	assert.NotEqual(t, SecretName("weather", aliceHash), SecretName("calendar", aliceHash))
	assert.True(t, strings.HasPrefix(SecretName("weather", aliceHash), SecretPrefix+"-"))
}

func TestSecretCoordinates(t *testing.T) {
	userHash := UserHash(nil, "alice@example.com")

	t.Run("legacy mode keeps the grant in its source namespace", func(t *testing.T) {
		ns, name := SecretCoordinates("", "team-alpha", "weather", userHash)
		assert.Equal(t, "team-alpha", ns)
		assert.Equal(t, SecretName("weather", userHash), name)
	})

	t.Run("locked mode folds the source namespace into ns + name", func(t *testing.T) {
		ns, name := SecretCoordinates("ae-credentials", "team-alpha", "weather", userHash)
		assert.Equal(t, "ae-credentials", ns)
		assert.True(t, strings.HasPrefix(name, SecretName("weather", userHash)+"-"))
		// A different source namespace ⇒ a distinct name in the shared locked namespace.
		_, other := SecretCoordinates("ae-credentials", "team-beta", "weather", userHash)
		assert.NotEqual(t, name, other, "different source namespaces must not collide")
	})
}

func TestSecretLabels(t *testing.T) {
	userHash := UserHash(nil, "alice@example.com")

	legacy := SecretLabels("weather", userHash, "")
	assert.Equal(t, ManagedByGrant, legacy[LabelManagedBy])
	assert.Equal(t, userHash, legacy[LabelGrantUser])
	assert.Equal(t, "weather", legacy[LabelGrantServer])
	_, hasSource := legacy[LabelGrantSourceNS]
	assert.False(t, hasSource, "no source-namespace label in legacy mode")

	locked := SecretLabels("weather", userHash, "team-alpha")
	assert.Equal(t, "team-alpha", locked[LabelGrantSourceNS])

	// A label value must never be a token; the labels carry only lookup keys.
	for k, v := range locked {
		require.NotContains(t, v, "token", "label %s must not carry token material", k)
	}
}
