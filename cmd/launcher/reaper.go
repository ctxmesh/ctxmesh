//go:build !windows

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
	"os/exec"
	"os/signal"
	"syscall"
)

// startChild spawns cfg.Argv[0] as a child process, overriding AGENT_PORT in
// the child's environment to cfg.UpstreamPort so the child binds internally
// rather than on the proxy port. stdout/stderr/stdin are inherited.
func startChild(cfg Config) (*exec.Cmd, error) {
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...) //nolint:gosec // entrypoint validated by validateEntrypoint
	cmd.Env = buildChildEnv(cfg, os.Environ())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", cfg.Argv[0], err)
	}
	return cmd, nil
}

// forwardSignals subscribes to SIGTERM and SIGINT and forwards each to child.
// It runs until the channel is closed (signal.Stop). Call it in a goroutine.
func forwardSignals(child *exec.Cmd) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	for sig := range ch {
		_ = child.Process.Signal(sig)
	}
}

// reapAll is the PID-1 zombie-reaping loop. It calls Wait4(-1) in a blocking
// loop, reaping any child that exits — including the direct child (mainPID)
// and any orphaned grandchildren reparented to PID 1.
//
// When mainPID is reaped, reapAll returns its exit code:
//   - ws.ExitStatus() if the process exited normally
//   - 128 + signal number if the process was killed by a signal
//   - 1 as a fallback
//
// This replaces the M1 syscall.Exec model: instead of exec-replacing the
// launcher, we stay alive and reap. cmd.Wait() is NOT called — Wait4(-1,0)
// owns all child reaping to avoid a race between two waitpid callers on the
// same PID.
func reapAll(mainPID int) int {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil) // blocking: waits for any child
		if err != nil {
			if err == syscall.EINTR { //nolint:errorlint // direct comparison is correct for errno
				continue // interrupted by a forwarded signal, retry
			}
			// ECHILD: no more children. The main child must have already been
			// reaped (or never started, which is caught earlier). Treat as
			// non-zero exit.
			return 1
		}
		if pid != mainPID {
			// Orphan reaped — continue waiting for the main child.
			continue
		}
		// Main child exited.
		switch {
		case ws.Exited():
			return ws.ExitStatus()
		case ws.Signaled():
			return 128 + int(ws.Signal())
		default:
			return 1
		}
	}
}
