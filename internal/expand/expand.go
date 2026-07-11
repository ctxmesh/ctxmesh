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

// Package expand is the single source of truth for the simplified agent.yaml →
// CRD manifest mapping (PRD §8.5 M2+M8+M9 subset). It is imported by BOTH the
// `agent-engine expand` CLI (cmd/agent-engine) and the BFF config-builder
// (internal/bff, m12.6) so the form-driven UI and the CLI produce byte-identical
// manifests — there is exactly one mapping, never a forked second one.
//
// The public surface is:
//
//   - ExpandBytes(rawYAML, w): parse+validate+render to a writer (the CLI path).
//   - Expand(rawYAML) ([]byte, error): the same, buffered (the BFF path).
//   - Error: a validation/parse error carrying an ErrorKind so callers can map
//     it to an exit code (CLI) or an HTTP status (BFF) without swallowing it.
package expand

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the group/version stamped on every expanded CRD manifest.
const APIVersion = "agents.ctxmesh.ai/v1alpha1"

// AnnotationSourceSpec is the annotation key under which the BFF persists the
// exact submitted simplified agent spec (canonical JSON) on the primary
// AgentDeployment at create/update through the console (ADR 0017). It is the
// edit source of truth: a later PUT re-expands from it. Agents created outside
// the console (kubectl) won't carry it → they are "managed outside the UI".
// It is exported here — beside APIVersion, the annotation-convention home — so
// the create path, the PUT path (m15.3), and the detail DTO share one key.
const AnnotationSourceSpec = "agents.ctxmesh.ai/source-spec"

// ── Managed-runtime configuration (ADR 0013) ──────────────────────────────────
//
// The managed runtime resolves a `runtime: managed` agent to a stock,
// platform-owned image + registry-backed tool bindings. These three references
// are configurable (env override with a sane default) so a Helm deploy can pin
// the published managed image (Helm value managedAgent.image), a cluster can
// point at its own tool registry, and the per-tool server convention can be
// retargeted — without changing code. Defaults keep the CLI usable offline.

const (
	// DefaultManagedImage is the pinned managed-agent image ref used when an
	// agent sets `runtime: managed` and omits `image`. Override at deploy time
	// via the Helm value managedAgent.image (surfaced to expand as the
	// MANAGED_AGENT_IMAGE env). This is the image built by
	// `make docker-build-managed`.
	DefaultManagedImage = "ghcr.io/ctxmesh/managed-agent:latest"

	// DefaultManagedToolRegistry is the ToolRegistry the generated MCPToolBindings
	// reference (registryRef). The BYO-MCP flow (ADR 0016) feeds this catalog;
	// override via MANAGED_TOOL_REGISTRY.
	DefaultManagedToolRegistry = "default-tools"

	// DefaultManagedToolServerTemplate is the remote-mode server URL the generated
	// MCPToolBindings point at, built from the tool name via a single "%s"
	// placeholder. The concrete catalog URL is owned by the ToolRegistry/BYO-MCP
	// path; this convention keeps expand's output admissible (the CRD's CEL
	// requires a server) and deterministic. Override via MANAGED_TOOL_SERVER_URL
	// (must contain exactly one "%s" for the tool name).
	DefaultManagedToolServerTemplate = "http://%s.mcp.svc.cluster.local/mcp"

	// envManagedImage / envManagedToolRegistry / envManagedToolServer are the
	// env vars that override the defaults above.
	envManagedImage        = "MANAGED_AGENT_IMAGE"
	envManagedToolRegistry = "MANAGED_TOOL_REGISTRY"
	envManagedToolServer   = "MANAGED_TOOL_SERVER_URL"
)

// managedImageRef returns the resolved managed-agent image ref (env override →
// default).
func managedImageRef() string {
	if v := os.Getenv(envManagedImage); v != "" {
		return v
	}
	return DefaultManagedImage
}

// managedToolRegistry returns the ToolRegistry name generated bindings reference.
func managedToolRegistry() string {
	if v := os.Getenv(envManagedToolRegistry); v != "" {
		return v
	}
	return DefaultManagedToolRegistry
}

