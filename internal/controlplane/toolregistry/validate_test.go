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

package toolregistry

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

func TestValidate(t *testing.T) {
	valid := ToolRegistry{
		Namespace: "default",
		Name:      "scalekit-mcp-server",
		Tools: []ToolEntry{
			{Name: "list_files", Image: "img:1", Source: sourceCurated, ApprovalStatus: approvalApproved},
			{Name: "read_file", URL: "https://mcp.example/read", Source: sourceUserAdded, ApprovalStatus: approvalPending},
		},
	}

	cases := []struct {
		name    string
		mutate  func(tr *ToolRegistry)
		wantErr bool
	}{
		{"valid", func(*ToolRegistry) {}, false},
		{"valid with empty source/approval (backward compat)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t"}}
		}, false},
		{"missing namespace", func(tr *ToolRegistry) { tr.Namespace = "" }, true},
		{"invalid namespace (uppercase)", func(tr *ToolRegistry) { tr.Namespace = "Default" }, true},
		{"missing name", func(tr *ToolRegistry) { tr.Name = "" }, true},
		{"invalid name (underscore)", func(tr *ToolRegistry) { tr.Name = "bad_name" }, true},
		{"no tools (MinItems=1)", func(tr *ToolRegistry) { tr.Tools = nil }, true},
		{"too many tools (MaxItems=200)", func(tr *ToolRegistry) {
			tr.Tools = make([]ToolEntry, maxTools+1)
			for i := range tr.Tools {
				tr.Tools[i] = ToolEntry{Name: "t" + strings.Repeat("x", 0) + itoa(i)}
			}
		}, true},
		{"empty tool name (MinLength=1)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: ""}}
		}, true},
		{"tool name too long (MaxLength=63)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: strings.Repeat("a", maxToolNameLen+1)}}
		}, true},
		{"image too long (MaxLength=512)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t", Image: strings.Repeat("a", maxImageLen+1)}}
		}, true},
		{"url too long (MaxLength=512)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t", URL: strings.Repeat("a", maxURLLen+1)}}
		}, true},
		{"description too long (MaxLength=1024)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t", Description: strings.Repeat("a", maxDescLen+1)}}
		}, true},
		{"bad source enum", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t", Source: "invented"}}
		}, true},
		{"bad approvalStatus enum", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "t", ApprovalStatus: "maybe"}}
		}, true},
		{"duplicate tool name (uniqueness)", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: "dup"}, {Name: "dup"}}
		}, true},
		{"boundary: exactly 200 tools ok", func(tr *ToolRegistry) {
			tr.Tools = make([]ToolEntry, maxTools)
			for i := range tr.Tools {
				tr.Tools[i] = ToolEntry{Name: "t" + itoa(i)}
			}
		}, false},
		{"boundary: name exactly 63 ok", func(tr *ToolRegistry) {
			tr.Tools = []ToolEntry{{Name: strings.Repeat("a", maxToolNameLen)}}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := valid
			// deep-copy the tools slice header the mutate may replace wholesale
			tr.Tools = append([]ToolEntry(nil), valid.Tools...)
			tc.mutate(&tr)
			err := Validate(tr)
			if tc.wantErr {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, controlplane.ErrInvalid), "must wrap ErrInvalid for a 422")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// itoa is a tiny int→string without pulling strconv into the test's hot table.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
