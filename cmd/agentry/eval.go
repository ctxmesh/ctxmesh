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

// eval.go implements `agentry eval` — the out-of-cluster CI/CD gate command
// (ADR 0063 D5, m70.10). It expands a simplified agent.yaml, applies the
// resulting AgentDeployment + EvalSuite to a zero-traffic eval namespace, polls
// status.gate until terminal, then exits with a structured report and the
// appropriate exit code so CI can gate on the result.
//
// Exit codes:
//
//	0 = pass (score >= minScore)
//	1 = fail below threshold (score < minScore, gate terminal)
//	2 = infra error (kubeconfig unavailable, apply failed, timeout)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/eval"
	"github.com/ctxmesh/agentry/internal/expand"
)

// ── Exit codes (eval command contract) ────────────────────────────────────────

const (
	evalExitPass  = 0 // score >= minScore
	evalExitFail  = 1 // score < minScore (gate is terminal)
	evalExitInfra = 2 // kubeconfig/apply/timeout error
)

// evalOutputJSON and evalOutputJUnit are the valid --output values.
const (
	evalOutputJSON  = "json"
	evalOutputJUnit = "junit"
)

// ── evalError: typed error carrying the exit code ─────────────────────────────

// evalError is a typed error that carries the exit code so RunE's os.Exit
// mapping in newEvalCmd can distinguish infra errors from threshold failures.
type evalError struct {
	code int
	msg  string
}

func (e *evalError) Error() string { return e.msg }

func evalInfraErr(format string, args ...any) *evalError {
	return &evalError{code: evalExitInfra, msg: fmt.Sprintf(format, args...)}
}

func evalFailErr(format string, args ...any) *evalError {
	return &evalError{code: evalExitFail, msg: fmt.Sprintf(format, args...)}
}

// ── EvalReport: the stable JSON/JUnit contract ────────────────────────────────

// EvalReport is the stable output schema emitted by `agentry eval`.
// The JSON shape is the API contract (ADR 0063 D5) — NEVER reorder or rename
// fields without a version bump.
//
//	{
//	  "candidate":  "path/to/agent.yaml",
//	  "namespace":  "agent-eval",
//	  "score":      "0.8500",
//	  "minScore":   0.80,
//	  "threshold":  "0.80",
//	  "decision":   "blocked",
//	  "phase":      "blocked",
//	  "pass":       false
//	}
type EvalReport struct {
	Candidate string  `json:"candidate"`
	Namespace string  `json:"namespace"`
	Score     string  `json:"score"`
	MinScore  float64 `json:"minScore"`
	Threshold string  `json:"threshold"`
	Decision  string  `json:"decision"`
	Phase     string  `json:"phase"`
	Pass      bool    `json:"pass"`
}

// ── evalFlagValues holds the raw cobra flag values ────────────────────────────

type evalFlagValues struct {
	candidate string
	dataset   string
	minScore  float64
	namespace string
	output    string
	timeout   time.Duration
}

// ── newEvalCmd builds the cobra eval command ──────────────────────────────────

func newEvalCmd() *cobra.Command {
	var fv evalFlagValues

	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run the CI/CD eval gate for a candidate agent.yaml",
		Long: `eval applies a candidate agent.yaml to an eval namespace as a zero-traffic
preview, polls the in-cluster eval-gate until it reaches a terminal phase
(promoted | blocked | warned), and exits with a structured report.

Exit codes:
  0 = pass  (score >= --min-score)
  1 = fail  (score < --min-score; gate produced a terminal decision)
  2 = infra (kubeconfig missing, apply failed, timeout, or other infra error)

The candidate is held at 0% traffic during eval (no live traffic is served).
Resources applied to the eval namespace are deleted on exit (best-effort).

Examples:
  agentry eval --candidate agent.yaml --min-score 0.80
  agentry eval --candidate agent.yaml --min-score 0.75 --output junit
  agentry eval --candidate agent.yaml --min-score 0.90 \
      --dataset my-dataset --namespace agent-eval --timeout 5m`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := runEval(cmd.Context(), fv, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err == nil {
				return nil
			}
			var ee *evalError
			if errors.As(err, &ee) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error:", ee.msg)
				os.Exit(ee.code)
			}
			// Unexpected error: let cobra print it and exit 1.
			return err
		},
	}

	cmd.Flags().StringVar(&fv.candidate, "candidate", "", "path to the agent.yaml to evaluate (required)")
	cmd.Flags().StringVar(&fv.dataset, "dataset", "",
		"override the EvalSuite's dataset ref (uses the agent.yaml eval.dataset when omitted)")
	cmd.Flags().Float64Var(&fv.minScore, "min-score", 0, "minimum score required to pass, in [0,1] (required)")
	cmd.Flags().StringVar(&fv.namespace, "namespace", "agent-eval",
		"eval namespace to apply the candidate resources into")
	cmd.Flags().StringVar(&fv.output, "output", "json",
		"report format: json or junit")
	cmd.Flags().DurationVar(&fv.timeout, "timeout", 2*time.Minute,
		"maximum time to wait for a terminal gate phase")

	_ = cmd.MarkFlagRequired("candidate")
	_ = cmd.MarkFlagRequired("min-score")

	return cmd
}

