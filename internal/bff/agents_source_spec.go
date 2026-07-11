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
	"fmt"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ctxmesh/agent-engine/internal/expand"
)

// agentDeploymentKind is the CRD kind of the primary object a console create
// produces. The source-spec annotation is stamped on this object only — never on
// the generated MCPToolBindings / EvalSuite / PromptVersion, which are derived
// state, not the user's intent (ADR 0017).
const agentDeploymentKind = "AgentDeployment"

// maxSourceSpecBytes bounds the canonical-JSON source-spec stored in the
// annotation. Kubernetes caps the TOTAL size of all annotations on an object at
// ~256KB; we reserve well under half for this single annotation so it can never
// be the thing that pushes an object over the ceiling, and reject an oversize
// spec with a teaching error rather than store a truncated (corrupt) round-trip.
// A normal simplified spec is a few KB.
const maxSourceSpecBytes = 128 * 1024

// secretFieldNames are the well-known credential field names whose presence
// ANYWHERE in the submitted simplified spec means inline credential material was
// supplied. The simplified spec must reference secrets by NAME (a SecretBinding),
// never carry the value inline — annotations are readable by anyone with `get` on
// the resource (ADR 0017 §2, the CLAUDE.md no-secrets rule). The match is on the
// KEY name; we deliberately keep this list to unambiguous credential names so an
// ordinary field is not over-rejected.
var secretFieldNames = map[string]bool{
	"apikey":   true, // apiKey
	"token":    true,
	"secret":   true,
	"password": true,
	"bearer":   true,
}

// canonicalizeSourceSpec converts the submitted simplified spec (YAML bytes) to
// canonical JSON so it round-trips deterministically when a later PUT re-expands
// it (ADR 0017). It first inspects the parsed structure for inline credential
// material and for size, returning a *createError (4xx, teaching) on either — no
// annotation is stored in those cases. sigs.k8s.io/yaml.YAMLToJSON produces a
// stable, key-sorted JSON encoding (it round-trips YAML through encoding/json),
// which is what makes the stored form canonical.
func canonicalizeSourceSpec(agentYAML []byte) (string, *createError) {
	// Parse to a generic tree first so we can walk it for inline secrets. A parse
	// failure here is redundant with expand's own parse (expand runs on the same
	// bytes), but we surface it as a 400 rather than panic on malformed input.
	var tree any
	if err := sigsyaml.Unmarshal(agentYAML, &tree); err != nil {
		return "", &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}
	if path := findInlineSecret(tree, ""); path != "" {
		return "", &createError{
			status: 400,
			msg: fmt.Sprintf(
				"inline secrets are not allowed (found credential field %q); reference a SecretBinding by name instead",
				path,
			),
		}
	}

	canonical, err := sigsyaml.YAMLToJSON(agentYAML)
	if err != nil {
		return "", &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}
	if len(canonical) > maxSourceSpecBytes {
		return "", &createError{
			status: 400,
			msg: fmt.Sprintf(
				"agent spec too large to store for editing (%d bytes; limit %d)",
				len(canonical), maxSourceSpecBytes,
			),
		}
	}
	return string(canonical), nil
}

// findInlineSecret walks the parsed simplified spec looking for inline credential
// material and returns the dotted path to the first offending field (empty string
// = clean). Two patterns are rejected:
//
//   - a MAP KEY that is a well-known credential name (apiKey/token/secret/
//     password/bearer) carrying a non-empty SCALAR value — an inline credential
//     literal (e.g. `apiKey: sk-123`);
//   - a credential-named key whose value is a MAP carrying a literal `value:`
//     (e.g. `secret: {name: db, value: raw-pw}`) — the pattern of embedding a raw
//     value under a key/secret field where only a by-name reference belongs.
//
// A credential key whose value is a MAP that carries NO literal `value:`
// (e.g. `apiKey: {secretName: foo}` — a structured by-name reference) is NOT
// rejected. This keeps the check conservative: it fires on literal inline values,
// not on by-name references that legitimately use these keys.
func findInlineSecret(node any, path string) string {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			childPath := joinPath(path, key)
			if secretFieldNames[strings.ToLower(key)] {
				// A credential-named key holding a literal scalar → inline secret.
				if isNonEmptyScalar(child) {
					return childPath
				}
				// A credential-named key whose block embeds a literal `value:` →
				// inline secret (a raw value where only a name reference belongs).
				if m, ok := child.(map[string]any); ok {
					if val, has := m["value"]; has && isNonEmptyScalar(val) {
						return joinPath(childPath, "value")
					}
				}
			}
			if found := findInlineSecret(child, childPath); found != "" {
				return found
			}
		}
	case []any:
		for i, child := range v {
			if found := findInlineSecret(child, fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	}
	return ""
}

// isNonEmptyScalar reports whether v is a non-empty scalar (string/number/bool) —
// i.e. a literal inline value rather than a nested structure or an empty field. A
// map or slice value is a structured reference shape, not an inline literal.
func isNonEmptyScalar(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case map[string]any, []any:
		return false
	default:
		// Numbers, bools — any non-nil scalar counts as a literal value.
		return true
	}
}

// joinPath joins a dotted field path, tolerating an empty base (the root).
func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// stampSourceSpec sets the source-spec annotation (canonical JSON of the
// submitted simplified spec) on the primary AgentDeployment among the decoded
// objects (ADR 0017). It touches ONLY the AgentDeployment — the generated
// bindings/versions are derived state and must not carry the annotation. It is a
// no-op guarded by the caller: canonicalizeSourceSpec has already validated the
// spec (no inline secrets, within the size limit) by the time this runs.
func stampSourceSpec(objs []decodedObject, canonicalJSON string) {
	for _, o := range objs {
		if o.kind != agentDeploymentKind {
			continue
		}
		ann := o.obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[expand.AnnotationSourceSpec] = canonicalJSON
		o.obj.SetAnnotations(ann)
	}
}
