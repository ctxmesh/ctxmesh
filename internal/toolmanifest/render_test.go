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

import "testing"

// TestRenderRemoteEndpointVerbatim: a remote binding's URL is carried into the
// manifest endpoint unchanged (it must already carry /mcp).
func TestRenderRemoteEndpointVerbatim(t *testing.T) {
	t.Parallel()

	url := "http://mcp-echo.default.svc.cluster.local/mcp"
	m, sidecars := Render([]Binding{
		{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: url},
	})

	if len(sidecars) != 0 {
		t.Fatalf("remote binding must produce no sidecar tools; got %d", len(sidecars))
	}
	if len(m.Tools) != 1 {
		t.Fatalf("expected 1 tool; got %d", len(m.Tools))
	}
	if m.Tools[0].Endpoint != url {
		t.Errorf("remote endpoint = %q, want verbatim %q", m.Tools[0].Endpoint, url)
	}
	if m.Tools[0].Mode != ModeRemote {
		t.Errorf("mode = %q, want %q", m.Tools[0].Mode, ModeRemote)
	}
	if m.Tools[0].Transport != Transport {
		t.Errorf("transport = %q, want %q", m.Tools[0].Transport, Transport)
	}
}

// TestRenderSidecarPortAssignment: sidecar bindings get deterministic ports
// 3001, 3002, … in binding-NAME order (not input order), and the manifest
// endpoint is http://127.0.0.1:<port>/mcp.
func TestRenderSidecarPortAssignment(t *testing.T) {
	t.Parallel()

	// Supplied out of name order; render must assign by sorted binding name.
	m, sidecars := Render([]Binding{
		{BindingName: "zebra", ToolName: "z-tool", Mode: ModeSidecar, Image: "img-z"},
		{BindingName: "alpha", ToolName: "a-tool", Mode: ModeSidecar, Image: "img-a"},
	})

	if len(sidecars) != 2 {
		t.Fatalf("expected 2 sidecar tools; got %d", len(sidecars))
	}

	// sidecars slice is in binding-name order: alpha → 3001, zebra → 3002.
	if sidecars[0].BindingName != "alpha" || sidecars[0].Port != 3001 {
		t.Errorf("sidecars[0] = {%s, %d}, want {alpha, 3001}", sidecars[0].BindingName, sidecars[0].Port)
	}
	if sidecars[1].BindingName != "zebra" || sidecars[1].Port != 3002 {
		t.Errorf("sidecars[1] = {%s, %d}, want {zebra, 3002}", sidecars[1].BindingName, sidecars[1].Port)
	}
	if sidecars[0].Image != "img-a" || sidecars[1].Image != "img-z" {
		t.Errorf("sidecar images not carried: %q, %q", sidecars[0].Image, sidecars[1].Image)
	}

	// Manifest endpoints: a-tool → :3001/mcp, z-tool → :3002/mcp.
	byName := map[string]Tool{}
	for _, tl := range m.Tools {
		byName[tl.Name] = tl
	}
	if got, want := byName["a-tool"].Endpoint, "http://127.0.0.1:3001/mcp"; got != want {
		t.Errorf("a-tool endpoint = %q, want %q", got, want)
	}
	if got, want := byName["z-tool"].Endpoint, "http://127.0.0.1:3002/mcp"; got != want {
		t.Errorf("z-tool endpoint = %q, want %q", got, want)
	}
	if byName["a-tool"].Mode != ModeSidecar {
		t.Errorf("a-tool mode = %q, want %q", byName["a-tool"].Mode, ModeSidecar)
	}
}