// ── runEval: the core logic ───────────────────────────────────────────────────

// runEval is the command body and the primary test seam. It returns an *evalError
// with the appropriate exit code so the RunE mapping in newEvalCmd can call
// os.Exit(code) rather than letting cobra map the error.
func runEval(ctx context.Context, fv evalFlagValues, out, errOut io.Writer) error {
	// ── 1. Validate flags ─────────────────────────────────────────────────────
	if err := validateEvalFlags(fv); err != nil {
		return err
	}

	// ── 2. Read + expand the candidate agent.yaml ─────────────────────────────
	raw, err := os.ReadFile(fv.candidate)
	if err != nil {
		return evalInfraErr("reading %q: %v", fv.candidate, err)
	}
	expanded, err := expand.Expand(raw)
	if err != nil {
		return evalInfraErr("expanding %q: %v", fv.candidate, err)
	}

	// ── 3. Decode the expanded multi-doc YAML → typed objects ─────────────────
	scheme, err := buildEvalScheme()
	if err != nil {
		return evalInfraErr("building scheme: %v", err)
	}
	ad, es, err := decodeEvalManifests(expanded, scheme)
	if err != nil {
		return evalInfraErr("decoding manifests from %q: %v", fv.candidate, err)
	}
	if ad == nil {
		return evalInfraErr("%q does not produce an AgentDeployment manifest (add image: or runtime: managed)", fv.candidate)
	}

	// ── 4. Override dataset if --dataset was provided ─────────────────────────
	if fv.dataset != "" && es != nil {
		es.Spec.Dataset.Ref = fv.dataset
	}

	// ── 5. Build the K8s client (ambient kubeconfig / in-cluster) ─────────────
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return evalInfraErr("building K8s client config (KUBECONFIG not set?): %v", err)
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return evalInfraErr("building K8s client: %v", err)
	}

	// ── 6. Stamp the namespace onto all resources ─────────────────────────────
	ns := fv.namespace
	if es != nil {
		es.Namespace = ns
	}
	ad.Namespace = ns

	// ── 7. Apply (create-or-replace) resources; defer teardown ───────────────
	if err := applyEvalResources(ctx, k8sClient, errOut, es, ad); err != nil {
		return evalInfraErr("applying resources: %v", err)
	}
	defer func() {
		teardownEvalResources(context.Background(), k8sClient, errOut, es, ad)
	}()

	// ── 8. Poll status.gate until terminal or timeout ─────────────────────────
	pollCtx, cancel := context.WithTimeout(ctx, fv.timeout)
	defer cancel()
	gate, err := pollGateStatus(pollCtx, k8sClient, errOut, ns, ad.Name)
	if err != nil {
		return evalInfraErr("polling gate status: %v", err)
	}

	// ── 9. Compute pass/fail and build the report ─────────────────────────────
	report, exitCode, decideErr := computeDecision(fv, gate)
	if decideErr != nil {
		return decideErr
	}

	// ── 10. Emit report ───────────────────────────────────────────────────────
	if err := emitReport(out, fv.output, report); err != nil {
		// Reporting failure is an infra error (stdout write problem).
		return evalInfraErr("writing report: %v", err)
	}

	if exitCode == evalExitFail {
		return evalFailErr("score %s < minScore %.4f (phase=%s decision=%s)",
			report.Score, fv.minScore, report.Phase, report.Decision)
	}
	return nil
}

// ── validateEvalFlags performs up-front flag validation ───────────────────────

func validateEvalFlags(fv evalFlagValues) *evalError {
	if fv.candidate == "" {
		return evalInfraErr("--candidate is required")
	}
	if fv.minScore < 0 || fv.minScore > 1 {
		return evalInfraErr("--min-score must be in [0,1], got %.4f", fv.minScore)
	}
	switch strings.ToLower(fv.output) {
	case evalOutputJSON, evalOutputJUnit:
	default:
		return evalInfraErr("--output must be json or junit, got %q", fv.output)
	}
	if fv.timeout <= 0 {
		return evalInfraErr("--timeout must be positive")
	}
	return nil
}

// ── buildEvalScheme builds the scheme used for the eval client ────────────────

func buildEvalScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := agentsv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := agentsv1beta1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("adding agents/v1beta1: %w", err)
	}
	return s, nil
}

// ── decodeEvalManifests splits the multi-doc YAML and extracts the AD + ES ────

