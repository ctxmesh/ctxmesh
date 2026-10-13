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
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// The schemas use a small JSON Schema subset, because the same validator is re-implemented in each
// SDK's generated fake and every keyword is six implementations. CheckSchema refuses anything else,
// so a keyword one implementation ignores can never make it into the contract.
var schemaKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "minLength": true, "maxLength": true, "description": true,
}

// Decode parses JSON keeping numbers exact, the form Validate expects.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return v, nil
}

// CheckSchema reports keywords outside the supported subset, and malformed values of supported ones.
func CheckSchema(schema json.RawMessage) []string {
	s, err := Decode(schema)
	if err != nil {
		return []string{"schema is not JSON: " + err.Error()}
	}
	return checkSchema(s, "#")
}

func checkSchema(s any, at string) []string {
	obj, ok := s.(map[string]any)
	if !ok {
		return []string{at + ": a schema must be an object"}
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !schemaKeywords[k] {
			out = append(out, fmt.Sprintf("%s: keyword %q is outside the supported subset", at, k))
		}
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(props)) {
			out = append(out, checkSchema(props[k], at+"/properties/"+k)...)
		}
	}
	if items, ok := obj["items"]; ok {
		out = append(out, checkSchema(items, at+"/items")...)
	}
	if ap, ok := obj["additionalProperties"]; ok {
		if _, isBool := ap.(bool); !isBool {
			out = append(out, checkSchema(ap, at+"/additionalProperties")...)
		}
	}
	return out
}

// Validate checks a decoded value against schema and returns one message per violation, each
// naming where in the value it is.
func Validate(schema json.RawMessage, value any) []string {
	s, err := Decode(schema)
	if err != nil {
		return []string{"schema is not JSON: " + err.Error()}
	}
	return validate(s, value, "$")
}

func validate(schema, v any, at string) []string {
	s, ok := schema.(map[string]any)
	if !ok {
		return []string{at + ": malformed schema"}
	}
	if t, ok := s["type"]; ok && !typeMatches(t, v) {
		return []string{fmt.Sprintf("%s: is %s, want %s", at, jsonType(v), typeList(t))}
	}
	var out []string
	if enum, ok := s["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return jsonEqual(e, v) }) {
		out = append(out, fmt.Sprintf("%s: %s is not one of %s", at, render(v), render(enum)))
	}
	if str, ok := v.(string); ok {
		out = append(out, validateString(s, str, at)...)
	}
	if obj, ok := v.(map[string]any); ok {
		out = append(out, validateObject(s, obj, at)...)
	}
	if arr, ok := v.([]any); ok {
		if items, ok := s["items"]; ok {
			for i, e := range arr {
				out = append(out, validate(items, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return out
}

func validateString(s map[string]any, str, at string) []string {
	var out []string
	n := int64(utf8.RuneCountInString(str))
	if v, ok := s["minLength"].(json.Number); ok {
		if want, _ := v.Int64(); n < want {
			out = append(out, fmt.Sprintf("%s: %q is shorter than %d", at, str, want))
		}
	}
	if v, ok := s["maxLength"].(json.Number); ok {
		if want, _ := v.Int64(); n > want {
			out = append(out, fmt.Sprintf("%s: %q is longer than %d", at, str, want))
		}
	}
	return out
}

func validateObject(s map[string]any, obj map[string]any, at string) []string {
	var out []string
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if name, _ := k.(string); name != "" {
				if _, present := obj[name]; !present {
					out = append(out, fmt.Sprintf("%s: missing required property %q", at, name))
				}
			}
		}
	}
	props, _ := s["properties"].(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if ps, ok := props[k]; ok {
			out = append(out, validate(ps, obj[k], at+"."+k)...)
			continue
		}
		switch ap := s["additionalProperties"].(type) {
		case bool:
			if !ap {
				out = append(out, fmt.Sprintf("%s: property %q is not in the contract", at, k))
			}
		case map[string]any:
			out = append(out, validate(ap, obj[k], at+"."+k)...)
		}
	}
	return out
}

func typeMatches(t, v any) bool {
	switch tt := t.(type) {
	case string:
		return isType(tt, v)
	case []any:
		return slices.ContainsFunc(tt, func(e any) bool { s, _ := e.(string); return isType(s, v) })
	}
	return false
}

func isType(t string, v any) bool {
	switch t {
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	case "number":
		_, ok := v.(json.Number)
		return ok
	default:
		return jsonType(v) == t
	}
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func typeList(t any) string {
	if l, ok := t.([]any); ok {
		parts := make([]string, 0, len(l))
		for _, e := range l {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, " or ")
	}
	return fmt.Sprint(t)
}

// jsonEqual compares two decoded JSON values, treating numbers by value rather than spelling.
func jsonEqual(a, b any) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok && bok {
		af, aerr := an.Float64()
		bf, berr := bn.Float64()
		return aerr == nil && berr == nil && af == bf
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, present := bv[k]
			if !present || !jsonEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// JSONEqual reports whether two JSON documents are equal as values.
func JSONEqual(a, b []byte) (bool, error) {
	av, err := Decode(a)
	if err != nil {
		return false, err
	}
	bv, err := Decode(b)
	if err != nil {
		return false, err
	}
	return jsonEqual(av, bv), nil
}

func render(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
