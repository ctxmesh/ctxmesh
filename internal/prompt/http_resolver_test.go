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

package prompt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

func TestRawContentURL(t *testing.T) {
	cases := []struct {
		name    string
		src     agentsv1alpha1.GitPromptSource
		want    string
		wantErr bool
	}{
		{
			name: "github repo with .git suffix",
			src:  agentsv1alpha1.GitPromptSource{Repo: "https://github.com/acme/prompts.git", Ref: "main", Path: "support/greeting-v1.md"},
			want: "https://raw.githubusercontent.com/acme/prompts/main/support/greeting-v1.md",
		},
		{
			name: "github repo, leading slash on path trimmed",
			src:  agentsv1alpha1.GitPromptSource{Repo: "https://github.com/acme/prompts", Ref: "v2", Path: "/greeting.md"},
			want: "https://raw.githubusercontent.com/acme/prompts/v2/greeting.md",
		},
		{
			name:    "non-github host unsupported",
			src:     agentsv1alpha1.GitPromptSource{Repo: "https://gitlab.com/acme/prompts.git", Ref: "main", Path: "p.md"},
			wantErr: true,
		},
		{
			name:    "incomplete pointer",
			src:     agentsv1alpha1.GitPromptSource{Repo: "https://github.com/acme/prompts", Ref: "", Path: "p.md"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawContentURL(tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// roundTripFunc adapts a func to http.RoundTripper for injecting canned responses.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newFakeResolver(status int, body, token string) *HTTPResolver {
	return &HTTPResolver{
		token: token,
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// The URL must be the mapped raw endpoint (proves Resolve calls rawContentURL).
			if !strings.HasPrefix(r.URL.String(), "https://raw.githubusercontent.com/") {
				return nil, errors.New("unexpected URL: " + r.URL.String())
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}
}

func TestHTTPResolverResolve(t *testing.T) {
	src := agentsv1alpha1.GitPromptSource{Repo: "https://github.com/acme/prompts.git", Ref: "main", Path: "support/greeting-v1.md"}

	t.Run("200 returns content + deterministic version", func(t *testing.T) {
		content := "You are Acme Support. Greet the customer warmly."
		r := newFakeResolver(http.StatusOK, content, "")
		got, err := r.Resolve(context.Background(), src)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Content != content {
			t.Fatalf("content mismatch: %q", got.Content)
		}
		if got.Version != Version(src, content) {
			t.Fatalf("version %q != Version(src,content) %q", got.Version, Version(src, content))
		}
	})

	t.Run("404 maps to ErrNotFound", func(t *testing.T) {
		r := newFakeResolver(http.StatusNotFound, "not found", "")
		_, err := r.Resolve(context.Background(), src)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("500 is a transient error, not ErrNotFound", func(t *testing.T) {
		r := newFakeResolver(http.StatusInternalServerError, "boom", "")
		_, err := r.Resolve(context.Background(), src)
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("want a non-ErrNotFound error, got %v", err)
		}
	})

	t.Run("unsupported host maps to ErrNotFound (pointer error)", func(t *testing.T) {
		r := NewHTTPResolver("")
		_, err := r.Resolve(context.Background(), agentsv1alpha1.GitPromptSource{
			Repo: "https://gitlab.com/acme/p.git", Ref: "main", Path: "p.md",
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound for unsupported host, got %v", err)
		}
	})
}
