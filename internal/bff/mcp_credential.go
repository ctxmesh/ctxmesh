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

package bff

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// The (server, user) credential-resolution SEAM (ADR 0016 §5).
//
// The whole point of this seam is that per-user OAuth (M17) is a FILL-IN, not a
// retrofit. A tool call to a BYO-MCP server needs a credential; who that
// credential belongs to differs by milestone:
//
//   - M14 (this file): SHARED / service credential. The key the registering user
//     pasted is stored once as a Secret + SecretBinding (register flow). Every
//     invoking user's tool call to that server uses the SAME shared key. The
//     resolver is keyed (server, user) even though it ignores `user` today — the
//     interface shape is what makes M17 additive.
//
//   - M17 (later): PER-USER on-behalf-of. The first tool use → the invoking user
//     consents → their OAuth grant is stored as a Secret labeled (user, server) →
//     the resolver returns THAT user's token (with refresh/revocation). Because
//     the seam already takes `user`, M17 swaps the resolver body — no caller
//     changes, no new interface.
//
// The credential is resolved + attached SERVER-SIDE at the egress hop only (the
// tool proxy / launcher, m14.6b) — never the browser, never the agent container.
// This file is the seam and the M14 shared-credential implementation; the loop
// plumbing that CALLS it is m14.6b.

// errNoMCPCredential is returned when a server was registered without a key
// (an unauthenticated MCP server). The caller treats it as "attach no
// Authorization header" — not an error condition for an open server.
var errNoMCPCredential = errors.New("no credential registered for this MCP server")

// MCPCredential is a resolved credential for one (server, user) pair. In M14 it
// is always the shared bearer key from the register flow; in M17 it becomes the
// invoking user's OAuth access token. Kind lets m14.6b/M17 branch on how to
// attach it (today always "bearer").
type MCPCredential struct {
	// Kind is the credential scheme. "bearer" (the only M14 value) → the value is
	// attached as an Authorization: Bearer header at the egress hop.
	Kind string
	// Value is the secret material (the shared key now, a per-user token in M17).
	// It is resolved SERVER-SIDE and attached at the egress hop — it never reaches
	// the browser or the agent container. Do not log it.
	Value string
}

// MCPCredentialResolver resolves the credential to attach to a tool call for a
// given (server, user). The seam is keyed by BOTH from day one so M17's per-user
// OAuth is a resolver swap, not a caller change (ADR 0016 §5).
type MCPCredentialResolver interface {
	// Resolve returns the credential for (server, user) in namespace ns. It returns
	// errNoMCPCredential when the server has no stored key (an open server — attach
	// nothing). Any other error is a real read failure the caller surfaces.
	//
	// M14: returns the SHARED key (ignores user). M17: returns the invoking user's
	// grant (uses user). The signature is stable across the swap.
	Resolve(ctx context.Context, ns, server, user string) (MCPCredential, error)
}

// sharedSecretCredentialResolver is the M14 implementation: it resolves the
// SHARED bearer key the register flow stored (the Secret named after the server,
// via its SecretBinding). It ignores `user` (all users share the service key) —
// but takes it, so M17 can fill in per-user resolution behind the same interface.
//
// The read runs through the client the caller passes in (caller-scoped at
// invoke, or the tool-proxy's control-plane identity at the egress hop —
// whichever m14.6b wires). The Secret is the SAME trust domain as the provider
// keys (ADR 0016): readable only by the control-plane credential component.
type sharedSecretCredentialResolver struct {
	// reader reads the SecretBinding + Secret. The tool proxy (m14.6b) supplies the
	// control-plane reader; tests supply a fake client.
	reader client.Client
}

// NewSharedSecretCredentialResolver returns the M14 (server, user) resolver
// backed by the shared register-flow Secret. reader reads the SecretBinding +
// Secret server-side.
func NewSharedSecretCredentialResolver(reader client.Client) MCPCredentialResolver {
	return &sharedSecretCredentialResolver{reader: reader}
}

// Resolve implements MCPCredentialResolver for the M14 shared-key case. It
// resolves the register-flow SecretBinding (named after the server) → its Secret
// → the api-key value, and returns it as a bearer credential. A server with no
// SecretBinding (registered without a key) yields errNoMCPCredential. `user` is
// accepted but unused in M14 — the seam is keyed (server, user) so M17 fills in
// per-user resolution here without changing callers.
func (r *sharedSecretCredentialResolver) Resolve(ctx context.Context, ns, server, user string) (MCPCredential, error) {
	_ = user // M14: shared/service credential — user is the M17 fill-in point.

	// The register flow names the SecretBinding after the server. Absent → the
	// server is unauthenticated; attach nothing.
	var binding agentsv1alpha1.SecretBinding
	if err := r.reader.Get(ctx, client.ObjectKey{Name: server, Namespace: ns}, &binding); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return MCPCredential{}, errNoMCPCredential
		}
		return MCPCredential{}, err
	}

	var secret corev1.Secret
	if err := r.reader.Get(ctx, client.ObjectKey{
		Name:      binding.Spec.SecretRef.Name,
		Namespace: ns,
	}, &secret); err != nil {
		return MCPCredential{}, err
	}
	keyBytes, ok := secret.Data[binding.Spec.SecretRef.Key]
	if !ok || len(keyBytes) == 0 {
		return MCPCredential{}, errNoMCPCredential
	}
	return MCPCredential{Kind: "bearer", Value: string(keyBytes)}, nil
}
