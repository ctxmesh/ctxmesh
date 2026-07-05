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

// Package main implements the agent-engine launcher — PID 1 for agent containers.
//
// Why exec instead of fork+wait:
//
//	syscall.Exec replaces the launcher process image entirely. The user's agent
//	binary inherits PID 1, receives signals directly (SIGTERM, SIGKILL), and its
//	exit code becomes the container's exit code — all with zero bookkeeping.
//	A fork+wait loop adds complexity (signal proxying, wait4 edge cases, zombie
//	reaping) with no benefit in M1 where the launcher has no post-exec work.
//	Secret/tool/memory hydration (M2+) will run before the Exec call, not after.
package main

import (
	"fmt"
	"os"
	"strings"
)

// Config holds the parsed launcher configuration.
type Config struct {
	// Argv is the argument vector for syscall.Exec: Argv[0] is the agent binary
	// path; remaining elements are its command-line arguments.
	Argv []string
}

// loadConfig reads launcher configuration from environment variables.
//
// Environment variables:
//
//	AGENT_ENTRYPOINT (required): absolute path to the agent binary.
//	AGENT_ENTRYPOINT_ARGS (optional): additional arguments passed to the agent,
//	    split on whitespace. Example: "--port 8080 --log-level debug".
//
// The lookup parameter is a function that returns the value of an environment
// variable by name (typically os.Getenv); it is a parameter so the pure parsing
// logic can be exercised in unit tests without mutating process state.
func loadConfig(lookup func(string) string) (Config, error) {
	ep := lookup("AGENT_ENTRYPOINT")
	if ep == "" {
		return Config{}, fmt.Errorf(
			"AGENT_ENTRYPOINT is not set: set it to the absolute path of the agent binary to exec",
		)
	}

	argv := []string{ep}

	if args := lookup("AGENT_ENTRYPOINT_ARGS"); args != "" {
		argv = append(argv, strings.Fields(args)...)
	}

	return Config{Argv: argv}, nil
}

// validateEntrypoint checks that the binary named in cfg.Argv[0] exists on the
// filesystem and has at least one executable bit set. It does not attempt to
// exec the binary — the caller is responsible for calling syscall.Exec.
func validateEntrypoint(cfg Config) error {
	ep := cfg.Argv[0]

	info, err := os.Stat(ep)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("entrypoint %q: file not found", ep)
		}
		return fmt.Errorf("entrypoint %q: %w", ep, err)
	}
	if info.IsDir() {
		return fmt.Errorf("entrypoint %q: is a directory, not an executable binary", ep)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("entrypoint %q: not executable (mode %s)", ep, info.Mode())
	}

	return nil
}
