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

// Package oauthpending is the control-plane store for the short-lived, single-use records an MCP
// OAuth consent passes through (ADR 0040 amendment, m184.42): a pending authorization keyed by its
// OAuth state, and a consent ticket a chatbox hands to the console. They live in Postgres rather than
// one BFF pod's memory because the callback or the redeem may reach any replica.
//
// A record's handle (the state or the ticket) is a bearer capability, so it is never stored. The row is
// keyed by an HMAC of the handle and its payload is sealed with AES-256-GCM under a key derived from
// the handle. The payload holds the caller's bearer token and the PKCE verifier; a dump of the table
// yields neither without a live handle, and a handle lives only until its record is taken or expires.
package oauthpending

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// Kind separates the record families. The same handle under two kinds is two unrelated records.
type Kind string

const (
	// KindFlow is a pending OAuth authorization, keyed by its state and taken by the callback.
	KindFlow Kind = "flow"
	// KindTicket is a consent ticket, keyed by the ticket and taken by the console's redeem.
	KindTicket Kind = "ticket"
)

// MinHandleLen is the shortest handle accepted. Handles are the only key material, so a short one
// would make both the row id and the sealing key guessable.
const MinHandleLen = 32

// Store holds sealed records until they are taken or expire. Implementations never see a handle in
// stored form and never log one.
//
// Expiry is measured on the store's own clock, never a caller's: replicas' clocks disagree, and a
// replica running ahead would otherwise expire, or sweep away, records another replica just wrote.
// The Postgres store uses the database's now() for the expiry it writes, reads and sweeps.
type Store interface {
	// Put stores payload under handle for ttl, which must be positive. A duplicate handle of the
	// same kind is an error.
	Put(ctx context.Context, kind Kind, handle string, payload []byte, ttl time.Duration) error
	// Take removes the record for handle and returns its payload. It returns (nil, false, nil) when no
	// record exists, it was already taken, or it has expired; the record is removed in every case,
	// so a handle is good for one Take at most.
	Take(ctx context.Context, kind Kind, handle string) ([]byte, bool, error)
	// DeleteExpired removes every expired record and reports how many.
	DeleteExpired(ctx context.Context) (int64, error)
}

// Domain-separation labels: the id and the key derived from one handle must be unrelated values.
const (
	labelID  = "ctxmesh/oauthpending/id/v1|"
	labelKey = "ctxmesh/oauthpending/key/v1|"
)

func validKind(k Kind) bool { return k == KindFlow || k == KindTicket }

func checkTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("oauthpending: ttl %s is not positive: %w", ttl, controlplane.ErrInvalid)
	}
	return nil
}

func checkArgs(kind Kind, handle string) error {
	if !validKind(kind) {
		return fmt.Errorf("oauthpending: unknown kind %q: %w", kind, controlplane.ErrInvalid)
	}
	if len(handle) < MinHandleLen {
		return fmt.Errorf("oauthpending: handle shorter than %d bytes: %w", MinHandleLen, controlplane.ErrInvalid)
	}
	return nil
}

func derive(handle, label string, kind Kind) []byte {
	mac := hmac.New(sha256.New, []byte(handle))
	mac.Write([]byte(label + string(kind)))
	return mac.Sum(nil)
}

// recordID is the stored key for handle: an HMAC, so the handle cannot be recovered from it.
func recordID(kind Kind, handle string) string {
	return hex.EncodeToString(derive(handle, labelID, kind))
}

func newAEAD(kind Kind, handle string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(derive(handle, labelKey, kind))
	if err != nil {
		return nil, fmt.Errorf("oauthpending: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("oauthpending: gcm: %w", err)
	}
	return aead, nil
}

// seal returns the record id and the sealed payload (nonce || ciphertext). The id is the additional
// data, so a sealed payload moved to another row does not open.
func seal(kind Kind, handle string, payload []byte) (id string, sealed []byte, err error) {
	if err := checkArgs(kind, handle); err != nil {
		return "", nil, err
	}
	aead, err := newAEAD(kind, handle)
	if err != nil {
		return "", nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", nil, fmt.Errorf("oauthpending: nonce: %w", err)
	}
	id = recordID(kind, handle)
	return id, aead.Seal(nonce, nonce, payload, []byte(id)), nil
}

// open reverses seal for the record stored under id.
func open(kind Kind, handle, id string, sealed []byte) ([]byte, error) {
	aead, err := newAEAD(kind, handle)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("oauthpending: sealed record too short")
	}
	out, err := aead.Open(nil, sealed[:ns], sealed[ns:], []byte(id))
	if err != nil {
		return nil, fmt.Errorf("oauthpending: open record: %w", err)
	}
	return out, nil
}
