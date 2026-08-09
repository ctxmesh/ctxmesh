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

package workflow

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"google.golang.org/protobuf/types/known/structpb"
)

// Evaluator is the RUNTIME CEL evaluator the m67.3 executor uses to compute a workflow's edges + input
// bindings against prior node outputs. It shares the EXACT SAME env as validation (newCELEnv) — the two
// available top-level variables are `input` (the workflow input) and `steps` (a map of `<name>.output`) —
// so an expression the m67.1 controller accepted evaluates here without surprise. Reusing the one env is the
// point of the k8s-client-free validation lib: compile-time (the controller) and run-time (the executor)
// agree by construction.
//
// An Evaluator holds no per-run state; it is safe to build once per executor pass and reuse across a node's
// bindings. Compiles are done lazily per expression (v1a workflows are small — a handful of expressions per
// node — so a per-pass cache buys little; correctness over a micro-optimization).
type Evaluator struct {
	env *cel.Env
}

// NewEvaluator builds a runtime evaluator over the shared workflow CEL env. It errors only if the env cannot
// be constructed (a programming error, not a spec error) — the same env validation already built.
func NewEvaluator() (*Evaluator, error) {
	env, err := newCELEnv()
	if err != nil {
		return nil, fmt.Errorf("workflow: building the runtime CEL env: %w", err)
	}
	return &Evaluator{env: env}, nil
}

// Activation is the runtime data an expression is evaluated against: the workflow `input` and each prior
// node's `steps.<name>.output`. Both are plain Go maps/values (decoded from the node outputs' JSON) — CEL's
// native map/list/scalar handling drives field selection, indexing, and the exists()/size() macros over them.
type Activation struct {
	// Input is the workflow's input, decoded to a Go value (typically map[string]any). Bound to `input`.
	Input any
	// Outputs maps a prior node name to its decoded output value. Bound to `steps`, each as `{output: <v>}`.
	Outputs map[string]any
}

// vars renders the activation as the CEL variable bindings the shared env declares: `input` and `steps`
// (a map of `<name> -> {output: <decoded output>}`). A nil Input binds an empty map so a reference to an
// absent field is a CEL no-such-field error (honest), not a Go panic.
func (a Activation) vars() map[string]any {
	steps := make(map[string]any, len(a.Outputs))
	for name, out := range a.Outputs {
		steps[name] = map[string]any{"output": out}
	}
	in := a.Input
	if in == nil {
		in = map[string]any{}
	}
	return map[string]any{"input": in, "steps": steps}
}

// EvalBool compiles + evaluates a CEL boolean predicate (a branch `when`) against the activation. A non-bool
// result is a hard error (a `when` MUST be a predicate) — never coerced — so a malformed edge fails the node
// rather than silently taking a branch. A compile error is also returned (the controller already compiled it,
// but the executor validates defensively; the snapshot is pinned so it should match).
func (e *Evaluator) EvalBool(expr string, act Activation) (bool, error) {
	out, err := e.eval(expr, act)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("workflow: expression %q did not evaluate to a bool (got %s)", expr, out.Type().TypeName())
	}
	return b, nil
}

// EvalAny compiles + evaluates a CEL expression (a node `input` binding) against the activation and returns
// the result as a native Go value (via ref.Val → Go conversion), ready to be marshalled into the sub-run's
// input JSON. Errors on compile or a runtime error (e.g. selecting a field of a null, or an unknown step —
// the validator forbids the latter, but the executor is defensive).
func (e *Evaluator) EvalAny(expr string, act Activation) (any, error) {
	out, err := e.eval(expr, act)
	if err != nil {
		return nil, err
	}
	// Convert via structpb.Value → AsInterface: this yields a JSON-FAITHFUL Go value (map[string]any / []any /
	// string / float64 / bool / nil) — the shape json.Marshal renders into the sub-agent's /invoke input. A raw
	// ConvertToNative(any) on a CEL map literal returns map[ref.Val]ref.Val (not JSON-marshalable), so the
	// structpb bridge is the correct path for the input-binding use (a CEL value → JSON value).
	native, err := out.ConvertToNative(structpbValueType)
	if err != nil {
		return nil, fmt.Errorf("workflow: expression %q result is not representable as a JSON value: %w", expr, err)
	}
	pv, ok := native.(*structpb.Value)
	if !ok {
		return nil, fmt.Errorf("workflow: expression %q converted to an unexpected type %T", expr, native)
	}
	return pv.AsInterface(), nil
}

// eval is the shared compile + program + eval path. It returns the raw ref.Val so callers apply their own
// result typing (bool vs any). A CEL runtime error (types.Err) is lifted to a Go error, never returned as a
// value — the executor must see an eval failure as a failure.
func (e *Evaluator) eval(expr string, act Activation) (ref.Val, error) {
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("workflow: CEL expression %q failed to compile: %w", expr, iss.Err())
	}
	prg, err := e.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("workflow: building the CEL program for %q: %w", expr, err)
	}
	out, _, err := prg.Eval(act.vars())
	if err != nil {
		return nil, fmt.Errorf("workflow: evaluating %q: %w", expr, err)
	}
	if types.IsError(out) {
		return nil, fmt.Errorf("workflow: evaluating %q: %v", expr, out)
	}
	return out, nil
}

// structpbValueType is the reflect target for ConvertToNative used by EvalAny: converting a CEL result to a
// *structpb.Value and then calling AsInterface() yields a JSON-faithful Go value (the sub-run input shape),
// unlike a raw any-conversion which returns map[ref.Val]ref.Val for map literals.
var structpbValueType = reflect.TypeFor[*structpb.Value]()
