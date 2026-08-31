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

package credplane_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// writerResolver is a mockResolver that ALSO persists grants (a config-selected backend).
type writerResolver struct {
	mockResolver
	storeCalls                         int
	gotG                               credresolve.Grant
	stNS, stBoundary, stServer, stUser string
}

func (wr *writerResolver) StoreGrant(_ context.Context, ns, boundary, server, userHash string, g credresolve.Grant) error {
	wr.storeCalls++
	wr.stNS, wr.stBoundary, wr.stServer, wr.stUser, wr.gotG = ns, boundary, server, userHash, g
	return nil
}

// TestDelegationStore: the BFF-side client delegates a grant persist to the central service,
// which routes it to the (writer) backend with the full token material intact.
func TestDelegationStore(t *testing.T) {
	t.Parallel()
	wr := &writerResolver{}
	client := newDelegation(t, wr)

	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	g := credresolve.Grant{
		Tokens:    credresolve.Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: exp},
		Config:    credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid", RevocationEndpoint: "https://as/revoke"},
		ServerURL: "https://mcp.example/mcp",
	}
	if err := client.StoreGrant(context.Background(), "ns", "", "srv", "uh", g); err != nil {
		t.Fatalf("StoreGrant: %v", err)
	}
	if wr.storeCalls != 1 || wr.stNS != "ns" || wr.stServer != "srv" || wr.stUser != "uh" {
		t.Fatalf("store routed wrong: calls=%d (%s/%s/%s)", wr.storeCalls, wr.stNS, wr.stServer, wr.stUser)
	}
	if wr.gotG.Tokens.AccessToken != "at" || wr.gotG.Tokens.RefreshToken != "rt" ||
		!wr.gotG.Tokens.ExpiresAt.Equal(exp) || wr.gotG.Config.ClientID != "cid" || wr.gotG.ServerURL != "https://mcp.example/mcp" {
		t.Fatalf("grant material not preserved across the wire: %+v", wr.gotG)
	}
}

// TestDelegationStore_UnsupportedBackend: a read-only backend (no GrantWriter) fails closed
// with a stable "unsupported" code — never a silent partial write.
func TestDelegationStore_UnsupportedBackend(t *testing.T) {
	t.Parallel()
	client := newDelegation(t, &mockResolver{}) // mockResolver is not a GrantWriter

	err := client.StoreGrant(context.Background(), "ns", "", "srv", "uh", credresolve.Grant{})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("StoreGrant on a read-only backend = %v, want an 'unsupported' error (fail closed)", err)
	}
}
