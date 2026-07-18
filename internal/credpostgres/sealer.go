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
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// Sealed is the envelope-encrypted form of token material (spec §Axis B): a per-record
// data key (DEK) encrypts the token; the DEK is wrapped by a key-encryption key (KEK) that
// never leaves its custodian. Storing Sealed alone (a DB dump) is inert without the KEK.
type Sealed struct {
	// KeyID identifies the KEK used (per-tenant → crypto-shred unit).
	KeyID string
	// WrappedDEK is the KEK-encrypted data key (nonce-prefixed).
	WrappedDEK []byte
	// Nonce is the AES-GCM nonce for Ciphertext.
	Nonce []byte
	// Ciphertext is the DEK-encrypted token material.
	Ciphertext []byte
}

// Sealer envelope-encrypts token material per tenant. Implementations must never log
// plaintext. LocalSealer (this file) wraps the DEK with a local, tenant-derived key;
// the KMS-v2 sealer (m27.5) wraps it via an external KMS for true per-tenant crypto-shred.
type Sealer interface {
	Seal(ctx context.Context, plaintext []byte, tenant string) (Sealed, error)
	Unseal(ctx context.Context, s Sealed, tenant string) ([]byte, error)
}

// dekLen is the AES-256 data-key length.
const dekLen = 32

// LocalSealer envelope-encrypts with a per-tenant key DERIVED from a local master KEK
// (HMAC-SHA256 domain separation). It is the m27.4 default; it gives encryption-at-rest and
// per-tenant domain separation, but NOT true crypto-shredding (you cannot destroy a derived
// key without the master) — that requires the KMS-v2 sealer (m27.5) with real per-tenant keys.
type LocalSealer struct {
	masterKEK []byte
}

// NewLocalSealer builds a LocalSealer from a 32-byte master KEK.
func NewLocalSealer(masterKEK []byte) (*LocalSealer, error) {
	if len(masterKEK) != dekLen {
		return nil, fmt.Errorf("credpostgres: master KEK must be %d bytes, got %d", dekLen, len(masterKEK))
	}
	return &LocalSealer{masterKEK: masterKEK}, nil
}

// tenantKEK derives a per-tenant key-encryption key from the master via HMAC-SHA256
// (a PRF), so each tenant's DEKs are wrapped under a distinct, domain-separated key.
func (l *LocalSealer) tenantKEK(tenant string) []byte {
	mac := hmac.New(sha256.New, l.masterKEK)
	mac.Write([]byte("credpostgres-kek:" + tenant))
	return mac.Sum(nil) // 32 bytes
}

func (l *LocalSealer) keyID(tenant string) string {
	if tenant == "" {
		return "local:default"
	}
	return "local:" + tenant
}

// Seal generates a fresh DEK, AES-256-GCM-encrypts the plaintext with it, and wraps the
// DEK under the tenant KEK.
func (l *LocalSealer) Seal(_ context.Context, plaintext []byte, tenant string) (Sealed, error) {
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Sealed{}, fmt.Errorf("credpostgres: gen DEK: %w", err)
	}
	nonce, ciphertext, err := gcmSeal(dek, plaintext)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := wrapDEK(l.tenantKEK(tenant), dek)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyID: l.keyID(tenant), WrappedDEK: wrapped, Nonce: nonce, Ciphertext: ciphertext}, nil
}

// Unseal unwraps the DEK under the tenant KEK and decrypts the token.
func (l *LocalSealer) Unseal(_ context.Context, s Sealed, tenant string) ([]byte, error) {
	if s.KeyID != l.keyID(tenant) {
		return nil, fmt.Errorf("credpostgres: sealed key id %q does not match tenant %q", s.KeyID, tenant)
	}
	dek, err := unwrapDEK(l.tenantKEK(tenant), s.WrappedDEK)
	if err != nil {
		return nil, err
	}
	return gcmOpen(dek, s.Nonce, s.Ciphertext)
}

// wrapDEK AES-256-GCM-encrypts the DEK under the KEK, prepending the nonce.
func wrapDEK(kek, dek []byte) ([]byte, error) {
	nonce, ct, err := gcmSeal(kek, dek)
	if err != nil {
		return nil, err
	}
	return append(nonce, ct...), nil
}

// unwrapDEK reverses wrapDEK.
func unwrapDEK(kek, wrapped []byte) ([]byte, error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(wrapped) < ns {
		return nil, errors.New("credpostgres: wrapped DEK too short")
	}
	return gcmOpen(kek, wrapped[:ns], wrapped[ns:])
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credpostgres: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credpostgres: gcm: %w", err)
	}
	return gcm, nil
}

func gcmSeal(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("credpostgres: gen nonce: %w", err)
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func gcmOpen(key, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("credpostgres: decrypt: %w", err)
	}
	return pt, nil
}
