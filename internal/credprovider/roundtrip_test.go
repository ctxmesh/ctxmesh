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

package credprovider_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/ctxmesh/agent-engine/internal/credprovider"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// fakeBackend is an in-memory Backend double — the shape a third-party vault (or the m27.6
// conformance harness) implements.
type fakeBackend struct {
	caps      credprovider.Capabilities
	resolveFn func(ns, boundary, server, userHash, tenant string) (credresolve.Credential, error)
	stored    map[string]credprovider.GrantMaterial
	revoked   map[string]bool
}

func key(ns, boundary, server, userHash string) string {
	return ns + "|" + boundary + "|" + server + "|" + userHash
}

func (f *fakeBackend) Capabilities(context.Context) (credprovider.Capabilities, error) {
	return f.caps, nil
}

func (f *fakeBackend) Resolve(_ context.Context, ns, boundary, server, userHash, tenant string) (credresolve.Credential, error) {
	return f.resolveFn(ns, boundary, server, userHash, tenant)
}

func (f *fakeBackend) Store(_ context.Context, ns, boundary, server, userHash, _ string, g credprovider.GrantMaterial) error {
	f.stored[key(ns, boundary, server, userHash)] = g
	return nil
}

func (f *fakeBackend) Revoke(_ context.Context, ns, boundary, server, userHash, _ string) error {
	f.revoked[key(ns, boundary, server, userHash)] = true
	return nil
}

func newServer(b credprovider.Backend) (*credprovider.Client, func()) {
	srv := httptest.NewServer(credprovider.NewHandler(b))
	return credprovider.NewClient(srv.URL, srv.Client()), srv.Close
}

// TestRoundTrip_Capabilities: the client reads the backend's self-declared capabilities.
func TestRoundTrip_Capabilities(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{caps: credprovider.Capabilities{SelfRefresh: true, OwnEncryption: true}}
	c, closeFn := newServer(b)
	defer closeFn()

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.SelfRefresh || !caps.OwnEncryption || caps.APIVersion != credprovider.APIVersion {
		t.Fatalf("caps = %+v, want selfRefresh+ownEncryption and apiVersion %q", caps, credprovider.APIVersion)
	}
}

// TestRoundTrip_ResolveOutcomes: a success returns the credential; consent_required and
// no_credential map to the credresolve sentinels; a backend failure is an error, NEVER a
// blank credential (fail closed).
func TestRoundTrip_ResolveOutcomes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fn       func(ns, boundary, server, userHash, tenant string) (credresolve.Credential, error)
		wantCred credresolve.Credential
		wantErr  error
	}{
		"success": {fn: func(_, _, _, _, _ string) (credresolve.Credential, error) {
			return credresolve.Credential{Kind: "bearer", Value: "tok"}, nil
		}, wantCred: credresolve.Credential{Kind: "bearer", Value: "tok"}},
		"consent_required": {fn: func(_, _, _, _, _ string) (credresolve.Credential, error) {
			return credresolve.Credential{}, credresolve.ErrConsentRequired
		}, wantErr: credresolve.ErrConsentRequired},
		"no_credential": {fn: func(_, _, _, _, _ string) (credresolve.Credential, error) {
			return credresolve.Credential{}, credresolve.ErrNoCredential
		}, wantErr: credresolve.ErrNoCredential},
		"backend_error": {fn: func(_, _, _, _, _ string) (credresolve.Credential, error) {
			return credresolve.Credential{}, errors.New("boom")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, closeFn := newServer(&fakeBackend{resolveFn: tc.fn})
			defer closeFn()

			cred, err := c.Resolve(context.Background(), "ns", "", "srv", "uh")
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case name == "backend_error":
				if err == nil {
					t.Fatal("backend error must surface as an error, not a blank credential (fail closed)")
				}
				if cred.Value != "" {
					t.Fatalf("blank-credential leaked on error: %+v", cred)
				}
			default:
				if err != nil || cred != tc.wantCred {
					t.Fatalf("Resolve = (%+v, %v), want %+v", cred, err, tc.wantCred)
				}
			}
		})
	}
}

// TestRoundTrip_StoreAndRevoke: store persists the grant; revoke marks it revoked.
func TestRoundTrip_StoreAndRevoke(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{stored: map[string]credprovider.GrantMaterial{}, revoked: map[string]bool{}}
	c, closeFn := newServer(b)
	defer closeFn()
	ctx := context.Background()

	if err := c.Store(ctx, "ns", "", "srv", "uh", "tenant-a", credprovider.GrantMaterial{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got := b.stored[key("ns", "", "srv", "uh")]; got.AccessToken != "at" || got.RefreshToken != "rt" {
		t.Fatalf("stored = %+v, want the grant material", got)
	}
	if err := c.Revoke(ctx, "ns", "", "srv", "uh"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !b.revoked[key("ns", "", "srv", "uh")] {
		t.Fatal("revoke did not reach the backend")
	}
}

// TestFailClosed_ServerDown: an unreachable provider yields an error, never a blank
// credential — the plane never injects nothing-as-something.
func TestFailClosed_ServerDown(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(credprovider.NewHandler(&fakeBackend{
		resolveFn: func(_, _, _, _, _ string) (credresolve.Credential, error) {
			return credresolve.Credential{Kind: "bearer", Value: "tok"}, nil
		},
	}))
	c := credprovider.NewClient(srv.URL, srv.Client())
	srv.Close() // provider is now down

	cred, err := c.Resolve(context.Background(), "ns", "", "srv", "uh")
	if err == nil {
		t.Fatal("a down provider must error, not succeed with a blank credential")
	}
	if cred.Value != "" {
		t.Fatalf("blank credential leaked from a down provider: %+v", cred)
	}
}
