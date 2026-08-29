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

// Package workflow holds the SHARED, k8s-client-free validation library for the Workflow graph (M67,
// ADR 0060). It validates a WorkflowSpec's structure, compiles every CEL expression, and enforces the
// load-bearing rule — any step whose output is referenced by another step's when/input/map.over MUST pin an
// outputSchema (CEL over a typed object, not free text).
//
// This package deliberately imports ONLY the API types + cel-go — NO controller-runtime, NO k8s client. That
// is what lets BOTH the CRD controller (m67.1) AND the BFF/executor (planning mode, ADR 0060 §6) call
// Validate on the same spec value: the executor validates a runtime-generated inline spec that never becomes
// an etcd object, so the validator must not depend on a live API server. Keep it that way.
package workflow

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// Result is the richer outcome of a validation pass. Err is nil iff the spec is valid; when non-nil it is the
// first (and, in Errors, every) problem found. Start is the resolved start step name (the first step) when the
// graph is structurally determinable.
type Result struct {
	// Err is the first validation error (nil ⇒ valid). It is what Validate returns.
	Err error
	// Errors lists every problem found (structural + CEL + the outputSchema rule), for surfacing all at once.
	Errors []error
	// Start is the resolved start step (the first step's name) when steps are non-empty.
	Start string
}

// Validate is the single entry point the CRD controller and the BFF/executor share. It returns nil iff spec
// is a valid workflow graph; otherwise the first problem (naming the offending step + expression). Use
// ValidateResult for every problem at once.
func Validate(spec agentsv1beta1.WorkflowSpec) error {
	return ValidateResult(spec).Err
}

// ValidateResult runs the full validation pass and returns every problem found (Err is the first). The pass
// is three phases, each factored into its own helper: (1) structure (names, edges, bounds), (2) CEL compile,
// (3) the referenced-output⇒outputSchema rule. Phases 2+3 run only once the name set is known.
func ValidateResult(spec agentsv1beta1.WorkflowSpec) Result {
	var res Result

	// ── 1. Structural ────────────────────────────────────────────────────────
	if len(spec.Steps) == 0 {
		res.add(fmt.Errorf("workflow has no steps; a workflow needs at least one step"))
		return res.finish()
	}
	names := res.checkNames(spec)
	// The start step is the first (a determinable start). With unique, non-empty names present it is
	// well-defined; report it even if later checks fail, for the caller's use.
	if spec.Steps[0].Name != "" {
		res.Start = spec.Steps[0].Name
	}
	for i := range spec.Steps {
		res.checkStep(&spec.Steps[i], names)
	}

	// ── 2. CEL compile + 3. the referenced-output⇒outputSchema rule ──────────
	res.checkExpressions(spec, names)

	// ── 4. Static spawn-budget guard (the map-bomb / runaway-loop lower bound) ──
	res.checkBudget(spec)

	return res.finish()
}

// checkBudget rejects a spec whose STATICALLY-KNOWABLE worst-case launch count exceeds the workflow's total
// spawn budget (ADR 0060 "Validate statically what's knowable"). A map's list size is a RUNTIME value (its
// dynamic ReserveSpawn is the real backstop), but a loop's `maxIterations` is known at author time: each
// iteration is one node launch, so the sum of every loop's maxIterations is a conservative lower bound on the
// total launches. When a budget is set (MaxTotalSpawns > 0) and that lower bound already exceeds it, the
// workflow can NEVER complete without hitting the budget — reject it at authoring time rather than let it
// fail-fast at runtime. A workflow with no loops (or no budget) passes this cheaply; the dynamic per-root
// counter still backstops maps + everything else.
func (r *Result) checkBudget(spec agentsv1beta1.WorkflowSpec) {
	if spec.Budget == nil || spec.Budget.MaxTotalSpawns <= 0 {
		return // no total budget configured → nothing to statically bound (the CRD defaults it for real specs).
	}
	// Every non-loop step is at least one launch when reached; every loop contributes up to maxIterations
	// launches. We do not model reachability (a conservative over-count is the safe direction for a guard —
	// but we also must not reject a legal linear graph), so we bound only the KNOWABLE amplifier: the loops.
	// The lower bound = (# non-loop steps that launch) + Σ loop.maxIterations. A loop step's own single "slot"
	// is subsumed by its iterations, so count it once via maxIterations.
	var lowerBound int64
	for i := range spec.Steps {
		s := &spec.Steps[i]
		if s.Loop != nil {
			lowerBound += int64(s.Loop.MaxIterations)
			continue
		}
		lowerBound++ // a plain/conditional/map step is at least one launch (a map's fan-out is runtime).
	}
	if lowerBound > int64(spec.Budget.MaxTotalSpawns) {
		r.add(fmt.Errorf(
			"workflow's worst-case static launch count (%d, from loop maxIterations + steps) exceeds its spawn budget maxTotalSpawns=%d; raise the budget or lower maxIterations (ADR 0060 map-bomb guard)",
			lowerBound, spec.Budget.MaxTotalSpawns))
	}
}

