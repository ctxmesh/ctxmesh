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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// capabilityCensusPath is the census of the cells GET /api/capabilities answers, under the UI
// test tree so the console's nav-gate census (ui/src/lib/nav-gates.test.ts) can import it.
const capabilityCensusPath = "../../ui/src/test/contract/capabilities.json"

// probedCells runs the real probe against a client that allows everything and returns every
// resource it answered with that resource's verbs, sorted: exactly the cells a console gate
// may name. A cell missing from it is one the console reads as allowed for everybody.
func probedCells(t *testing.T) map[string][]string {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssarInterceptor(func(string, string, string) bool { return true })).
		Build()
	allowed, err := probeCapabilities(context.Background(), c, "census")
	require.NoError(t, err)
	out := make(map[string][]string, len(allowed))
	for res, verbs := range allowed {
		vs := make([]string, 0, len(verbs))
		for v := range verbs {
			vs = append(vs, v)
		}
		slices.Sort(vs)
		out[res] = vs
	}
	return out
}

// TestCapabilityCensus keeps the committed census equal to what the probe actually asks. A probe
// dropped or added here fails this test until the census is regenerated, and the regenerated
// census then fails the console's nav-gate test if a gate still names the dropped cell:
//
//	UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestCapabilityCensus
func TestCapabilityCensus(t *testing.T) {
	got, err := json.MarshalIndent(probedCells(t), "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(capabilityCensusPath), 0o755))
		require.NoError(t, os.WriteFile(capabilityCensusPath, got, 0o644))
		t.Logf("wrote %s", capabilityCensusPath)
		return
	}

	want, err := os.ReadFile(capabilityCensusPath)
	require.NoError(t, err,
		"capability census missing — run: UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestCapabilityCensus")
	require.Equal(t, string(want), string(got),
		"the capability probe changed but the console's census did not — regenerate with "+
			"UPDATE_FIXTURES=1 go test ./internal/bff/ -run TestCapabilityCensus")
}
