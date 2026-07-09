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

// Package main contains the agent-engine CLI expand command.
// expand reads a simplified agent.yaml (PRD §8.5 M2 subset) and prints the
// fully-expanded AgentDeployment CRD manifest to stdout.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// knownFields is the set of top-level agent.yaml fields supported in M2+M8+M9.
var knownFields = map[string]bool{
	"name":           true,
	"image":          true,
	"executionModel": true,
	"resources":      true,
	"scaling":        true,
	"model":          true,
	"budget":         true,
	"eval":           true,
	"prompt":         true,
}

// futureField describes a top-level field not yet supported, with the milestone
// where it lands.
type futureField struct {
	milestone string
}

// futureFields is the set of recognised-but-not-yet-supported top-level fields.
// Fields not in knownFields and not in futureFields are fully unknown → hard error.
var futureFields = map[string]futureField{
	"tools":    {milestone: "M4"},
	"memory":   {milestone: "M5"},
	"registry": {milestone: "M6"},
}

// ── Input types ───────────────────────────────────────────────────────────────

// agentYAML is the M2+M8+M9 subset of the simplified PRD §8.5 agent.yaml format.
type agentYAML struct {
	Name           string         `yaml:"name"`
	Image          string         `yaml:"image"`
	ExecutionModel string         `yaml:"executionModel"`
	Resources      *resourcesYAML `yaml:"resources"`
	Scaling        *scalingYAML   `yaml:"scaling"`
	Model          *modelYAML     `yaml:"model"`
	Budget         *budgetYAML    `yaml:"budget"`
	Eval           *evalYAML      `yaml:"eval"`
	Prompt         *promptYAML    `yaml:"prompt"`
}

// budgetYAML holds optional cost-governance caps from the agent.yaml budget block.
// USD values may be numbers (float/int) in YAML and are converted to exact-decimal
// strings for the CRD field (see floatToDecimalString for the conversion rule).
type budgetYAML struct {
	PerConversationUSD *float64 `yaml:"perConversationUSD"`
	PerAgentUSD        *float64 `yaml:"perAgentUSD"`
	SoftThresholdPct   *int32   `yaml:"softThresholdPct"`
}

// evalYAML holds the optional eval gate configuration from the agent.yaml eval
// block. It maps to an EvalSuite CRD + AgentDeployment.spec.evalSuiteRef.
type evalYAML struct {
	// suite is the name of the EvalSuite resource in the same namespace.
	Suite string `yaml:"suite"`
	// dataset is the named dataset ref (maps to EvalSuite.spec.dataset.ref).
	Dataset string `yaml:"dataset"`
	// scorers is the list of scorers to include in the generated EvalSuite.
	Scorers []scorerYAML `yaml:"scorers"`
	// threshold is the pass threshold (0..1 decimal string).
	Threshold string `yaml:"threshold"`
	// gate is "block" or "warn". Defaults to "block" when absent.
	Gate string `yaml:"gate"`
}

// scorerYAML is one scorer entry in the agent.yaml eval.scorers list.
type scorerYAML struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`
	Weight *int32 `yaml:"weight"`
}

// promptYAML holds the optional prompt-version configuration from the agent.yaml
// prompt block. It maps to a PromptVersion CRD + AgentDeployment.spec.promptRef.
type promptYAML struct {
	// name is the name of the PromptVersion resource to create/reference.
	Name string `yaml:"name"`
	// git holds the git-backed prompt source fields.
	Git gitPromptYAML `yaml:"git"`
}

// gitPromptYAML mirrors PromptVersion.spec.git for agent.yaml parsing.
type gitPromptYAML struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path"`
}

// resourcesYAML holds optional resource requests for the agent container.
type resourcesYAML struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// scalingYAML configures the Knative autoscaler bounds.
type scalingYAML struct {
	Min int32 `yaml:"min"`
	Max int32 `yaml:"max"`
}

// modelYAML specifies the ModelRoute alias for the agent's LLM calls.
type modelYAML struct {
	Route string `yaml:"route"`
}

// ── Output types ──────────────────────────────────────────────────────────────

// agentDeploymentOut is a lightweight representation of an AgentDeployment
// manifest for YAML output. It mirrors the CRD spec without the k8s ObjectMeta
// noise (creationTimestamp, managedFields, etc.) that the full k8s types carry.
type agentDeploymentOut struct {
	APIVersion string  `yaml:"apiVersion"`
	Kind       string  `yaml:"kind"`
	Metadata   metaOut `yaml:"metadata"`
	Spec       specOut `yaml:"spec"`
}

// evalSuiteOut is a lightweight representation of an EvalSuite manifest for
// YAML output.
type evalSuiteOut struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   metaOut       `yaml:"metadata"`
	Spec       evalSuiteSpec `yaml:"spec"`
}