// decodeEvalManifests parses the multi-doc YAML produced by expand.Expand and
// returns the AgentDeployment and (optionally) the EvalSuite. Both are decoded
// into typed objects via the scheme using the same decoder the BFF create path
// uses (utilyaml + serializer.NewCodecFactory). Returns the first AgentDeployment
// and the first EvalSuite found; other kinds (PromptVersion, MCPToolBinding) are
// silently skipped — the eval gate only needs these two.
func decodeEvalManifests(manifests []byte, scheme *runtime.Scheme) (
	*agentsv1alpha1.AgentDeployment, *agentsv1alpha1.EvalSuite, error,
) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(manifests)), 4096)
	codec := serializer.NewCodecFactory(scheme).UniversalDeserializer()

	var ad *agentsv1alpha1.AgentDeployment
	var es *agentsv1alpha1.EvalSuite

	for {
		var raw runtime.RawExtension
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("splitting YAML documents: %w", err)
		}
		if len(bytes.TrimSpace(raw.Raw)) == 0 {
			continue
		}
		obj, _, err := codec.Decode(raw.Raw, nil, nil)
		if err != nil {
			// Skip unrecognised kinds (PromptVersion, MCPToolBinding) gracefully.
			continue
		}
		switch typed := obj.(type) {
		case *agentsv1alpha1.AgentDeployment:
			if ad == nil {
				ad = typed
			}
		case *agentsv1alpha1.EvalSuite:
			if es == nil {
				es = typed
			}
		}
	}
	return ad, es, nil
}

// ── applyEvalResources creates or replaces the ES + AD in the eval namespace ──

// applyEvalResources applies the EvalSuite (if present) then the AgentDeployment
// to the eval namespace. Idempotency: if the resource already exists it is
// deleted and recreated. This keeps the implementation simple and deterministic —
// the eval namespace is ephemeral.
func applyEvalResources(
	ctx context.Context,
	k8sClient client.Client,
	errOut io.Writer,
	es *agentsv1alpha1.EvalSuite,
	ad *agentsv1alpha1.AgentDeployment,
) error {
	if es != nil {
		if err := applyObject(ctx, k8sClient, errOut, es); err != nil {
			return fmt.Errorf("applying EvalSuite %q: %w", es.Name, err)
		}
	}
	if err := applyObject(ctx, k8sClient, errOut, ad); err != nil {
		return fmt.Errorf("applying AgentDeployment %q: %w", ad.Name, err)
	}
	return nil
}

// applyObject creates the object; if it already exists, deletes and recreates it.
func applyObject(ctx context.Context, k8sClient client.Client, errOut io.Writer, obj client.Object) error {
	err := k8sClient.Create(ctx, obj)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Exists: delete + recreate for a clean eval run.
	existing := obj.DeepCopyObject().(client.Object)
	if getErr := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}, existing); getErr != nil {
		return fmt.Errorf("getting existing %T %q: %w", obj, obj.GetName(), getErr)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, _ = fmt.Fprintf(errOut,
		"agentry eval: %T %q already exists in %q — recreating\n",
		obj, obj.GetName(), obj.GetNamespace())
	if delErr := k8sClient.Delete(ctx, existing); delErr != nil && !apierrors.IsNotFound(delErr) {
		return fmt.Errorf("deleting existing %T %q: %w", obj, obj.GetName(), delErr)
	}
	obj.SetResourceVersion("")
	return k8sClient.Create(ctx, obj)
}

// ── pollGateStatus polls status.gate until terminal or ctx is done ────────────

// terminalPhases is the set of gate phases that mean the eval is complete.
var terminalPhases = map[string]bool{
	eval.PhasePromoted:          true,
	eval.PhaseBlocked:           true,
	eval.PhaseWarned:            true,
	eval.PhaseCanary:            true,
	eval.PhaseAborted:           true,
	eval.PhaseAwaitingPromotion: true,
}

// pollGateStatus polls the AgentDeployment's status.gate every 2 seconds until
// the gate phase is terminal (promoted | blocked | warned | canary | aborted |
// awaiting-promotion) or ctx is cancelled / deadline exceeded. It returns the
// final GateStatus.
func pollGateStatus(
	ctx context.Context,
	k8sClient client.Client,
	errOut io.Writer,
	namespace, name string,
) (*agentsv1alpha1.GateStatus, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	_, _ = fmt.Fprintf(errOut, "agentry eval: polling gate status for %s/%s…\n", namespace, name)

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for terminal gate phase: %w", ctx.Err())
		case <-ticker.C:
			var current agentsv1alpha1.AgentDeployment
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &current); err != nil {
				_, _ = fmt.Fprintf(errOut, "agentry eval: get %s: %v (retrying…)\n", name, err)
				continue
			}
			gate := current.Status.Gate
			if gate == nil {
				_, _ = fmt.Fprintf(errOut, "agentry eval: status.gate not yet set (phase: pending)\n")
				continue
			}
			_, _ = fmt.Fprintf(errOut, "agentry eval: gate phase=%s score=%s\n", gate.Phase, gate.Score)
			if terminalPhases[gate.Phase] {
				return gate, nil
			}
		}
	}
}

