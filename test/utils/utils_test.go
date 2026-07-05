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

package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetNonEmptyLines(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wanted []string
	}{
		{name: "empty input", input: "", wanted: nil},
		{name: "only newlines", input: "\n\n\n", wanted: nil},
		{name: "single line without newline", input: "one", wanted: []string{"one"}},
		{name: "trailing newline", input: "one\n", wanted: []string{"one"}},
		{
			name:   "blank lines are dropped",
			input:  "one\n\ntwo\n\n\nthree\n",
			wanted: []string{"one", "two", "three"},
		},
		{
			name:   "whitespace-only lines are kept",
			input:  "one\n \ntwo",
			wanted: []string{"one", " ", "two"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wanted, GetNonEmptyLines(tt.input))
		})
	}
}

func TestUncommentCode(t *testing.T) {
	t.Run("uncomments a multi-line block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.yaml")
		content := "resources:\n#- one.yaml\n#- two.yaml\nimages: []\n"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		require.NoError(t, UncommentCode(path, "#- one.yaml\n#- two.yaml", "#"))

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "resources:\n- one.yaml\n- two.yaml\nimages: []\n", string(got))
	})

	t.Run("errors when the target is absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.yaml")
		require.NoError(t, os.WriteFile(path, []byte("resources: []\n"), 0o600))

		err := UncommentCode(path, "#- missing.yaml", "#")
		require.ErrorContains(t, err, "unable to find the code")
	})

	t.Run("errors when the file does not exist", func(t *testing.T) {
		err := UncommentCode(filepath.Join(t.TempDir(), "absent.yaml"), "x", "#")
		require.ErrorContains(t, err, "failed to read file")
	})
}