// evalSuiteSpec mirrors EvalSuiteSpec for YAML marshalling.
type evalSuiteSpec struct {
	Dataset   datasetRefOut `yaml:"dataset"`
	Scorers   []scorerOut   `yaml:"scorers"`
	Threshold string        `yaml:"threshold"`
	Gate      string        `yaml:"gate,omitempty"`
}

// datasetRefOut mirrors DatasetRef for YAML marshalling.
type datasetRefOut struct {
	Ref string `yaml:"ref"`
}

// scorerOut mirrors ScorerSpec for YAML marshalling.
type scorerOut struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`
	Weight int32  `yaml:"weight,omitempty"`
}

// promptVersionOut is a lightweight representation of a PromptVersion manifest
// for YAML output.
type promptVersionOut struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   metaOut           `yaml:"metadata"`
	Spec       promptVersionSpec `yaml:"spec"`
}

// promptVersionSpec mirrors PromptVersionSpec for YAML marshalling.
type promptVersionSpec struct {
	Git gitPromptOut `yaml:"git"`
}

// gitPromptOut mirrors GitPromptSource for YAML marshalling.
type gitPromptOut struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path"`
}

// metaOut holds the name for an expanded manifest.
type metaOut struct {
	Name string `yaml:"name"`
}

// specOut mirrors AgentDeploymentSpec for YAML marshalling.
type specOut struct {
	Image          string        `yaml:"image"`
	ExecutionModel string        `yaml:"executionModel"`
	Resources      *resourcesOut `yaml:"resources,omitempty"`
	Scaling        *scalingOut   `yaml:"scaling,omitempty"`
	Env            []envVarOut   `yaml:"env,omitempty"`
	Budget         *budgetOut    `yaml:"budget,omitempty"`
	EvalSuiteRef   string        `yaml:"evalSuiteRef,omitempty"`
	PromptRef      string        `yaml:"promptRef,omitempty"`
}

// budgetOut mirrors BudgetSpec for YAML marshalling.
// USD values are exact-decimal strings; softThresholdPct is omitted when 0
// (the CRD default of 80 applies).
type budgetOut struct {
	PerConversationUSD string `yaml:"perConversationUSD,omitempty"`
	PerAgentUSD        string `yaml:"perAgentUSD,omitempty"`
	SoftThresholdPct   int32  `yaml:"softThresholdPct,omitempty"`
}

// resourcesOut holds resource request strings (e.g. "500m", "256Mi").
type resourcesOut struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// scalingOut holds autoscaler bounds.
type scalingOut struct {
	Min int32 `yaml:"min"`
	Max int32 `yaml:"max"`
}

// envVarOut is a single key/value environment variable.
type envVarOut struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// ── Exit code sentinels ───────────────────────────────────────────────────────

// exitCodes for the expand command.
const (
	exitOK         = 0
	exitValidation = 1 // unknown field, future field, missing required field
	exitParse      = 2 // file-not-found or YAML syntax error
)

// apiVersion is the group/version for all agent-engine CRD manifests.
const apiVersion = "agents.ctxmesh.ai/v1alpha1"

// expandError wraps an error with a suggested exit code.
type expandError struct {
	code int
	err  error
}

func (e *expandError) Error() string { return e.err.Error() }

func validationErr(format string, args ...any) *expandError {
	return &expandError{code: exitValidation, err: fmt.Errorf(format, args...)}
}

func parseErr(format string, args ...any) *expandError {
	return &expandError{code: exitParse, err: fmt.Errorf(format, args...)}
}

// ── Command ───────────────────────────────────────────────────────────────────

// newExpandCmd builds the cobra expand command.
func newExpandCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "expand <file>",
		Short: "Expand a simplified agent.yaml to an AgentDeployment CRD manifest",
		Long: `expand reads a simplified agent.yaml (PRD §8.5 M2+M8+M9 subset) and prints
the fully-expanded YAML manifests to stdout.

Supported fields: name, image, executionModel, resources, scaling, model.route, budget, eval, prompt
When eval: is present an EvalSuite manifest is emitted first, followed by the
AgentDeployment with spec.evalSuiteRef set. When prompt: is present a PromptVersion
manifest is emitted, followed by the AgentDeployment with spec.promptRef set.
Multiple documents are separated by "---".
Unknown fields cause a hard error. Fields that land in later milestones
(tools, memory, registry) are rejected with an informative message.

Exit codes: 0 = ok; 1 = validation error; 2 = file or parse error`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runExpand(args[0], cmd.OutOrStdout()); err != nil {
				var xe *expandError
				if ok := isExpandError(err, &xe); ok {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error:", xe.err)
					os.Exit(xe.code)
				}
				return err
			}
			return nil
		},
	}
}

// isExpandError type-asserts err to *expandError.
func isExpandError(err error, out **expandError) bool {
	xe, ok := err.(*expandError)
	if ok {
		*out = xe
	}
	return ok
}

