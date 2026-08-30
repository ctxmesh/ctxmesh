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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ctxmesh/agentry/internal/expand"
)

// TestListRecipes_Basic verifies that GET /api/recipes returns at least 4
// recipes, each with the required fields populated.
func TestListRecipes_Basic(t *testing.T) {
	s := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/recipes", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body RecipeListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.GreaterOrEqual(t, len(body.Recipes), 4, "expected at least 4 embedded recipes")

	for _, r := range body.Recipes {
		assert.NotEmpty(t, r.Name, "recipe.Name must not be empty")
		assert.NotEmpty(t, r.Title, "recipe.Title must not be empty")
		assert.NotEmpty(t, r.Description, "recipe.Description must not be empty")
		assert.NotEmpty(t, r.Spec, "recipe.Spec must not be empty")
	}
}

// TestListRecipes_ExpandValidate is the key test: every embedded recipe's spec
// must parse and expand cleanly. A malformed recipe (bad YAML, missing required
// field, unknown field) is caught here before it ships.
func TestListRecipes_ExpandValidate(t *testing.T) {
	recipes, err := loadRecipes()
	require.NoError(t, err, "loadRecipes must succeed")
	require.NotEmpty(t, recipes, "at least one recipe must be embedded")

	for _, r := range recipes {
		t.Run(r.Name, func(t *testing.T) {
			_, expandErr := expand.Expand([]byte(r.Spec))
			assert.NoError(t, expandErr,
				"recipe %q spec must expand without errors", r.Name)
		})
	}
}

// TestListRecipes_NoImageField asserts that no embedded recipe carries an
// explicit `image:` field — all recipes use `runtime: managed` (no image).
func TestListRecipes_NoImageField(t *testing.T) {
	recipes, err := loadRecipes()
	require.NoError(t, err, "loadRecipes must succeed")

	for _, r := range recipes {
		assert.NotContains(t, r.Spec, "\nimage:",
			"recipe %q must not carry an explicit image field (managed runtime only)", r.Name)
	}
}

// TestListRecipes_NoInlineSecret asserts that no embedded recipe's spec carries
// inline credential material (reuses the codebase's findInlineSecret walker).
func TestListRecipes_NoInlineSecret(t *testing.T) {
	recipes, err := loadRecipes()
	require.NoError(t, err, "loadRecipes must succeed")

	for _, r := range recipes {
		t.Run(r.Name, func(t *testing.T) {
			// Parse the spec into a generic tree for the secret walker.
			// sigs.k8s.io/yaml unmarshals YAML→JSON-compatible types (map[string]any),
			// which matches what findInlineSecret expects.
			var tree any
			require.NoError(t, sigsyaml.Unmarshal([]byte(r.Spec), &tree),
				"recipe %q spec must be valid YAML", r.Name)
			path := findInlineSecret(tree, "")
			assert.Empty(t, path,
				"recipe %q contains inline secret at path %s", r.Name, path)
		})
	}
}

// TestListRecipes_NameUniqueness verifies that no two recipes share the same
// name (uniqueness is required for the one-click create flow).
func TestListRecipes_NameUniqueness(t *testing.T) {
	recipes, err := loadRecipes()
	require.NoError(t, err, "loadRecipes must succeed")

	seen := make(map[string]bool)
	for _, r := range recipes {
		assert.False(t, seen[r.Name], "duplicate recipe name %q", r.Name)
		seen[r.Name] = true
	}
}

// TestListRecipes_ManagedRuntime asserts every recipe spec uses runtime: managed.
func TestListRecipes_ManagedRuntime(t *testing.T) {
	recipes, err := loadRecipes()
	require.NoError(t, err, "loadRecipes must succeed")

	for _, r := range recipes {
		assert.True(t, strings.Contains(r.Spec, "runtime: managed"),
			"recipe %q must set runtime: managed", r.Name)
	}
}