// checkNames builds the unique, non-empty step-name set (the reference target space), recording a problem per
// empty or duplicate name.
func (r *Result) checkNames(spec agentsv1beta1.WorkflowSpec) map[string]bool {
	names := make(map[string]bool, len(spec.Steps))
	for i := range spec.Steps {
		n := spec.Steps[i].Name
		switch {
		case n == "":
			r.add(fmt.Errorf("step[%d] has an empty name", i))
		case names[n]:
			r.add(fmt.Errorf("duplicate step name %q", n))
		default:
			names[n] = true
		}
	}
	return names
}

// checkStep validates one step's control flow: exactly one construct (next / branches / map / loop), no
// dangling edges (every target names an existing step), and sane map/loop bounds.
func (r *Result) checkStep(s *agentsv1beta1.WorkflowStep, names map[string]bool) {
	// edgeRef records a dangling-edge problem when a non-empty target names no existing step.
	edgeRef := func(kind, to string) {
		if to == "" { // "" = terminal / no fallthrough — a legal non-edge.
			return
		}
		if !names[to] {
			r.add(fmt.Errorf("step %q %s references unknown step %q (dangling edge)", s.Name, kind, to))
		}
	}

	// Exactly one control-flow construct; empty `next` with no other construct = a terminal step (legal).
	set := 0
	for _, present := range []bool{len(s.Branches) > 0, s.Map != nil, s.Loop != nil, s.Next != ""} {
		if present {
			set++
		}
	}
	if set > 1 {
		r.add(fmt.Errorf("step %q sets more than one control-flow construct (use exactly one of next / branches / map / loop)", s.Name))
	}

	edgeRef("next", s.Next)
	edgeRef("default", s.Default)
	for bi := range s.Branches {
		edgeRef(fmt.Sprintf("branches[%d].to", bi), s.Branches[bi].To)
	}
	// onError is a real edge: its handler target must name an existing step (like next/default/to). It is a
	// ROUTE-ONLY error handler for PLAIN nodes — map/loop nodes keep their fail-fast behavior, so onError on a
	// map/loop node is rejected (m83.3). A `next`/`branches` graph can already cycle (validate does no cycle
	// analysis — see the package/checkBudget notes); the onError edge is bounded at runtime by the SAME per-root
	// spawn budget (reserveNodeSpawn) that backstops every other edge, so no separate static cycle guard is added.
	edgeRef("onError", s.OnError)
	if s.OnError != "" && (s.Map != nil || s.Loop != nil) {
		r.add(fmt.Errorf("step %q sets onError on a map/loop node; onError is route-only on plain (sequential/conditional) nodes (m83.3)", s.Name))
	}
	// catch (M138, ADR 0109): ordered error-class catchers on a PLAIN node. Each `next` must name an
	// existing step, and each error code must be a platform-reserved code or the "*" wildcard (an unknown
	// code would silently never match — reject it at validation, not discover it at runtime).
	if len(s.Catch) > 0 && (s.Map != nil || s.Loop != nil) {
		r.add(fmt.Errorf("step %q sets catch on a map/loop node; catch is route-only on plain nodes (ADR 0109)", s.Name))
	}
	for ci := range s.Catch {
		edgeRef(fmt.Sprintf("catch[%d].next", ci), s.Catch[ci].Next)
		for _, code := range s.Catch[ci].Errors {
			if !run.IsPlatformFailureCode(code) {
				r.add(fmt.Errorf("step %q catch[%d] has unknown error code %q (use a platform failure code or \"*\")", s.Name, ci, code))
			}
		}
	}
	if s.Map != nil {
		edgeRef("map.do", s.Map.Do)
		edgeRef("map.join", s.Map.Join)
		if s.Map.Parallelism < 1 {
			r.add(fmt.Errorf("step %q map.parallelism must be >= 1; got %d", s.Name, s.Map.Parallelism))
		}
		// completion is enum-bounded by the CRD (all|any, default all); defend it here too so a
		// snapshot/direct-apply that bypassed the CRD enum can't drive an unknown wake mode (m83.4, ADR
		// 0075 §1). Empty = the CRD default (all). completion:any is compatible with a `join`: the winning
		// item's output is fed forward as a single-element list, which a join step consumes exactly like an
		// all-collect list — so no extra any+join restriction is needed.
		switch s.Map.Completion {
		case "", "all", "any":
			// ok.
		default:
			r.add(fmt.Errorf("step %q map.completion must be one of all|any; got %q", s.Name, s.Map.Completion))
		}
	}
	if s.Loop != nil {
		edgeRef("loop.do", s.Loop.Do)
		if s.Loop.MaxIterations <= 0 {
			r.add(fmt.Errorf("step %q loop.maxIterations must be > 0; got %d", s.Name, s.Loop.MaxIterations))
		}
	}
}

