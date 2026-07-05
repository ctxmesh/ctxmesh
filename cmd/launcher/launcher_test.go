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
	"runtime"
	"testing"
)

// envMap returns a lookup function backed by a static map of env variables.
func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// TestLoadConfig covers the env-variable parsing logic for loadConfig.
func TestLoadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		wantArgv []string
		wantErr  bool
	}{
		{
			name:    "missing AGENT_ENTRYPOINT returns error",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "empty AGENT_ENTRYPOINT returns error",
			env:     map[string]string{"AGENT_ENTRYPOINT": ""},
			wantErr: true,
		},
		{
			name:     "entrypoint only — no args",
			env:      map[string]string{"AGENT_ENTRYPOINT": "/bin/myapp"},
			wantArgv: []string{"/bin/myapp"},
		},
		{
			name: "entrypoint with args",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "--port 8080",
			},
			wantArgv: []string{"/bin/myapp", "--port", "8080"},
		},
		{
			name: "args with extra whitespace are trimmed",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "  -v  --debug  ",
			},
			wantArgv: []string{"/bin/myapp", "-v", "--debug"},
		},
		{
			name: "args with tabs and multiple spaces",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "--host\t127.0.0.1   --port 9090",
			},
			wantArgv: []string{"/bin/myapp", "--host", "127.0.0.1", "--port", "9090"},
		},
		{
			name: "empty AGENT_ENTRYPOINT_ARGS is ignored",
			env: map[string]string{
				"AGENT_ENTRYPOINT":      "/bin/myapp",
				"AGENT_ENTRYPOINT_ARGS": "",
			},
			wantArgv: []string{"/bin/myapp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadConfig(envMap(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadConfig() expected an error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}
			if len(cfg.Argv) != len(tt.wantArgv) {
				t.Fatalf("argv len = %d, want %d; got %v", len(cfg.Argv), len(tt.wantArgv), cfg.Argv)
			}
			for i, v := range cfg.Argv {
				if v != tt.wantArgv[i] {
					t.Errorf("argv[%d] = %q, want %q", i, v, tt.wantArgv[i])
				}
			}
		})
	}
}

// TestValidateEntrypoint covers the filesystem-validation logic.
func TestValidateEntrypoint(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("launcher targets Linux containers; skipping on Windows")
	}

	t.Run("valid executable passes", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/bin/sh"}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error for /bin/sh: %v", err)
		}
	})

	t.Run("extra argv elements do not affect validation", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/bin/sh", "-c", "echo hello"}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error when extra args present: %v", err)
		}
	})

	t.Run("nonexistent binary returns error", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{"/nonexistent/binary-does-not-exist"}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error for nonexistent binary, got nil")
		}
	})

	t.Run("directory returns error", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Argv: []string{t.TempDir()}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error when entrypoint is a directory, got nil")
		}
	})

	t.Run("non-executable file returns error", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "noexec-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.Name(), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Argv: []string{f.Name()}}
		if err := validateEntrypoint(cfg); err == nil {
			t.Error("expected error for non-executable file, got nil")
		}
	})

	t.Run("executable file passes", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "exec-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.Name(), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Argv: []string{f.Name()}}
		if err := validateEntrypoint(cfg); err != nil {
			t.Errorf("unexpected error for executable file: %v", err)
		}
	})
}