// managedToolServerURL returns the remote server URL for a tool, from the
// configurable template. The tool name is DNS-sanitized before it goes into the
// URL host so an underscored catalog name (echo_tool, get_weather — the MCP
// mainline) yields a resolvable RFC-1123 label rather than an invalid host. A
// template missing the "%s" placeholder falls back to the default so a
// misconfigured env can never emit an empty (inadmissible) server URL.
func managedToolServerURL(tool string) string {
	tmpl := os.Getenv(envManagedToolServer)
	if tmpl == "" || !strings.Contains(tmpl, "%s") {
		tmpl = DefaultManagedToolServerTemplate
	}
	return fmt.Sprintf(tmpl, dns1123Label(tool))
}

// managedRuntimeValue is the only accepted value of the `runtime` field.
const managedRuntimeValue = "managed"

// dns1123Label sanitizes s into a valid RFC-1123 DNS label (a k8s metadata.name
// component and a DNS host label): lowercase, [a-z0-9-], starting and ending
// alphanumeric, ≤63 chars. This is used ONLY for the generated MCPToolBinding
// metadata.name component and the convention server-URL host label — the REAL
// catalog name is preserved verbatim in spec.toolName (the discovery/loop match
// is on toolName, and toolmanifest.Binding keeps BindingName and ToolName
// independent), so an underscored tool (echo_tool) binds correctly while its
// binding OBJECT gets an admissible name.
//
// Rule:
//   - lowercase; any char not in [a-z0-9-] (incl. "_" and ".") → "-".
//   - collapse runs and trim leading/trailing "-".
//   - cap at 63 chars (RFC-1123 label limit), re-trimming a trailing "-".
//   - an input that sanitizes to empty → "x" (never emit an empty label).
func dns1123Label(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(s) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		// Any other rune (incl. "_", ".", spaces) becomes a single "-".
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	if out == "" {
		return "x"
	}
	return out
}

// ErrorKind classifies an expand failure so callers can map it to their own
// error channel (an exit code for the CLI, an HTTP status for the BFF) without
// string-matching the message.
type ErrorKind int

const (
	// KindValidation is a semantically-invalid input: an unknown field, a
	// not-yet-supported field, or a missing required field. Maps to CLI exit 1 /
	// HTTP 400.
	KindValidation ErrorKind = iota + 1
	// KindParse is a structural failure: unreadable input or malformed YAML.
	// Maps to CLI exit 2 / HTTP 400 (the input the user supplied is not YAML).
	KindParse
)

// Error is a typed expand failure carrying the ErrorKind. Callers use the Kind
// field (or errors.As) to route it. The message is safe to surface to the user —
// it never contains server-side detail. Err is exported so callers outside this
// package (the CLI's file-read path) can construct a parse-kind error that maps
// to the same exit code.
type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func validationErr(format string, args ...any) *Error {
	return &Error{Kind: KindValidation, Err: fmt.Errorf(format, args...)}
}

func parseErr(format string, args ...any) *Error {
	return &Error{Kind: KindParse, Err: fmt.Errorf(format, args...)}
}

// knownFields is the set of top-level agent.yaml fields supported in
// M2+M8+M9+M14. `runtime`, `systemPrompt`, and `tools` are the M14 managed-
// runtime additions (ADR 0013).
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
	"runtime":        true,
	"systemPrompt":   true,
	"tools":          true,
}

// futureField describes a top-level field not yet supported, with the milestone
// where it lands.
type futureField struct {
	milestone string
}

// futureFields is the set of recognised-but-not-yet-supported top-level fields.
// Fields not in knownFields and not in futureFields are fully unknown → hard error.
var futureFields = map[string]futureField{
	"memory":   {milestone: "M5"},
	"registry": {milestone: "M6"},
}

// ── Input types ───────────────────────────────────────────────────────────────

