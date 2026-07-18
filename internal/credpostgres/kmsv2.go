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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	kmsapi "k8s.io/kms/apis/v2"
)

// KMSv2Sealer wraps DEKs via a Kubernetes KMS v2 provider (the standardized gRPC contract,
// same as etcd encryption) so any existing KMS plugin (cloud KMS, SoftHSM/PKCS#11) drops in.
// It is SINGLE-KEY per plugin: it gives KEK-in-KMS custody + rotation, but NOT per-tenant
// crypto-shred (the plugin manages one key) — use TransitSealer for that (spec §Axis B).
// The token bytes are AES-256-GCM'd locally; only the small DEK crosses to the KMS.
type KMSv2Sealer struct {
	client kmsapi.KeyManagementServiceClient
	conn   *grpc.ClientConn
}

// kmsWrapped is the persisted form of a KMS-wrapped DEK — everything a later Decrypt needs.
type kmsWrapped struct {
	Ciphertext  []byte            `json:"c"`
	KeyID       string            `json:"k"`
	Annotations map[string][]byte `json:"a,omitempty"`
}

// NewKMSv2Sealer dials a KMS v2 provider (typically a unix socket, e.g.
// unix:///var/run/kms/socket). The transport is insecure — a KMS plugin is a LOCAL socket.
func NewKMSv2Sealer(endpoint string) (*KMSv2Sealer, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("credpostgres: dial KMS v2 %q: %w", endpoint, err)
	}
	return &KMSv2Sealer{client: kmsapi.NewKeyManagementServiceClient(conn), conn: conn}, nil
}

// NewKMSv2SealerWithClient builds a sealer over an existing client (for tests / bufconn).
func NewKMSv2SealerWithClient(c kmsapi.KeyManagementServiceClient) *KMSv2Sealer {
	return &KMSv2Sealer{client: c}
}

// Close releases the gRPC connection (no-op for a client-injected sealer).
func (k *KMSv2Sealer) Close() error {
	if k.conn != nil {
		return k.conn.Close()
	}
	return nil
}

// Seal generates a fresh DEK, AES-256-GCM-encrypts the token with it, and wraps the DEK via
// the KMS. tenant is passed as the request uid (correlation only — KMS v2 is single-key).
func (k *KMSv2Sealer) Seal(ctx context.Context, plaintext []byte, tenant string) (Sealed, error) {
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Sealed{}, fmt.Errorf("credpostgres: gen DEK: %w", err)
	}
	nonce, ciphertext, err := gcmSeal(dek, plaintext)
	if err != nil {
		return Sealed{}, err
	}
	resp, err := k.client.Encrypt(ctx, &kmsapi.EncryptRequest{Plaintext: dek, Uid: tenant})
	if err != nil {
		return Sealed{}, fmt.Errorf("credpostgres: KMS v2 encrypt: %w", err)
	}
	wrapped, err := json.Marshal(kmsWrapped{Ciphertext: resp.Ciphertext, KeyID: resp.KeyId, Annotations: resp.Annotations})
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyID: resp.KeyId, WrappedDEK: wrapped, Nonce: nonce, Ciphertext: ciphertext}, nil
}

// Unseal unwraps the DEK via the KMS and decrypts the token.
func (k *KMSv2Sealer) Unseal(ctx context.Context, s Sealed, tenant string) ([]byte, error) {
	var kw kmsWrapped
	if err := json.Unmarshal(s.WrappedDEK, &kw); err != nil {
		return nil, fmt.Errorf("credpostgres: decode KMS-wrapped DEK: %w", err)
	}
	resp, err := k.client.Decrypt(ctx, &kmsapi.DecryptRequest{
		Ciphertext: kw.Ciphertext, KeyId: kw.KeyID, Annotations: kw.Annotations, Uid: tenant,
	})
	if err != nil {
		return nil, fmt.Errorf("credpostgres: KMS v2 decrypt: %w", err)
	}
	return gcmOpen(resp.Plaintext, s.Nonce, s.Ciphertext)
}

// Compile-time assertion: a Sealer (but NOT a CryptoShredder — single-key).
var _ Sealer = (*KMSv2Sealer)(nil)
