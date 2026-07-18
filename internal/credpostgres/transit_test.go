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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeTransit is a FAITHFUL in-process OpenBao transit engine: each named key is a real
// in-memory AES-256 key, so encrypt/decrypt genuinely wrap/unwrap the DEK and DELETE makes
// a key's ciphertext unrecoverable (real crypto-shred), all without a live OpenBao.
type fakeTransit struct {
	mu   sync.Mutex
	keys map[string][]byte // name → aes key
}

func newFakeTransit() *fakeTransit { return &fakeTransit{keys: map[string][]byte{}} }

func (f *fakeTransit) handler() http.Handler {
	mux := http.NewServeMux()
	// create key
	mux.HandleFunc("POST /v1/transit/keys/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		if strings.HasSuffix(name, "/config") { // config subpath won't match, but guard
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, ok := f.keys[name]; !ok {
			k := make([]byte, 32)
			_, _ = io.ReadFull(rand.Reader, k)
			f.keys[name] = k
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/transit/keys/{name}/config", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/transit/keys/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.keys, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/transit/encrypt/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k, ok := f.keys[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such key", http.StatusBadRequest)
			return
		}
		var body struct {
			Plaintext string `json:"plaintext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pt, _ := base64.StdEncoding.DecodeString(body.Plaintext)
		nonce, ct, _ := gcmSeal(k, pt)
		wrapped := "vault:v1:" + base64.StdEncoding.EncodeToString(append(nonce, ct...))
		writeData(w, "ciphertext", wrapped)
	})
	mux.HandleFunc("POST /v1/transit/decrypt/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k, ok := f.keys[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such key (shredded)", http.StatusBadRequest)
			return
		}
		var body struct {
			Ciphertext string `json:"ciphertext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(body.Ciphertext, "vault:v1:"))
		gcm, _ := newGCM(k)
		ns := gcm.NonceSize()
		pt, err := gcmOpen(k, raw[:ns], raw[ns:])
		if err != nil {
			http.Error(w, "decrypt failed", http.StatusBadRequest)
			return
		}
		writeData(w, "plaintext", base64.StdEncoding.EncodeToString(pt))
	})
	return mux
}

func writeData(w http.ResponseWriter, k, v string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{k: v}})
}

func newTransitSealer(t *testing.T, url string) *TransitSealer {
	t.Helper()
	s, err := NewTransitSealer(TransitSealerConfig{Address: url, Token: "test-token", KeyPrefix: "tenant-"})
	if err != nil {
		t.Fatalf("NewTransitSealer: %v", err)
	}
	return s
}

// TestTransitSealer_RoundTrip: seal→unseal recovers the plaintext through a real transit
// key wrap; the stored ciphertext contains none of the plaintext (dump-is-inert).
func TestTransitSealer_RoundTrip(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newFakeTransit().handler())
	defer srv.Close()
	s := newTransitSealer(t, srv.URL)
	ctx := context.Background()
	pt := []byte("a-users-oauth-token")

	sealed, err := s.Seal(ctx, pt, "acme")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed.KeyID != "tenant-acme" {
		t.Fatalf("keyID = %q, want tenant-acme", sealed.KeyID)
	}
	if bytes.Contains(sealed.Ciphertext, pt) || bytes.Contains(sealed.WrappedDEK, pt) {
		t.Fatal("plaintext leaked into sealed material")
	}
	got, err := s.Unseal(ctx, sealed, "acme")
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("Unseal = (%q, %v), want %q", got, err, pt)
	}
}

// TestTransitSealer_CryptoShred: after destroying a tenant's transit key, that tenant's
// sealed material is permanently unrecoverable — real per-tenant crypto-shredding.
func TestTransitSealer_CryptoShred(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newFakeTransit().handler())
	defer srv.Close()
	s := newTransitSealer(t, srv.URL)
	ctx := context.Background()

	sealedA, _ := s.Seal(ctx, []byte("secret-a"), "acme")
	sealedB, _ := s.Seal(ctx, []byte("secret-b"), "beta")

	if err := s.CryptoShred(ctx, "acme"); err != nil {
		t.Fatalf("CryptoShred: %v", err)
	}
	// acme is unrecoverable...
	if _, err := s.Unseal(ctx, sealedA, "acme"); err == nil {
		t.Fatal("acme's material recovered AFTER crypto-shred — the key was not destroyed")
	}
	// ...but beta is unaffected (per-tenant isolation of the shred).
	if got, err := s.Unseal(ctx, sealedB, "beta"); err != nil || string(got) != "secret-b" {
		t.Fatalf("beta Unseal = (%q, %v) after shredding acme, want secret-b (isolation)", got, err)
	}
}