// agentYAML is the M2+M8+M9+M14 subset of the simplified PRD §8.5 agent.yaml
// format. Runtime/SystemPrompt/Tools are the M14 managed-runtime fields.
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
	// Runtime selects the agent runtime. Empty = custom (image required, the
	// unchanged path); "managed" = the stock managed-agent image (image optional,
	// resolved to the pinned managed ref) — ADR 0013.
	Runtime string `yaml:"runtime"`
	// SystemPrompt is the managed agent's system prompt (delivered to the pod as
	// the SYSTEM_PROMPT env). Managed runtime only.
	SystemPrompt string `yaml:"systemPrompt"`
	// Tools is the list of tool catalog names to bind (each → one MCPToolBinding,
	// the same binding path a custom agent uses). Managed runtime only.
	Tools []string `yaml:"tools"`
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
	// Suite is the name of the EvalSuite resource in the same namespace.
	Suite string `yaml:"suite"`
	// Dataset is the named dataset ref (maps to EvalSuite.spec.dataset.ref).
	Dataset string `yaml:"dataset"`
	// Scorers is the list of scorers to include in the generated EvalSuite.
	Scorers []scorerYAML `yaml:"scorers"`
	// Threshold is the pass threshold (0..1 decimal string).
	Threshold string `yaml:"threshold"`
	// Gate is "block" or "warn". Defaults to "block" when absent.
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
	// Name is the name of the PromptVersion resource to create/reference.
	Name string `yaml:"name"`
	// Git holds the git-backed prompt source fields.
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

// mcpToolBindingOut is a lightweight representation of an MCPToolBinding manifest
// for YAML output (ADR 0013 / specs/mcp-tools.md). One is emitted per managed
// agent tool — the same binding path a custom agent uses.
type mcpToolBindingOut struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   metaOut            `yaml:"metadata"`
	Spec       mcpToolBindingSpec `yaml:"spec"`
}

// mcpToolBindingSpec mirrors MCPToolBindingSpec for YAML marshalling.
type mcpToolBindingSpec struct {
	AgentRef    string        `yaml:"agentRef"`
	RegistryRef string        `yaml:"registryRef"`
	ToolName    string        `yaml:"toolName"`
	Mode        string        `yaml:"mode"`
	Server      toolServerOut `yaml:"server"`
}

