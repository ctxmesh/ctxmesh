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

package credpostgres

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestIntegration_TransitSealer_RealOpenBao: seal→unseal→crypto-shred against a REAL OpenBao
// transit engine (so the HTTP contract — paths, token header, ciphertext shape — is verified
// against the actual product, not just the faithful mock). Skips unless the env is set:
//
//	OPENBAO_TEST_ADDR=http://localhost:8200 OPENBAO_TEST_TOKEN=root
func TestIntegration_TransitSealer_RealOpenBao(t *testing.T) {
	addr, token := os.Getenv("OPENBAO_TEST_ADDR"), os.Getenv("OPENBAO_TEST_TOKEN")
	if addr == "" || token == "" {
		t.Skip("set OPENBAO_TEST_ADDR + OPENBAO_TEST_TOKEN (transit enabled) to run the real-OpenBao test")
	}
	s, err := NewTransitSealer(TransitSealerConfig{Address: addr, Token: token, KeyPrefix: "credtest-"})
	if err != nil {
		t.Fatalf("NewTransitSealer: %v", err)
	}
	ctx := context.Background()
	pt := []byte("real-openbao-oauth-token")

	sealed, err := s.Seal(ctx, pt, "acme")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := s.Unseal(ctx, sealed, "acme")
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("Unseal = (%q, %v), want %q", got, err, pt)
	}
	if err := s.CryptoShred(ctx, "acme"); err != nil {
		t.Fatalf("CryptoShred: %v", err)
	}
	if _, err := s.Unseal(ctx, sealed, "acme"); err == nil {
		t.Fatal("material recovered after crypto-shred against real OpenBao")
	}
}
