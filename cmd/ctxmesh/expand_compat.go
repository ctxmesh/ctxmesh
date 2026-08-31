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

package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/expand"
)

// This file is a thin compatibility layer over the reusable internal/expand
// package. The comprehensive CLI expand_test.go — the behaviour guard that the
// refactor did NOT change the CLI's output/exit codes — drives the mapping
// through these package-local symbols. They forward to the single shared mapping
// so there is no second, divergent implementation: the CLI, the BFF, and this
// test surface all resolve to internal/expand.

// modelYAML is the model.route sub-field of the simplified agent.yaml. Only the
// `dev` command parses its own agent.yaml subset directly (it tolerates unknown
// fields so a superset agent.yaml still runs); the full expand mapping lives in
// internal/expand. Kept here so both CLI subcommands agree on the field name.
type modelYAML struct {
	Route string `yaml:"route"`
}

// expandError adapts an *expand.Error to the CLI's historical shape (a code +
// err), so the existing golden/error tests read xe.code without change, and the
// `dev` command surfaces the same exit-code contract.
type expandError struct {
	code int
	err  error
}

func (e *expandError) Error() string { return e.err.Error() }

// validationErr / parseErr build the CLI-local *expandError with the mapped exit
// code. The `dev` command uses these for its own flag/parse validation so its
// exit-code contract matches `expand`.
func validationErr(format string, args ...any) *expandError {
	return &expandError{code: exitValidation, err: fmt.Errorf(format, args...)}
}

func parseErr(format string, args ...any) *expandError {
	return &expandError{code: exitParse, err: fmt.Errorf(format, args...)}
}

// isExpandError type-asserts err to *expandError (used by the dev command's
// RunE to map a validation/parse failure to os.Exit(code)).
func isExpandError(err error, out **expandError) bool {
	xe, ok := err.(*expandError)
	if ok {
		*out = xe
	}
	return ok
}

// expandBytes forwards to expand.ExpandBytes but re-wraps any *expand.Error as
// the CLI-local *expandError (carrying the mapped exit code) so the behaviour
// guard asserts on the same type/exit-code contract as before the refactor.
func expandBytes(rawYAML []byte, w io.Writer) error {
	err := expand.ExpandBytes(rawYAML, w)
	if err == nil {
		return nil
	}
	var xe *expand.Error
	if errors.As(err, &xe) {
		code := exitParse
		if xe.Kind == expand.KindValidation {
			code = exitValidation
		}
		return &expandError{code: code, err: xe}
	}
	return err
}

// floatToDecimalString mirrors the shared package's exact-decimal conversion so
// the CLI's dedicated conversion table test keeps exercising the one rule. It is
// a verbatim forward of the same algorithm (kept in sync by the equivalence test
// in internal/expand, which drives the same inputs through the public API).
func floatToDecimalString(f float64) string {
	s := strconv.FormatFloat(f, 'f', 6, 64)
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s[:dot], s[dot+1:]
	trimmed := strings.TrimRight(fracPart, "0")
	for len(trimmed) < 2 {
		trimmed += "0"
	}
	return intPart + "." + trimmed
}
