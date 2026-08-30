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

// Package main contains the agentry CLI expand command.
// expand reads a simplified agent.yaml (PRD §8.5 M2 subset) and prints the
// fully-expanded AgentDeployment CRD manifest to stdout.
//
// The agent.yaml → CRD mapping itself lives in the reusable internal/expand
// package so the CLI and the BFF config-builder (m12.6) share ONE mapping — this
// file is only the cobra wiring + the CLI exit-code contract on top of it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/ctxmesh/agentry/internal/expand"
)

// ── Exit code sentinels ───────────────────────────────────────────────────────

// exitCodes for the expand command.
const (
	exitOK         = 0
	exitValidation = 1 // unknown field, future field, missing required field
	exitParse      = 2 // file-not-found or YAML syntax error
)

// ── Command ───────────────────────────────────────────────────────────────────

// newExpandCmd builds the cobra expand command.
func newExpandCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "expand <file>",
		Short: "Expand a simplified agent.yaml to an AgentDeployment CRD manifest",
		Long: `expand reads a simplified agent.yaml (PRD §8.5 M2+M8+M9+M14 subset) and prints
the fully-expanded YAML manifests to stdout.

Supported fields: name, image, executionModel, resources, scaling, model.route,
budget, eval, prompt, runtime, systemPrompt, tools
When eval: is present an EvalSuite manifest is emitted first, followed by the
AgentDeployment with spec.evalSuiteRef set. When prompt: is present a PromptVersion
manifest is emitted, followed by the AgentDeployment with spec.promptRef set.
With runtime: managed (ADR 0013) image becomes optional (resolved to the pinned
managed-agent ref), systemPrompt becomes the SYSTEM_PROMPT env, and each entry of
tools generates an MCPToolBinding; a custom agent (no runtime) still requires image.
Multiple documents are separated by "---".
Unknown fields cause a hard error. Fields that land in later milestones
(memory, registry) are rejected with an informative message.

Exit codes: 0 = ok; 1 = validation error; 2 = file or parse error`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runExpand(args[0], cmd.OutOrStdout()); err != nil {
				if code, ok := exitCodeFor(err); ok {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
					os.Exit(code)
				}
				return err
			}
			return nil
		},
	}
}

// exitCodeFor maps an error to the CLI exit-code contract. A validation error
// (unknown/future/missing field) → 1; a parse error (unreadable file or malformed
// YAML) → 2; anything else is not an *expand.Error and is returned to cobra.
func exitCodeFor(err error) (int, bool) {
	var xe *expand.Error
	if !errors.As(err, &xe) {
		return 0, false
	}
	switch xe.Kind {
	case expand.KindValidation:
		return exitValidation, true
	case expand.KindParse:
		return exitParse, true
	default:
		return exitParse, true
	}
}

// runExpand reads the agent.yaml at path, expands it, and writes YAML to w.
// The read error is wrapped as a parse-kind expand.Error so exitCodeFor maps a
// missing/unreadable file to exit 2 exactly as before the refactor.
func runExpand(path string, w io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &expand.Error{Kind: expand.KindParse, Err: fmt.Errorf("reading %q: %v", path, err)}
	}
	// Buffer so we can warn (ADR 0044) when the output includes a PromptVersion doc — PromptVersion is
	// retired to Postgres (API-managed), so that document cannot be `kubectl apply`'d (there is no CRD).
	var buf bytes.Buffer
	if err := expand.ExpandBytes(data, &buf); err != nil {
		return err
	}
	if bytes.Contains(buf.Bytes(), []byte("kind: PromptVersion")) {
		fmt.Fprintln(os.Stderr,
			"Note: PromptVersion is API-managed (retired to Postgres, ADR 0044) — the PromptVersion document "+
				"below cannot be applied with kubectl. Create the agent via the console/API, or use "+
				"`promptRef: <name>` to reference an existing prompt.")
	}
	_, err = w.Write(buf.Bytes())
	return err
}
