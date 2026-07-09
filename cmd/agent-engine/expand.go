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

// knownFields is the set of top-level agent.yaml fields supported in M2+M8.
var knownFields = map[string]bool{
	"name":           true,
	"image":          true,
	"executionModel": true,
	"resources":      true,
	"scaling":        true,
	"model":          true,
	"budget":         true,
}

// futureField describes a top-level field not yet supported, with the milestone
// where it lands.
type futureField struct {
	milestone string
}

// futureFields is the set of recognised-but-not-yet-supported top-level fields.
// Fields not in knownFields and not in futureFields are fully unknown → hard error.
var futureFields = map[string]futureField{
	"prompt":   {milestone: "M9"},
	"tools":    {milestone: "M4"},
	"memory":   {milestone: "M5"},
	"registry": {milestone: "M6"},
}

// ── Input types ───────────────────────────────────────────────────────────────

// agentYAML is the M2+M8 subset of the simplified PRD §8.5 agent.yaml format.
type agentYAML struct {
	Name           string         `yaml:"name"`
	Image          string         `yaml:"image"`
	ExecutionModel string         `yaml:"executionModel"`
	Resources      *resourcesYAML `yaml:"resources"`
	Scaling        *scalingYAML   `yaml:"scaling"`
	Model          *modelYAML     `yaml:"model"`
	Budget         *budgetYAML    `yaml:"budget"`
}

// budgetYAML holds optional cost-governance caps from the agent.yaml budget block.
// USD values may be numbers (float/int) in YAML and are converted to exact-decimal
// strings for the CRD field (see floatToDecimalString for the conversion rule).
type budgetYAML struct {
	PerConversationUSD *float64 `yaml:"perConversationUSD"`
	PerAgentUSD        *float64 `yaml:"perAgentUSD"`
	SoftThresholdPct   *int32   `yaml:"softThresholdPct"`
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

// metaOut holds the name for the expanded AgentDeployment.
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
		Long: `expand reads a simplified agent.yaml (PRD §8.5 M2+M8 subset) and prints
the fully-expanded AgentDeployment YAML to stdout.

Supported fields: name, image, executionModel, resources, scaling, model.route, budget
Unknown fields cause a hard error. Fields that land in later milestones
(prompt, tools, memory, registry) are rejected with an informative message.

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

// expandBytes parses rawYAML and writes the expanded AgentDeployment to w.
// It is split out for testability.
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

	// Phase 4: build and emit the AgentDeployment manifest.
	return marshalYAML(w, buildOutput(&ay))
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

	return &agentDeploymentOut{
		APIVersion: "agents.ctxmesh.ai/v1alpha1",
		Kind:       "AgentDeployment",
		Metadata:   metaOut{Name: ay.Name},
		Spec:       spec,
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