// runExpand reads the agent.yaml at path, expands it, and writes YAML to w.
func runExpand(path string, w io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return parseErr("reading %q: %v", path, err)
	}
	return expandBytes(data, w)
}

// expandBytes parses rawYAML and writes the expanded manifests to w.
// When eval: and/or prompt: are present, additional EvalSuite/PromptVersion
// manifests are emitted as YAML documents separated by "---" before the
// AgentDeployment. It is split out for testability.
func expandBytes(rawYAML []byte, w io.Writer) error {
	// Phase 1: parse into a raw map to validate field names before type coercion.
	var raw map[string]any
	if err := yaml.Unmarshal(rawYAML, &raw); err != nil {
		return parseErr("YAML syntax error: %v", err)
	}
	if err := checkFields(raw); err != nil {
		return err
	}

	// Phase 2: parse into the typed struct.
	var ay agentYAML
	if err := yaml.Unmarshal(rawYAML, &ay); err != nil {
		return parseErr("YAML parse error: %v", err)
	}

	// Phase 3: validate required fields.
	if ay.Name == "" {
		return validationErr("required field missing: name")
	}
	if ay.Image == "" {
		return validationErr("required field missing: image")
	}

	// Phase 4: validate eval/prompt sub-fields when present.
	if ay.Eval != nil {
		if err := validateEvalYAML(ay.Eval); err != nil {
			return err
		}
	}
	if ay.Prompt != nil {
		if err := validatePromptYAML(ay.Prompt); err != nil {
			return err
		}
	}

	// Phase 5: build and emit manifests. Additional CRD manifests come first,
	// then the AgentDeployment. Each document is separated by "---\n".
	if ay.Eval != nil {
		if err := marshalYAML(w, buildEvalSuiteOutput(ay.Eval)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, "---\n"); err != nil {
			return fmt.Errorf("writing YAML separator: %w", err)
		}
	}
	if ay.Prompt != nil {
		if err := marshalYAML(w, buildPromptVersionOutput(ay.Prompt)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, "---\n"); err != nil {
			return fmt.Errorf("writing YAML separator: %w", err)
		}
	}
	return marshalYAML(w, buildOutput(&ay))
}

// validateEvalYAML checks that required sub-fields of the eval block are present.
func validateEvalYAML(e *evalYAML) error {
	if e.Suite == "" {
		return validationErr("eval.suite is required when eval block is present")
	}
	if e.Dataset == "" {
		return validationErr("eval.dataset is required when eval block is present")
	}
	if len(e.Scorers) == 0 {
		return validationErr("eval.scorers must have at least one entry")
	}
	if e.Threshold == "" {
		return validationErr("eval.threshold is required when eval block is present")
	}
	return nil
}

// validatePromptYAML checks that required sub-fields of the prompt block are present.
func validatePromptYAML(p *promptYAML) error {
	if p.Name == "" {
		return validationErr("prompt.name is required when prompt block is present")
	}
	if p.Git.Repo == "" {
		return validationErr("prompt.git.repo is required when prompt block is present")
	}
	if p.Git.Ref == "" {
		return validationErr("prompt.git.ref is required when prompt block is present")
	}
	if p.Git.Path == "" {
		return validationErr("prompt.git.path is required when prompt block is present")
	}
	return nil
}

// checkFields validates that every top-level key in raw is either a known
// supported field or emits an appropriate error for unknown/future fields.
func checkFields(raw map[string]any) error {
	for key := range raw {
		if knownFields[key] {
			continue
		}
		if ff, isFuture := futureFields[key]; isFuture {
			return validationErr("field %q is not yet supported (lands %s)", key, ff.milestone)
		}
		return validationErr("unknown field %q — check agent.yaml is correct", key)
	}
	return nil
}

// floatToDecimalString converts a float64 budget value from agent.yaml to the
// exact-decimal string form required by the CRD BudgetSpec fields.
//
// The CRD validation pattern is ^[0-9]+(\.[0-9]{1,6})?$ — at most 6 fractional
// digits — so the output must be bounded to [2, 6] decimal places. USD budgets
// never need finer than a millionth of a dollar, so capping at 6 is lossless in
// practice; inputs with more precision are rounded to 6 decimals rather than
// producing a manifest the API server would reject at admission.
//
// Conversion rule (deterministic):
//   - Format with exactly 6 fractional digits (strconv.FormatFloat, prec=6),
//     which rounds any excess precision (e.g. 0.1234567 → "0.123457").
//   - Trim trailing zeros, but keep a minimum of 2 fractional digits
//     (e.g. 0.5 → "0.500000" → "0.50"; 10 → "10.000000" → "10.00";
//     0.123456 → "0.123456").
//
// The output always matches the CRD pattern (2–6 fractional digits, no more).
func floatToDecimalString(f float64) string {
	s := strconv.FormatFloat(f, 'f', 6, 64)
	// s always contains a decimal point and exactly 6 fractional digits here.
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s[:dot], s[dot+1:]
	// Trim trailing zeros down to a minimum of 2 fractional digits.
	trimmed := strings.TrimRight(fracPart, "0")
	for len(trimmed) < 2 {
		trimmed += "0"
	}
	return intPart + "." + trimmed
}

