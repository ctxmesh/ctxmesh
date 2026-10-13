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

package runcap

import "context"

type tokenCtxKey struct{}

// WithToken returns a context carrying a run capability's raw token, so a call made on the request's
// behalf can forward it: the egress sidecar hands the token service the capability it verified, and the
// service derives the user and boundary from it (ADR 0154).
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenCtxKey{}, token)
}

// TokenFrom returns the raw run capability WithToken stored, or "".
func TokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(tokenCtxKey{}).(string)
	return t
}
