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
	"fmt"
	"os"
	"syscall"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	if err := validateEntrypoint(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	// Replace the launcher process with the agent binary. After Exec returns
	// successfully this code never runs — the OS has replaced the process image.
	// On failure (e.g. permission denied after the stat check), report and exit.
	if err := syscall.Exec(cfg.Argv[0], cfg.Argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: exec %q: %v\n", cfg.Argv[0], err)
		os.Exit(1)
	}
}
