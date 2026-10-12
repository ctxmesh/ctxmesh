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
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controlPlanePkg prefixes every package whose Store the control-plane database backs.
const controlPlanePkg = "github.com/ctxmesh/ctxmesh/internal/controlplane/"

// TestUseControlPlaneDBSetsEveryStore: an Options field typed by a control-plane package is a
// store this database backs, and the binary only sets what UseControlPlaneDB sets. Skills shipped
// with no wiring for exactly this reason: /api/skills answered 501 on every install.
func TestUseControlPlaneDBSetsEveryStore(t *testing.T) {
	t.Parallel()

	// sql.Open never connects; the constructors only keep the handle.
	db, err := sql.Open("pgx", "postgres://census.invalid/never")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var opts Options
	opts.UseControlPlaneDB(db)

	v := reflect.ValueOf(&opts).Elem()
	var stores []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type.Kind() != reflect.Interface || !strings.HasPrefix(f.Type.PkgPath(), controlPlanePkg) {
			continue
		}
		stores = append(stores, f.Name)
		assert.False(t, v.Field(i).IsNil(), "Options.%s (%s) is never set from the control-plane database", f.Name, f.Type)
	}
	assert.Contains(t, stores, "SkillStore", "the census must see the field this test exists for")
	assert.NotNil(t, opts.OnlineResolver, "the online-scoring worker reads its judge policy through the store")
}
