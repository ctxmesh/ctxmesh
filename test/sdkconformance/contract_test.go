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

package sdkconformance

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ctxmesh/ctxmesh/internal/bff"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory/recall"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge/retrieval"
)

func load(t *testing.T) *Contract {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := LoadFromRepo(wd)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheContractAgreesWithItself(t *testing.T) {
	for _, problem := range load(t).Check() {
		t.Error(problem)
	}
}

// The stubs answer for the real upstreams, so their canned bodies must be shapes those upstreams
// produce. Decoding strictly into the upstream's own types catches a renamed or invented field.
func TestTheWorldStubsSpeakTheRealUpstreamShapes(t *testing.T) {
	up := load(t).World.Upstreams
	cases := []struct {
		upstream, key string
		into          any
	}{
		{UpstreamTokenService, "POST /v1/memory/search", &struct {
			Results []recall.Hit `json:"results"`
		}{}},
		{UpstreamTokenService, "POST /v1/knowledge/search", &struct {
			Results []retrieval.Hit `json:"results"`
		}{}},
		{UpstreamBFF, "POST /api/internal/spawn", &bff.SpawnRunResponse{}},
		{UpstreamBFF, "GET /api/internal/runs/{id}", &bff.SpawnedRunStatus{}},
		{UpstreamBFF, "POST /api/internal/handoff", &bff.HandoffRunResponse{}},
	}
	for _, tc := range cases {
		c, ok := up[tc.upstream][tc.key]
		if !ok {
			t.Errorf("world.upstreams.%s has no %q", tc.upstream, tc.key)
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(c.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(tc.into); err != nil {
			t.Errorf("world.upstreams.%s[%s] is not what the real upstream sends: %v", tc.upstream, tc.key, err)
		}
	}
}

// The validator is re-implemented in every SDK fake, so its semantics are pinned here.
func TestValidatorSemantics(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["a"],"additionalProperties":false,"properties":{
		"a":{"type":"string","minLength":1,"maxLength":3},
		"n":{"type":"integer"},
		"e":{"enum":["x","y"]},
		"l":{"type":"array","items":{"type":["number","null"]}},
		"m":{"type":"object","additionalProperties":{"type":"string"}}}}`)
	cases := []struct {
		doc  string
		want string // a substring of the single expected violation, "" for none
	}{
		{`{"a":"ok","n":3,"e":"x","l":[1,null,2.5],"m":{"k":"v"}}`, ""},
		{`{"a":"ok","n":3.0}`, ""},
		{`{"n":1}`, `missing required property "a"`},
		{`{"a":""}`, "shorter than 1"},
		{`{"a":"long"}`, "longer than 3"},
		{`{"a":"ok","n":1.5}`, "want integer"},
		{`{"a":"ok","e":"z"}`, "is not one of"},
		{`{"a":"ok","l":["s"]}`, "want number or null"},
		{`{"a":"ok","m":{"k":1}}`, "want string"},
		{`{"a":"ok","typo":1}`, `property "typo" is not in the contract`},
		{`[]`, "want object"},
	}
	for _, tc := range cases {
		v, err := Decode([]byte(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		got := Validate(schema, v)
		switch {
		case tc.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected violations %v", tc.doc, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: got %v, want one violation containing %q", tc.doc, got, tc.want)
		}
	}
	if bad := CheckSchema(json.RawMessage(`{"type":"string","pattern":"^a"}`)); len(bad) != 1 {
		t.Errorf("a keyword outside the subset must be refused, got %v", bad)
	}
}

func TestResolve(t *testing.T) {
	doc, err := Decode([]byte(`{"ok":true,"results":[{"content":"a"},{"content":"b"}],"empty":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"$": true, "ok": true, "results[].content": true,
		"answer": false, "results[].score": false, "empty[].content": false, "ok[].x": false,
	} {
		if got := Resolve(doc, path); got != want {
			t.Errorf("Resolve(%q) = %t, want %t", path, got, want)
		}
	}
}
