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
	"os"
	"path/filepath"
	"testing"
)

// installLauncher self-copies the running binary to dest with an executable mode, atomically.
func TestInstallLauncher_CopiesExecutable(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "platform", "launcher")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := installLauncher(dest); err != nil {
		t.Fatalf("installLauncher: %v", err)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("dest mode = %o, want 0755 (agent container must exec it)", fi.Mode().Perm())
	}
	// The copy must byte-match the running executable's size (the launcher binary).
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat self: %v", err)
	}
	if fi.Size() != selfInfo.Size() {
		t.Errorf("dest size = %d, want %d (a complete copy of the launcher)", fi.Size(), selfInfo.Size())
	}
	// No temp-file residue in the dest dir (the atomic rename cleaned up).
	entries, _ := os.ReadDir(filepath.Dir(dest))
	for _, e := range entries {
		if e.Name() != "launcher" {
			t.Errorf("unexpected residue in dest dir: %q", e.Name())
		}
	}
}

func TestInstallLauncher_BadDestDir(t *testing.T) {
	// A destination whose parent directory does not exist must error, not panic.
	if err := installLauncher(filepath.Join(t.TempDir(), "no-such-dir", "launcher")); err == nil {
		t.Fatal("installLauncher into a missing directory must error")
	}
}

// maybeInstall dispatches ONLY on `--install <path>`; a normal launcher invocation falls through.
func TestMaybeInstall_Dispatch(t *testing.T) {
	t.Run("not the install subcommand → falls through", func(t *testing.T) {
		if handled, code := maybeInstall([]string{"launcher"}); handled || code != 0 {
			t.Errorf("a normal invocation must fall through, got handled=%v code=%d", handled, code)
		}
	})
	t.Run("install → copies + exit 0", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "launcher")
		handled, code := maybeInstall([]string{"launcher", "--install", dest})
		if !handled || code != 0 {
			t.Fatalf("--install must be handled with exit 0, got handled=%v code=%d", handled, code)
		}
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("--install did not stage the binary: %v", err)
		}
	})
	t.Run("install without a path → exit 2", func(t *testing.T) {
		if handled, code := maybeInstall([]string{"launcher", "--install"}); !handled || code != 2 {
			t.Errorf("--install with no path must be handled with exit 2, got handled=%v code=%d", handled, code)
		}
	})
	t.Run("install into a missing dir → exit 1", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "missing", "launcher")
		if handled, code := maybeInstall([]string{"launcher", "--install", bad}); !handled || code != 1 {
			t.Errorf("a failed install must be handled with exit 1, got handled=%v code=%d", handled, code)
		}
	})
}
