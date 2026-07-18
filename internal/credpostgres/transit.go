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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CryptoShredder is the optional capability of destroying a tenant's KEK so its ciphertext
// becomes permanently unrecoverable (GDPR erasure / tenant offboarding) without scanning
// rows. Advertised via Capabilities.cryptoShred (a LocalSealer does NOT implement it).
type CryptoShredder interface {
	CryptoShred(ctx context.Context, tenant string) error
}

// transitTimeout bounds an OpenBao transit call.
const transitTimeout = 15 * time.Second

// TransitSealer wraps DEKs with a named per-tenant OpenBao (or Vault) transit key: the KEK
// never leaves OpenBao, and crypto-shred is a real key-delete. The token bytes are still
// AES-256-GCM'd locally with a per-record DEK (transit wraps only the small DEK — keeping
// OpenBao off the token-bytes path).
type TransitSealer struct {
	base      string // <address>/v1/<mount>
	token     string
	keyPrefix string
	http      *http.Client
}

// TransitSealerConfig configures a TransitSealer.
type TransitSealerConfig struct {
	Address   string
	MountPath string // default "transit"
	Token     string
	KeyPrefix string
	HTTP      *http.Client // supplies TLS/CA + timeout; nil ⇒ default
}

// NewTransitSealer builds a TransitSealer.
func NewTransitSealer(cfg TransitSealerConfig) (*TransitSealer, error) {
	if cfg.Address == "" || cfg.Token == "" {
		return nil, fmt.Errorf("credpostgres: transit sealer needs an address and token")
	}
	mount := cfg.MountPath
	if mount == "" {
		mount = "transit"
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: transitTimeout}
	}
	return &TransitSealer{
		base:      strings.TrimRight(cfg.Address, "/") + "/v1/" + strings.Trim(mount, "/"),
		token:     cfg.Token,
		keyPrefix: cfg.KeyPrefix,
		http:      hc,
	}, nil
}

func (t *TransitSealer) keyName(tenant string) string {
	if tenant == "" {
		return t.keyPrefix + "default"
	}
	return t.keyPrefix + tenant
}

// Seal AES-GCM-encrypts the token with a fresh DEK, then wraps the DEK under the tenant's
// transit key (creating it on first use).
func (t *TransitSealer) Seal(ctx context.Context, plaintext []byte, tenant string) (Sealed, error) {
	key := t.keyName(tenant)
	if err := t.ensureKey(ctx, key); err != nil {
		return Sealed{}, err
	}
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Sealed{}, fmt.Errorf("credpostgres: gen DEK: %w", err)
	}
	nonce, ciphertext, err := gcmSeal(dek, plaintext)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := t.transitEncrypt(ctx, key, dek)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyID: key, WrappedDEK: []byte(wrapped), Nonce: nonce, Ciphertext: ciphertext}, nil
}

// Unseal unwraps the DEK via transit and decrypts the token.
func (t *TransitSealer) Unseal(ctx context.Context, s Sealed, tenant string) ([]byte, error) {
	if s.KeyID != t.keyName(tenant) {
		return nil, fmt.Errorf("credpostgres: sealed key id %q does not match tenant %q", s.KeyID, tenant)
	}
	dek, err := t.transitDecrypt(ctx, s.KeyID, string(s.WrappedDEK))
	if err != nil {
		return nil, err
	}
	return gcmOpen(dek, s.Nonce, s.Ciphertext)
}

// CryptoShred allows deletion then deletes the tenant's transit key — all of that tenant's
// wrapped DEKs (and thus tokens) become permanently unrecoverable.
func (t *TransitSealer) CryptoShred(ctx context.Context, tenant string) error {
	key := t.keyName(tenant)
	// deletion must be explicitly allowed on the key before it can be deleted.
	if err := t.post(ctx, "/keys/"+key+"/config", map[string]any{"deletion_allowed": true}, nil); err != nil {
		return fmt.Errorf("credpostgres: allow transit key deletion: %w", err)
	}
	if err := t.do(ctx, http.MethodDelete, "/keys/"+key, nil, nil); err != nil {
		return fmt.Errorf("credpostgres: delete transit key: %w", err)
	}
	return nil
}

func (t *TransitSealer) ensureKey(ctx context.Context, key string) error {
	// POST keys/<name> creates the key; creating an existing key is a no-op (204).
	if err := t.post(ctx, "/keys/"+key, map[string]any{"type": "aes256-gcm96"}, nil); err != nil {
		return fmt.Errorf("credpostgres: ensure transit key: %w", err)
	}
	return nil
}

func (t *TransitSealer) transitEncrypt(ctx context.Context, key string, dek []byte) (string, error) {
	var resp struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	body := map[string]any{"plaintext": base64.StdEncoding.EncodeToString(dek)}
	if err := t.post(ctx, "/encrypt/"+key, body, &resp); err != nil {
		return "", fmt.Errorf("credpostgres: transit encrypt: %w", err)
	}
	if resp.Data.Ciphertext == "" {
		return "", fmt.Errorf("credpostgres: transit encrypt returned no ciphertext")
	}
	return resp.Data.Ciphertext, nil
}

func (t *TransitSealer) transitDecrypt(ctx context.Context, key, wrapped string) ([]byte, error) {
	var resp struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	body := map[string]any{"ciphertext": wrapped}
	if err := t.post(ctx, "/decrypt/"+key, body, &resp); err != nil {
		return nil, fmt.Errorf("credpostgres: transit decrypt: %w", err)
	}
	dek, err := base64.StdEncoding.DecodeString(resp.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("credpostgres: decode transit plaintext: %w", err)
	}
	return dek, nil
}

func (t *TransitSealer) post(ctx context.Context, path string, body, out any) error {
	return t.do(ctx, http.MethodPost, path, body, out)
}

func (t *TransitSealer) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", t.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("openbao %s %s → %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode openbao response: %w", err)
		}
	}
	return nil
}

// Compile-time assertions.
var (
	_ Sealer         = (*TransitSealer)(nil)
	_ CryptoShredder = (*TransitSealer)(nil)
)
