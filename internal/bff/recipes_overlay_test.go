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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operatorRecipeYAML builds a valid operator-recipe file (the on-disk overlay shape) whose spec passes
// expand.Expand — the same gate the embedded recipes get at test time.
func operatorRecipeYAML(name, title string) string {
	return fmt.Sprintf(`name: %s
title: %s
description: An operator-provided test recipe.
icon: sparkles
spec: |
  name: %s
  runtime: managed
  systemPrompt: |
    You are a helpful test assistant.
  model:
    route: default
`, name, title, name)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// TestLoadRecipeOverlay_ValidAndSkips: a valid operator recipe loads; a malformed-YAML, a missing-name,
// and a spec-that-fails-validation are each SKIPPED (not the whole overlay) — fail-closed, per-file.
func TestLoadRecipeOverlay_ValidAndSkips(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.yaml", operatorRecipeYAML("acme-support", "Acme Support"))
	writeFile(t, dir, "bad-yaml.yaml", "name: x\n  : this is not: valid yaml: [")
	writeFile(t, dir, "no-name.yaml", "title: Nameless\nspec: |\n  runtime: managed\n")
	writeFile(t, dir, "bad-spec.yaml", "name: broken\ntitle: Broken\nspec: |\n  this is not a valid agent spec at all\n")
	writeFile(t, dir, "ignored.txt", "not a yaml file") // non-*.yaml is skipped

	s := &Server{log: logr.Discard()}
	overlay := s.loadRecipeOverlay(dir)

	require.Len(t, overlay, 1, "only the one valid recipe loads; the rest are skipped per-file")
	assert.Equal(t, "acme-support", overlay[0].Name)
	assert.Equal(t, "Acme Support", overlay[0].Title)
}

// TestLoadRecipeOverlay_AbsentEmpty: an absent or empty dir yields nil (embedded-only, fail-closed).
func TestLoadRecipeOverlay_AbsentEmpty(t *testing.T) {
	s := &Server{log: logr.Discard()}
	assert.Nil(t, s.loadRecipeOverlay(filepath.Join(t.TempDir(), "does-not-exist")), "absent dir ⇒ nil overlay")
	assert.Nil(t, s.loadRecipeOverlay(t.TempDir()), "empty dir ⇒ nil overlay")
}

// TestMergeRecipes_OverrideInPlaceAndAppend: an operator recipe with an embedded name REPLACES it IN
// PLACE (stable position); a net-new operator recipe is appended.
func TestMergeRecipes_OverrideInPlaceAndAppend(t *testing.T) {
	embedded := []recipeFile{
		{Name: "a", Title: "A"},
		{Name: "b", Title: "B (embedded)"},
		{Name: "c", Title: "C"},
	}
	overlay := []recipeFile{
		{Name: "b", Title: "B (operator override)"},
		{Name: "d", Title: "D (operator new)"},
	}
	merged := mergeRecipes(embedded, overlay)

	require.Len(t, merged, 4)
	assert.Equal(t, []string{"a", "b", "c", "d"}, []string{merged[0].Name, merged[1].Name, merged[2].Name, merged[3].Name},
		"override is in place (b stays 2nd); the net-new recipe is appended")
	assert.Equal(t, "B (operator override)", merged[1].Title, "the operator recipe overrides the embedded default")
	assert.Equal(t, "D (operator new)", merged[3].Title)
}

// TestHandleListRecipes_MergesOverlay: GET /api/recipes returns the embedded gallery merged with the
// operator overlay (a net-new operator recipe appears alongside the embedded ones).
func TestHandleListRecipes_MergesOverlay(t *testing.T) {
	s := &Server{log: logr.Discard(), recipeOverlay: &recipeOverlayHolder{}}
	s.recipeOverlay.store([]recipeFile{{Name: "acme-support", Title: "Acme Support", Spec: "name: acme-support"}})

	rec := httptest.NewRecorder()
	s.handleListRecipes(rec, httptest.NewRequest(http.MethodGet, "/api/recipes", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RecipeListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	names := map[string]bool{}
	for _, r := range resp.Recipes {
		names[r.Name] = true
	}
	assert.True(t, names["acme-support"], "the operator recipe appears in the gallery")
	assert.True(t, names["summarizer"], "the embedded defaults are still present")
	assert.Greater(t, len(resp.Recipes), 1, "embedded + operator merged")
}

// TestHandleListRecipes_NilOverlayServesEmbedded: with no overlay the gallery is exactly the embedded
// defaults (fail-closed / backward-compatible).
func TestHandleListRecipes_NilOverlayServesEmbedded(t *testing.T) {
	s := &Server{log: logr.Discard()} // recipeOverlay nil
	rec := httptest.NewRecorder()
	s.handleListRecipes(rec, httptest.NewRequest(http.MethodGet, "/api/recipes", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RecipeListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Recipes, "embedded gallery serves even with a nil overlay")
}
