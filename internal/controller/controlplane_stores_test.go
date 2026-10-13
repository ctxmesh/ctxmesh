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

package controller

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
)

// TestUseControlPlaneDBSetsEveryStore: a reconciler field typed by a control-plane package is a
// store this database backs, and the binary only sets what UseControlPlaneDB sets. spec.skillRefs
// shipped unresolvable on every install because its resolver was never constructed.
func TestUseControlPlaneDBSetsEveryStore(t *testing.T) {
	t.Parallel()

	// sql.Open never connects; the constructors only keep the handle.
	db, err := sql.Open("pgx", "postgres://census.invalid/never")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var r AgentDeploymentReconciler
	r.UseControlPlaneDB(db, agentversion.NewMemStore())

	v := reflect.ValueOf(&r).Elem()
	var stores []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type.Kind() != reflect.Interface ||
			!strings.HasPrefix(f.Type.PkgPath(), "github.com/ctxmesh/ctxmesh/internal/controlplane/") {
			continue
		}
		stores = append(stores, f.Name)
		assert.False(t, v.Field(i).IsNil(), "AgentDeploymentReconciler.%s (%s) is never set from the control-plane database",
			f.Name, f.Type)
	}
	assert.Contains(t, stores, "Skills", "the census must see the field this test exists for")
	assert.NotNil(t, r.OnlineScore, "the rollback guard reads per-version scores")
	assert.NotNil(t, r.OnlineConfig, "the online-scoring policy is projected for the BFF")
}