// toolServerOut mirrors ToolServer for YAML marshalling (remote mode: url).
type toolServerOut struct {
	URL string `yaml:"url"`
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

// ── Public API ────────────────────────────────────────────────────────────────

// Expand parses rawYAML and returns the expanded CRD manifest(s) as a byte
// slice. It is the buffered form of ExpandBytes used by the BFF config-builder;
// the CLI streams directly to stdout via ExpandBytes. Errors are *Error with a
// Kind the caller can route (KindValidation → 400, KindParse → 400).
func Expand(rawYAML []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := ExpandBytes(rawYAML, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExpandBytes parses rawYAML and writes the expanded manifests to w.
// When eval: and/or prompt: are present, additional EvalSuite/PromptVersion
// manifests are emitted as YAML documents separated by "---" before the
// AgentDeployment.
func ExpandBytes(rawYAML []byte, w io.Writer) error {
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

	// Phase 3: validate required fields + the runtime branch (ADR 0013).
	if ay.Name == "" {
		return validationErr("required field missing: name")
	}
	if err := validateRuntime(&ay); err != nil {
		return err
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
	// Managed runtime: one MCPToolBinding per bound tool (the same binding path a
	// custom agent uses). Emitted before the AgentDeployment, in list order.
	for _, binding := range buildToolBindings(&ay) {
		if err := marshalYAML(w, binding); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, "---\n"); err != nil {
			return fmt.Errorf("writing YAML separator: %w", err)
		}
	}
	return marshalYAML(w, buildOutput(&ay))
}

// ── Validation ────────────────────────────────────────────────────────────────

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

// validateRuntime enforces the two-runtime branch (ADR 0013):
//
//   - runtime unset (custom path): `image` is REQUIRED (unchanged); the
//     managed-only fields (systemPrompt, tools) are rejected so a custom agent
//     can't accidentally set managed-only knobs.
//   - runtime: managed: `image` is OPTIONAL (resolved to the pinned managed ref
//     when omitted; an explicit image still wins so a fork can be pinned).
//
// Any other runtime value is a hard error.
func validateRuntime(ay *agentYAML) error {
	switch ay.Runtime {
	case "":
		// Custom path — unchanged: image required, no managed-only fields.
		if ay.Image == "" {
			return validationErr("required field missing: image")
		}
		if ay.SystemPrompt != "" {
			return validationErr("systemPrompt requires runtime: managed")
		}
		if len(ay.Tools) > 0 {
			return validationErr("tools requires runtime: managed")
		}
		return nil
	case managedRuntimeValue:
		// Managed path — image optional (resolved when omitted). Tool names must
		// be non-empty when provided.
		for i, t := range ay.Tools {
			if strings.TrimSpace(t) == "" {
				return validationErr("tools[%d] is empty — each tool must be a non-empty catalog name", i)
			}
		}
		return nil
	default:
		return validationErr("unknown runtime %q — the only supported value is %q", ay.Runtime, managedRuntimeValue)
	}
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

// ── Mapping ───────────────────────────────────────────────────────────────────

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

	// Resolve the image: for a managed agent with no explicit image, resolve to
	// the pinned managed-agent ref (ADR 0013). An explicit image always wins (a
	// managed fork can be pinned); the custom path uses ay.Image verbatim.
	image := ay.Image
	if ay.Runtime == managedRuntimeValue && image == "" {
		image = managedImageRef()
	}

	spec := specOut{
		Image:          image,
		ExecutionModel: execModel,
	}

	// Managed runtime: deliver the system prompt as the SYSTEM_PROMPT env the
	// managed-agent entrypoint reads (config → behaviour). Emitted before
	// MODEL_ROUTE so the env order is deterministic (systemPrompt, then route).
	if ay.Runtime == managedRuntimeValue && ay.SystemPrompt != "" {
		spec.Env = append(spec.Env, envVarOut{
			Name:  "SYSTEM_PROMPT",
			Value: ay.SystemPrompt,
		})
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
		APIVersion: APIVersion,
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
		APIVersion: APIVersion,
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
		APIVersion: APIVersion,
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

// buildToolBindings converts a managed agent's tools list into one
// MCPToolBinding manifest per tool — the same binding path a custom agent uses
// (specs/mcp-tools.md). Each binding references the configured ToolRegistry
// (registryRef) and points at the per-tool remote server URL (the configurable
// convention). Returns nil for a custom agent or a managed agent with no tools.
//
// The binding OBJECT name is "<agent>-<dns-sanitized-tool>" so it is always a
// valid RFC-1123 subdomain even for an underscored catalog name (echo_tool →
// managed-agent-echo-tool). The REAL catalog name is preserved verbatim in
// spec.toolName — that is what the discovery manifest and the loop match on, so
// the binding still resolves the correct tool.
func buildToolBindings(ay *agentYAML) []*mcpToolBindingOut {
	if ay.Runtime != managedRuntimeValue || len(ay.Tools) == 0 {
		return nil
	}
	registry := managedToolRegistry()
	bindings := make([]*mcpToolBindingOut, 0, len(ay.Tools))
	for _, tool := range ay.Tools {
		bindings = append(bindings, &mcpToolBindingOut{
			APIVersion: APIVersion,
			Kind:       "MCPToolBinding",
			// metadata.name is DNS-sanitized (admissible); spec.toolName is the
			// real catalog name (the match key) — kept independent on purpose.
			Metadata: metaOut{Name: ay.Name + "-" + dns1123Label(tool)},
			Spec: mcpToolBindingSpec{
				AgentRef:    ay.Name,
				RegistryRef: registry,
				ToolName:    tool,
				Mode:        "remote",
				Server:      toolServerOut{URL: managedToolServerURL(tool)},
			},
		})
	}
	return bindings
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