// buildOutput converts a parsed agentYAML into the output struct.
func buildOutput(ay *agentYAML) *agentDeploymentOut {
	execModel := ay.ExecutionModel
	if execModel == "" {
		execModel = "serving" // CRD default
	}

	spec := specOut{
		Image:          ay.Image,
		ExecutionModel: execModel,
	}

	if ay.Resources != nil {
		spec.Resources = &resourcesOut{
			CPU:    ay.Resources.CPU,
			Memory: ay.Resources.Memory,
		}
	}

	if ay.Scaling != nil {
		spec.Scaling = &scalingOut{
			Min: ay.Scaling.Min,
			Max: ay.Scaling.Max,
		}
	}

	// model.route → MODEL_ROUTE env var (picked up by the agent at runtime;
	// MODEL_GATEWAY_URL is injected by the controller, not the manifest).
	if ay.Model != nil && ay.Model.Route != "" {
		spec.Env = append(spec.Env, envVarOut{
			Name:  "MODEL_ROUTE",
			Value: ay.Model.Route,
		})
	}

	// budget → spec.budget: map float/int USD values to exact-decimal strings.
	// Absent budget block → spec.budget stays nil (unenforced).
	if ay.Budget != nil {
		bo := &budgetOut{}
		if ay.Budget.PerConversationUSD != nil {
			bo.PerConversationUSD = floatToDecimalString(*ay.Budget.PerConversationUSD)
		}
		if ay.Budget.PerAgentUSD != nil {
			bo.PerAgentUSD = floatToDecimalString(*ay.Budget.PerAgentUSD)
		}
		if ay.Budget.SoftThresholdPct != nil {
			bo.SoftThresholdPct = *ay.Budget.SoftThresholdPct
		}
		// When all sub-fields are zero/empty (e.g. budget: {} with no caps),
		// still emit the block so downstream readers see the explicit intent.
		spec.Budget = bo
	}

	// eval → spec.evalSuiteRef: the EvalSuite resource name (same namespace).
	// Absent eval block → evalSuiteRef stays empty (no gate).
	if ay.Eval != nil {
		spec.EvalSuiteRef = ay.Eval.Suite
	}

	// prompt → spec.promptRef: the PromptVersion resource name (same namespace).
	// Absent prompt block → promptRef stays empty (image-bundled prompt).
	if ay.Prompt != nil {
		spec.PromptRef = ay.Prompt.Name
	}

	return &agentDeploymentOut{
		APIVersion: apiVersion,
		Kind:       "AgentDeployment",
		Metadata:   metaOut{Name: ay.Name},
		Spec:       spec,
	}
}

// buildEvalSuiteOutput converts a parsed evalYAML into an EvalSuite manifest.
func buildEvalSuiteOutput(e *evalYAML) *evalSuiteOut {
	scorers := make([]scorerOut, 0, len(e.Scorers))
	for _, s := range e.Scorers {
		so := scorerOut{
			Name: s.Name,
			Type: s.Type,
		}
		if s.Weight != nil {
			so.Weight = *s.Weight
		}
		// Weight 0 → omitted (CRD default 1 applies via +kubebuilder:default=1).
		scorers = append(scorers, so)
	}

	gate := e.Gate
	if gate == "" {
		gate = "block" // CRD default
	}

	return &evalSuiteOut{
		APIVersion: apiVersion,
		Kind:       "EvalSuite",
		Metadata:   metaOut{Name: e.Suite},
		Spec: evalSuiteSpec{
			Dataset:   datasetRefOut{Ref: e.Dataset},
			Scorers:   scorers,
			Threshold: e.Threshold,
			Gate:      gate,
		},
	}
}

// buildPromptVersionOutput converts a parsed promptYAML into a PromptVersion manifest.
func buildPromptVersionOutput(p *promptYAML) *promptVersionOut {
	return &promptVersionOut{
		APIVersion: apiVersion,
		Kind:       "PromptVersion",
		Metadata:   metaOut{Name: p.Name},
		Spec: promptVersionSpec{
			Git: gitPromptOut{
				Repo: p.Git.Repo,
				Ref:  p.Git.Ref,
				Path: p.Git.Path,
			},
		},
	}
}

// marshalYAML writes the output struct as 2-space-indented YAML to w.
func marshalYAML(w io.Writer, v any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("marshalling output YAML: %w", err)
	}
	return enc.Close()
}
