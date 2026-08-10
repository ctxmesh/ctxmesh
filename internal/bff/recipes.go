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

// recipes.go implements GET /api/recipes (ADR 0066 D4): a recipe gallery of
// curated, Go-embedded agent.yaml starters a user one-clicks to pre-fill the
// create form. Every embedded recipe is expand-validated at test time so a
// malformed recipe is caught before it ships.

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed recipes/*.yaml
var recipeFS embed.FS

// recipeFile is the on-disk shape of each embedded recipe YAML file.
// spec is the raw simplified agent.yaml string — exactly what the create form
// pre-fills and what expand.Expand validates.
type recipeFile struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Icon        string `yaml:"icon"`
	Spec        string `yaml:"spec"`
}

// embeddedRecipes is the parsed, immutable slice of all embedded recipes.
// It is initialised exactly once by loadRecipes (via recipeOnce).
var (
	embeddedRecipes []recipeFile
	recipeOnce      sync.Once
	recipeLoadErr   error
)

// loadRecipes parses all *.yaml files embedded in recipeFS into embeddedRecipes.
// It is called lazily the first time handleListRecipes runs so the hot path is
// a single atomic read after the first request.
func loadRecipes() ([]recipeFile, error) {
	recipeOnce.Do(func() {
		entries, err := fs.ReadDir(recipeFS, "recipes")
		if err != nil {
			recipeLoadErr = fmt.Errorf("reading embedded recipes dir: %w", err)
			return
		}
		out := make([]recipeFile, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, readErr := recipeFS.ReadFile("recipes/" + e.Name())
			if readErr != nil {
				recipeLoadErr = fmt.Errorf("reading embedded recipe %s: %w", e.Name(), readErr)
				return
			}
			var rf recipeFile
			if parseErr := yaml.Unmarshal(data, &rf); parseErr != nil {
				recipeLoadErr = fmt.Errorf("parsing embedded recipe %s: %w", e.Name(), parseErr)
				return
			}
			out = append(out, rf)
		}
		embeddedRecipes = out
	})
	return embeddedRecipes, recipeLoadErr
}

// handleListRecipes serves GET /api/recipes — the curated recipe gallery.
// Recipes carry no secrets and no caller-specific data, so the endpoint sits on
// the authenticated mux (consistent with all other console read endpoints) but
// performs no cluster lookup; the response is the same for every authenticated
// caller on any namespace.
func (s *Server) handleListRecipes(w http.ResponseWriter, _ *http.Request) {
	recipes, err := loadRecipes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recipe gallery unavailable")
		return
	}

	summaries := make([]RecipeSummary, 0, len(recipes))
	for _, r := range recipes {
		summaries = append(summaries, RecipeSummary(r))
	}

	writeJSON(w, http.StatusOK, RecipeListResponse{Recipes: summaries})
}
