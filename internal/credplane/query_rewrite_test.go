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

package credplane

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
)

type fakeRewriter struct {
	out string
	err error
}

func (f fakeRewriter) Rewrite(_ context.Context, _ string) (string, error) {
	return f.out, f.err
}

func TestApplyRewrite_UsesRewrittenQuery(t *testing.T) {
	s := NewServer(nil, logr.Discard()).WithRewriter(fakeRewriter{out: "reset forgotten account password login credentials"})
	got := s.applyRewrite(context.Background(), "how do I get back in")
	if got != "reset forgotten account password login credentials" {
		t.Fatalf("applyRewrite = %q, want the rewritten query", got)
	}
}

func TestApplyRewrite_FailOpenOnError(t *testing.T) {
	s := NewServer(nil, logr.Discard()).WithRewriter(fakeRewriter{err: errors.New("gateway down")})
	got := s.applyRewrite(context.Background(), "original query")
	if got != "original query" {
		t.Fatalf("applyRewrite on error = %q, want the original query (fail open)", got)
	}
}

func TestApplyRewrite_FailOpenOnEmpty(t *testing.T) {
	s := NewServer(nil, logr.Discard()).WithRewriter(fakeRewriter{out: "   "})
	got := s.applyRewrite(context.Background(), "original query")
	if got != "original query" {
		t.Fatalf("applyRewrite on empty = %q, want the original query (fail open)", got)
	}
}