// ── computeDecision compares the gate score to minScore ───────────────────────

// computeDecision determines whether the run passes (exit 0) or fails (exit 1)
// by comparing the gate score to fv.minScore. Returns the report and the exit
// code. This is a pure function — no I/O — so it is trivially unit-testable.
func computeDecision(fv evalFlagValues, gate *agentsv1alpha1.GateStatus) (EvalReport, int, *evalError) {
	report := EvalReport{
		Candidate: fv.candidate,
		Namespace: fv.namespace,
		MinScore:  fv.minScore,
		Threshold: gate.Threshold,
		Decision:  gate.Decision,
		Phase:     gate.Phase,
		Score:     gate.Score,
	}

	// Parse the score string (e.g. "0.8500") to compare with minScore.
	scoreF, parseErr := parseScore(gate.Score)
	if parseErr != nil {
		// Unscored gate (LangFuse down / scoring error) — infra error.
		return report, evalExitInfra, evalInfraErr("gate score %q is not parseable: %v", gate.Score, parseErr)
	}

	report.Pass = scoreF >= fv.minScore

	exitCode := evalExitPass
	if !report.Pass {
		exitCode = evalExitFail
	}
	return report, exitCode, nil
}

// parseScore parses a gate score string like "0.8500" into float64. Returns an
// error when the string is empty or not a valid decimal — the caller maps this
// to an infra error (the gate didn't complete scoring).
func parseScore(s string) (float64, error) {
	if s == "" {
		return 0, errors.New("score is empty — gate did not produce a score")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %q: %w", s, err)
	}
	return f, nil
}

// ── emitReport renders the report to out ──────────────────────────────────────

// emitReport writes the report in the selected format (json or junit) to w.
func emitReport(w io.Writer, format string, report EvalReport) error {
	switch strings.ToLower(format) {
	case evalOutputJSON:
		return emitJSON(w, report)
	case evalOutputJUnit:
		return emitJUnit(w, report)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

func emitJSON(w io.Writer, report EvalReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// junitTestSuites is the root element of a JUnit XML report.
type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

// junitTestSuite wraps a single eval result as a JUnit test suite.
type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

// junitTestCase is the individual eval assertion.
type junitTestCase struct {
	XMLName   xml.Name      `xml:"testcase"`
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

// junitFailure captures the failure message when below threshold.
type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

func emitJUnit(w io.Writer, report EvalReport) error {
	tc := junitTestCase{
		Name:      "eval-gate",
		Classname: report.Candidate,
	}
	failures := 0
	if !report.Pass {
		failures = 1
		tc.Failure = &junitFailure{
			Message: fmt.Sprintf("score %s < minScore %.4f", report.Score, report.MinScore),
			Type:    "EvalGateFailure",
			Text: fmt.Sprintf(
				"candidate=%s namespace=%s score=%s minScore=%.4f threshold=%s decision=%s phase=%s",
				report.Candidate, report.Namespace, report.Score, report.MinScore,
				report.Threshold, report.Decision, report.Phase,
			),
		}
	}
	suite := junitTestSuites{
		TestSuites: []junitTestSuite{
			{
				Name:      "agentry-eval",
				Tests:     1,
				Failures:  failures,
				TestCases: []junitTestCase{tc},
			},
		},
	}
	data, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", xml.Header, data)
	return err
}

// ── teardownEvalResources deletes the resources (best-effort) ────────────────

// teardownEvalResources deletes the AgentDeployment and EvalSuite from the eval
// namespace. Both deletes are best-effort: errors are logged to errOut and
// silently swallowed so teardown never masks the primary result.
func teardownEvalResources(
	ctx context.Context,
	k8sClient client.Client,
	errOut io.Writer,
	es *agentsv1alpha1.EvalSuite,
	ad *agentsv1alpha1.AgentDeployment,
) {
	if ad != nil {
		if err := k8sClient.Delete(ctx, ad); err != nil && !apierrors.IsNotFound(err) {
			_, _ = fmt.Fprintf(errOut,
				"agentry eval: teardown warning: deleting AgentDeployment %q: %v\n", ad.Name, err)
		}
	}
	if es != nil {
		if err := k8sClient.Delete(ctx, es); err != nil && !apierrors.IsNotFound(err) {
			_, _ = fmt.Fprintf(errOut,
				"agentry eval: teardown warning: deleting EvalSuite %q: %v\n", es.Name, err)
		}
	}
}
