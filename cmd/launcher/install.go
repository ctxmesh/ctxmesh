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
	"io"
	"os"
	"path/filepath"
)

// launcherInstallFlag is the subcommand the C8 launcher-inject initContainer runs to stage the launcher
// binary into a shared emptyDir, so the agent (user) container can exec it WITHOUT baking the launcher into
// its own image (a launcher security fix then rolls centrally via a controller roll, not a fleet rebuild).
// The launcher image is distroless/static:nonroot — no shell, no `cp` — so the launcher self-installs.
const launcherInstallFlag = "--install"

// maybeInstall handles the `--install <dest>` subcommand: copy THIS executable to <dest> and signal main to
// exit with the returned code. Returns (false, 0) when this is NOT the install subcommand, so main proceeds
// to the normal launcher path. It is deliberately dependency-free — it runs as an initContainer with NO env
// / config / OTel — so it must not touch loadConfig or any runtime setup.
func maybeInstall(args []string) (handled bool, code int) {
	if len(args) < 2 || args[1] != launcherInstallFlag {
		return false, 0
	}
	if len(args) < 3 || args[2] == "" {
		fmt.Fprintln(os.Stderr, "launcher: --install requires a destination path")
		return true, 2
	}
	if err := installLauncher(args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: install failed: %v\n", err)
		return true, 1
	}
	return true, 0
}

// installLauncher copies this executable to dest with mode 0755 (owner rwx + world r-x) so the agent
// container — which mounts the emptyDir READ-ONLY, possibly under a different uid — can read + exec it. The
// copy is ATOMIC (write a temp file in dest's dir + rename) so a partially-written binary is never observed
// as the launcher; the initContainer completes before the agent container starts, but the atomic rename is
// defense-in-depth against a partial write on error.
func installLauncher(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own path: %w", err)
	}
	src, err := os.Open(self) //nolint:gosec // self is os.Executable(), not attacker-controlled.
	if err != nil {
		return fmt.Errorf("opening self %q: %w", self, err)
	}
	defer func() { _ = src.Close() }()
	srcInfo, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat self: %w", err)
	}

	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".launcher-install-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op after a successful rename

	n, err := io.Copy(tmp, src)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copying launcher binary: %w", err)
	}
	if err := tmp.Sync(); err != nil { // fsync: the bytes must hit the emptyDir before the rename
		_ = tmp.Close()
		return fmt.Errorf("syncing staged binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing staged binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // 0755: the agent container must exec it.
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("installing %q -> %q: %w", tmpName, dest, err)
	}
	// Verify the install is complete + executable — the initContainer must FAIL LOUDLY (Init:Error), never
	// leave a truncated/non-exec /platform/launcher that CrashLoops the agent container later.
	destInfo, err := os.Stat(dest)
	if err != nil {
		return fmt.Errorf("verifying installed binary: %w", err)
	}
	if n != srcInfo.Size() || destInfo.Size() != srcInfo.Size() {
		return fmt.Errorf("installed binary is truncated: copied %d of %d bytes", destInfo.Size(), srcInfo.Size())
	}
	if destInfo.Mode()&0o111 == 0 {
		return fmt.Errorf("installed binary is not executable (mode %s)", destInfo.Mode())
	}
	return nil
}
