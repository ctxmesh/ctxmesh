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

package toolmanifest

import (
	"encoding/hex"
	"testing"
)

// TestNormalizeVersionDeterminism verifies that the same tool set produces the
// same version regardless of the order tools are supplied.
func TestNormalizeVersionDeterminism(t *testing.T) {
	t.Parallel()

	toolA := Tool{Name: "alpha", Mode: "remote", Endpoint: "http://alpha.svc", Transport: "streamable-http"}
	toolB := Tool{Name: "beta", Mode: "sidecar", Endpoint: "http://localhost:3001", Transport: "streamable-http"}
	toolC := Tool{Name: "gamma", Mode: "remote", Endpoint: "http://gamma.svc", Transport: "streamable-http"}

	orders := [][]Tool{
		{toolA, toolB, toolC},
		{toolC, toolA, toolB},
		{toolB, toolC, toolA},
		{toolC, toolB, toolA},
		{toolB, toolA, toolC},
		{toolA, toolC, toolB},
	}

	first := Normalize(Manifest{Tools: orders[0]}).Version
	for i, order := range orders[1:] {
		got := Normalize(Manifest{Tools: order}).Version
		if got != first {
			t.Errorf("order %d: version = %q, want %q (same as order 0)", i+1, got, first)
		}
	}
}

// TestNormalizeVersionChangesOnContentChange verifies that a content change
// (different tool endpoint) produces a different version.
func TestNormalizeVersionChangesOnContentChange(t *testing.T) {
	t.Parallel()

	base := Manifest{Tools: []Tool{
		{Name: "word-count", Mode: "remote", Endpoint: "http://wc.svc", Transport: "streamable-http"},
	}}
	modified := Manifest{Tools: []Tool{
		{Name: "word-count", Mode: "remote", Endpoint: "http://wc-v2.svc", Transport: "streamable-http"},
	}}

	v1 := Normalize(base).Version
	v2 := Normalize(modified).Version

	if v1 == v2 {
		t.Errorf("version did not change after content change: both = %q", v1)
	}
}

// TestNormalizeVersionFormat verifies the version is exactly 8 lowercase hex characters.
func TestNormalizeVersionFormat(t *testing.T) {
	t.Parallel()

	m := Normalize(Manifest{Tools: []Tool{
		{Name: "tool-1", Mode: "remote", Endpoint: "http://tool1.svc", Transport: "streamable-http"},
	}})

	if len(m.Version) != 8 {
		t.Errorf("version length = %d, want 8; got %q", len(m.Version), m.Version)
	}

	decoded, err := hex.DecodeString(m.Version)
	if err != nil {
		t.Errorf("version is not valid hex: %q — %v", m.Version, err)
	}
	if len(decoded) != 4 {
		t.Errorf("decoded version length = %d bytes, want 4 (8 hex chars)", len(decoded))
	}
}

// TestNormalizeEmptyList verifies that an empty tool list produces a stable,
// valid 8-hex version and an empty (non-nil) slice.
func TestNormalizeEmptyList(t *testing.T) {
	t.Parallel()

	m := Normalize(Manifest{Tools: nil})

	if m.Tools == nil {
		t.Error("Normalize returned nil Tools slice; want empty (non-nil) slice")
	}
	if len(m.Tools) != 0 {
		t.Errorf("len(Tools) = %d, want 0", len(m.Tools))
	}
	if len(m.Version) != 8 {
		t.Errorf("version length = %d, want 8; got %q", len(m.Version), m.Version)
	}
	if _, err := hex.DecodeString(m.Version); err != nil {
		t.Errorf("empty-list version is not valid hex: %q — %v", m.Version, err)
	}

	// Two empty manifests must produce the same version (stability).
	m2 := Normalize(Manifest{Tools: []Tool{}})
	if m.Version != m2.Version {
		t.Errorf("two empty manifests produced different versions: %q vs %q", m.Version, m2.Version)
	}
}

// TestNormalizeSortsTools verifies that Normalize sorts tools by name in the
// returned manifest.
func TestNormalizeSortsTools(t *testing.T) {
	t.Parallel()

	m := Normalize(Manifest{Tools: []Tool{
		{Name: "zulu", Mode: "remote", Endpoint: "http://z.svc", Transport: "streamable-http"},
		{Name: "alpha", Mode: "remote", Endpoint: "http://a.svc", Transport: "streamable-http"},
		{Name: "mango", Mode: "remote", Endpoint: "http://m.svc", Transport: "streamable-http"},
	}})

	want := []string{"alpha", "mango", "zulu"}
	for i, tool := range m.Tools {
		if tool.Name != want[i] {
			t.Errorf("Tools[%d].Name = %q, want %q", i, tool.Name, want[i])
		}
	}
}

// TestNormalizeDoesNotMutateInput verifies that Normalize does not modify the
// original manifest's Tools slice.
func TestNormalizeDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	original := []Tool{
		{Name: "zulu", Mode: "remote", Endpoint: "http://z.svc", Transport: "streamable-http"},
		{Name: "alpha", Mode: "remote", Endpoint: "http://a.svc", Transport: "streamable-http"},
	}
	input := Manifest{Tools: original}

	Normalize(input)

	// original slice must be unchanged.
	if original[0].Name != "zulu" {
		t.Errorf("Normalize mutated the input slice: original[0].Name = %q, want %q", original[0].Name, "zulu")
	}
}

// TestNormalizeVersionIgnoresClientVersion verifies that the client-supplied
// Version field in the input manifest is ignored; the output version is
// computed from content only.
func TestNormalizeVersionIgnoresClientVersion(t *testing.T) {
	t.Parallel()

	tools := []Tool{
		{Name: "word-count", Mode: "remote", Endpoint: "http://wc.svc", Transport: "streamable-http"},
	}

	withVersion := Normalize(Manifest{Version: "deadbeef", Tools: tools})
	withoutVersion := Normalize(Manifest{Tools: tools})

	if withVersion.Version != withoutVersion.Version {
		t.Errorf(
			"version differs based on client-supplied Version field: %q vs %q (want equal)",
			withVersion.Version, withoutVersion.Version,
		)
	}
}
