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

package promptversion

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

func TestValidate(t *testing.T) {
	valid := PromptVersion{Namespace: "default", Name: "pv1", Repo: "github.com/a/b", Ref: "v1", Path: "p.md"}

	cases := []struct {
		name    string
		mutate  func(pv *PromptVersion)
		wantErr bool
	}{
		{"valid", func(*PromptVersion) {}, false},
		{"missing namespace", func(pv *PromptVersion) { pv.Namespace = "" }, true},
		{"missing name", func(pv *PromptVersion) { pv.Name = "" }, true},
		{"invalid name (uppercase)", func(pv *PromptVersion) { pv.Name = "Bad_Name" }, true},
		{"missing repo", func(pv *PromptVersion) { pv.Repo = "" }, true},
		{"blank ref", func(pv *PromptVersion) { pv.Ref = "   " }, true},
		{"missing path", func(pv *PromptVersion) { pv.Path = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv := valid
			tc.mutate(&pv)
			err := Validate(pv)
			if tc.wantErr {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, controlplane.ErrInvalid), "must wrap ErrInvalid for a 422")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