// TestRenderMixedModeDeterministic: mixed remote+sidecar renders the same
// manifest version regardless of input order (port assignment is name-ordered).
func TestRenderMixedModeDeterministic(t *testing.T) {
	t.Parallel()

	in := []Binding{
		{BindingName: "remote-b", ToolName: "rb", Mode: ModeRemote, URL: "http://rb.svc/mcp"},
		{BindingName: "side-a", ToolName: "sa", Mode: ModeSidecar, Image: "img-a"},
		{BindingName: "side-c", ToolName: "sc", Mode: ModeSidecar, Image: "img-c"},
	}
	reversed := []Binding{in[2], in[0], in[1]}

	m1, s1 := Render(in)
	m2, s2 := Render(reversed)

	if m1.Version != m2.Version {
		t.Errorf("manifest version depends on input order: %q vs %q", m1.Version, m2.Version)
	}
	// side-a sorts before side-c → 3001 and 3002 respectively regardless of order.
	if s1[0].Port != s2[0].Port || s1[1].Port != s2[1].Port {
		t.Errorf("port assignment depends on input order: %v vs %v", s1, s2)
	}
	if s1[0].BindingName != "side-a" || s1[0].Port != 3001 {
		t.Errorf("side-a should get 3001; got {%s,%d}", s1[0].BindingName, s1[0].Port)
	}
}

// TestStructuralDigestEmpty: no bindings → empty digest so the caller keeps the
// bare spec-hash revision name.
func TestStructuralDigestEmpty(t *testing.T) {
	t.Parallel()
	if d := StructuralDigest(nil, false); d != "" {
		t.Errorf("no-bindings digest = %q, want empty", d)
	}
}

// TestStructuralDigestManifestOnlyChangeUnchanged is the load-bearing hot-path
// assertion: changing ONLY a remote URL leaves the structural digest unchanged
// (same pod template → same revision name → no restart).
func TestStructuralDigestManifestOnlyChangeUnchanged(t *testing.T) {
	t.Parallel()

	v1 := []Binding{{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: "http://v1.svc/mcp"}}
	v2 := []Binding{{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: "http://v2.svc/mcp"}}

	m1, sc1 := Render(v1)
	m2, sc2 := Render(v2)

	// Sanity: the manifest DID change (hot-path content), proving the URL edit
	// is real — but the structural digest must NOT.
	if m1.Version == m2.Version {
		t.Fatal("manifest version should differ when the remote URL changes")
	}

	d1 := StructuralDigest(sc1, true)
	d2 := StructuralDigest(sc2, true)
	if d1 != d2 {
		t.Errorf("remote-URL-only change altered structural digest: %q vs %q (must be equal — hot path)", d1, d2)
	}
	if d1 == "" {
		t.Error("digest must be non-empty when bindings exist")
	}
}

// TestStructuralDigestStructuralChangeDiffers is the cold-path assertion:
// adding a binding (or changing a sidecar image) changes the structural digest
// → new revision name → pod rolls.
func TestStructuralDigestStructuralChangeDiffers(t *testing.T) {
	t.Parallel()

	base := []Binding{{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: "http://wc.svc/mcp"}}
	added := []Binding{
		{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: "http://wc.svc/mcp"},
		{BindingName: "echo", ToolName: "echo", Mode: ModeSidecar, Image: "img-echo"},
	}

	_, scBase := Render(base)
	_, scAdded := Render(added)

	dBase := StructuralDigest(scBase, true)
	dAdded := StructuralDigest(scAdded, true)
	if dBase == dAdded {
		t.Errorf("adding a sidecar binding did not change the structural digest: both %q", dBase)
	}

	// Also: changing only a sidecar image must change the digest.
	sameShapeDiffImg := []Binding{
		{BindingName: "wc", ToolName: "word-count", Mode: ModeRemote, URL: "http://wc.svc/mcp"},
		{BindingName: "echo", ToolName: "echo", Mode: ModeSidecar, Image: "img-echo-v2"},
	}
	_, scImg := Render(sameShapeDiffImg)
	if StructuralDigest(scImg, true) == dAdded {
		t.Error("changing a sidecar image did not change the structural digest")
	}
}

// TestStructuralDigestOrderIndependent: the same binding set in different input
// orders yields the same digest.
func TestStructuralDigestOrderIndependent(t *testing.T) {
	t.Parallel()

	a := []Binding{
		{BindingName: "b1", ToolName: "t1", Mode: ModeSidecar, Image: "i1"},
		{BindingName: "b2", ToolName: "t2", Mode: ModeSidecar, Image: "i2"},
	}
	b := []Binding{a[1], a[0]}

	_, sa := Render(a)
	_, sb := Render(b)
	if StructuralDigest(sa, true) != StructuralDigest(sb, true) {
		t.Error("structural digest depends on input order")
	}
}