// checkExpressions compiles every CEL expression in the spec (input bindings, branch predicates, map.over,
// loop.until) and enforces the load-bearing rule: any step whose output is referenced by an expression MUST
// pin an outputSchema. env-build / compile errors and the rule violations are recorded as problems.
func (r *Result) checkExpressions(spec agentsv1beta1.WorkflowSpec, names map[string]bool) {
	// Build the CEL env with the available variables: `input` (the workflow input) and `steps` (a map of
	// per-step { output: <dyn> }). Both are dynamic maps — the output objects are typed only conceptually by
	// their pinned outputSchema (we do not synthesize a CEL type from the JSON Schema in v1; the rule is that
	// a referenced step MUST pin one so CEL is not running over free text). Type errors we *can* catch
	// (unknown top-level variable, bad operator arity) are caught by Compile's type-check.
	env, err := newCELEnv()
	if err != nil {
		r.add(fmt.Errorf("building the CEL environment: %w", err))
		return
	}

	// referenced collects every step whose output is referenced by some expression (with a human-readable
	// site for the error message).
	referenced := map[string]string{} // step name -> first referencing site
	compile := func(site, expr string) {
		if expr == "" {
			return
		}
		ast, iss := env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			r.add(fmt.Errorf("%s: CEL expression %q failed to compile: %w", site, expr, iss.Err()))
			return
		}
		// Collect steps.<name>.output references from the compiled AST (structural, robust to whitespace /
		// method calls / indexing).
		for _, ref := range stepOutputRefs(ast) {
			if _, ok := referenced[ref]; !ok {
				referenced[ref] = site
			}
			if !names[ref] { // a reference to a nonexistent step is a dangling reference.
				r.add(fmt.Errorf("%s: references output of unknown step %q", site, ref))
			}
		}
	}

	for i := range spec.Steps {
		s := &spec.Steps[i]
		for _, k := range sortedKeys(s.Input) { // deterministic order for stable error reporting.
			compile(fmt.Sprintf("step %q input[%q]", s.Name, k), s.Input[k])
		}
		for bi := range s.Branches {
			compile(fmt.Sprintf("step %q branches[%d].when", s.Name, bi), s.Branches[bi].When)
		}
		if s.Map != nil {
			compile(fmt.Sprintf("step %q map.over", s.Name), s.Map.Over)
		}
		if s.Loop != nil {
			compile(fmt.Sprintf("step %q loop.until", s.Name), s.Loop.Until)
		}
	}

	// The load-bearing rule (ADR 0060): any step whose output is referenced MUST pin an outputSchema.
	hasSchema := make(map[string]bool, len(spec.Steps))
	for i := range spec.Steps {
		s := &spec.Steps[i]
		hasSchema[s.Name] = s.OutputSchema != nil && len(s.OutputSchema.Raw) > 0
	}
	for _, ref := range sortedKeys(referenced) {
		if names[ref] && !hasSchema[ref] {
			r.add(fmt.Errorf(
				"step %q output is referenced by %s but step %q pins no outputSchema; a referenced output MUST declare an outputSchema so CEL runs over a typed object (ADR 0060)",
				ref, referenced[ref], ref))
		}
	}
}

