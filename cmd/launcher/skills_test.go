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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
)

// mountSkillBodies lays files out as the kubelet mounts a ConfigMap volume: each key is a symlink
// through ..data into a timestamped directory. The controller keys the ConfigMap by skill name.
func mountSkillBodies(t *testing.T, bodies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	ts := filepath.Join(dir, "..2026_10_13_00_00_00.000000001")
	require.NoError(t, os.Mkdir(ts, 0o755))
	for name, body := range bodies {
		require.NoError(t, os.WriteFile(filepath.Join(ts, name), []byte(body), 0o644))
	}
	require.NoError(t, os.Symlink(filepath.Base(ts), filepath.Join(dir, "..data")))
	for name := range bodies {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)))
	}
	return dir
}

func loadSkill(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	sk := newSkillServer()
	require.NotNil(t, sk, "SKILL_REFS is set, so the skill endpoints exist")
	mux := http.NewServeMux()
	sk.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skills/load", strings.NewReader(`{"name":"`+name+`"}`)))
	return rec
}

// load() returns the body the controller mounted for an attached skill, and refuses a skill that is
// not attached however its file got there.
func TestLoadReturnsTheMountedBodyOfAnAttachedSkill(t *testing.T) {
	const body = "Refunds: check the order date, then the receipt."
	t.Setenv(envSkillRefs, "refund-policy@"+skill.Digest([]byte(body)))
	t.Setenv(envSkillDescriptions, `{"refund-policy":"How to handle a refund request."}`)
	t.Setenv(envSkillDir, mountSkillBodies(t, map[string]string{
		"refund-policy": body,
		"payroll":       "a body this agent does not have attached",
	}))

	rec := loadSkill(t, "refund-policy")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, body, got.Body)

	rec = loadSkill(t, "payroll")
	assert.Equal(t, http.StatusForbidden, rec.Code, "a skill not attached to the agent is refused: %s", rec.Body.String())
}

// An attached skill whose body the controller did not mount — too large, unfetchable or failing its
// digest — answers 501, as an agent with no mounted bodies does, and points at the condition.
func TestAnAttachedSkillWithoutAMountedBodyIs501(t *testing.T) {
	const body = "mounted"
	t.Setenv(envSkillRefs, "mounted@"+skill.Digest([]byte(body))+",missing@"+skill.Digest([]byte("x")))
	t.Setenv(envSkillDir, mountSkillBodies(t, map[string]string{"mounted": body}))

	rec := loadSkill(t, "missing")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Contains(t, rec.Body.String(), "SkillBodies")

	t.Setenv(envSkillDir, "")
	rec = loadSkill(t, "mounted")
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "no mounted bodies at all is also 501")
}

// The launcher serves a body only if it hashes to the pinned digest, so a file changed after the
// controller verified it never reaches the model.
func TestAChangedBodyIsNotServed(t *testing.T) {
	t.Setenv(envSkillRefs, "refund-policy@"+skill.Digest([]byte("what was reviewed")))
	t.Setenv(envSkillDir, mountSkillBodies(t, map[string]string{"refund-policy": "what is there now"}))

	rec := loadSkill(t, "refund-policy")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "what is there now")
	assert.Contains(t, rec.Body.String(), "does not match its pinned digest")
}