// newCELEnv builds the CEL environment shared by validation. Variables:
//
//	input : dyn  — the workflow input (typed conceptually by inputSchema; a dynamic map here).
//	steps : map(string, dyn) — steps.<name>.output for each prior step (each output is dyn until its
//	                           outputSchema pins it; the referenced-output rule enforces that a pin exists).
//	error : dyn  — the failure object {node, message, type} in a CATCH handler (M138, ADR 0109); an empty
//	               map in any other node, so a reference is an honest no-such-field.
//
// Kept dyn on purpose: synthesizing a CEL struct type from an arbitrary JSON Schema is a v1c concern; v1
// guarantees typedness via the outputSchema rule, not via CEL type synthesis. Compile still catches unknown
// top-level identifiers and arity/operator type errors.
func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("input", cel.DynType),
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("error", cel.DynType),
	)
}

// stepOutputRefs walks a compiled CEL AST and returns the names of every step whose `.output` is selected,
// i.e. every `steps.<name>.output...` reference. The detection is STRUCTURAL, not textual: for the expression
// `steps.classify.output.topic` the checked AST is a chain of field selections
//
//	Select(field="topic", Select(field="output", Select(field="classify", Ident("steps"))))
//
// so we look for a Select whose FieldName == "output" and whose operand is itself
// Select(field=<stepName>, operand=Ident("steps")). This is robust to whitespace, further chained selects
// (`.output.topic`), indexing (`.output.items[0]`), and method calls (`.output.tags.exists(...)`), because we
// only match the `steps.<name>.output` sub-expression wherever it appears in the tree.
func stepOutputRefs(ast *cel.Ast) []string {
	seen := map[string]bool{}
	celast.PostOrderVisit(ast.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() != celast.SelectKind {
			return
		}
		sel := e.AsSelect() // e = <operand>.output
		if sel.FieldName() != "output" {
			return
		}
		inner := sel.Operand()
		if inner.Kind() != celast.SelectKind {
			return
		}
		innerSel := inner.AsSelect() // inner = steps.<name>
		root := innerSel.Operand()
		if root.Kind() != celast.IdentKind || root.AsIdent() != "steps" {
			return
		}
		seen[innerSel.FieldName()] = true
	}))
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// sortedKeys returns the map's keys in deterministic order (stable error reporting).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// add appends a problem to the result.
func (r *Result) add(err error) { r.Errors = append(r.Errors, err) }

// finish sets Err to the first problem (nil when valid) and returns the result by value.
func (r *Result) finish() Result {
	if len(r.Errors) > 0 {
		r.Err = r.Errors[0]
	}
	return *r
}

// Error renders a Result's problems as a single multi-line string (convenience for callers that log the whole
// set rather than just Err).
func (r Result) Error() string {
	if len(r.Errors) == 0 {
		return ""
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}
