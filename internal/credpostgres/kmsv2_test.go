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
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	kmsapi "k8s.io/kms/apis/v2"
)

// mockKMS is a FAITHFUL in-process KMS v2 plugin: a real in-memory AES-256 key wraps/unwraps
// the DEK, so Seal→Unseal genuinely round-trips through the standard gRPC contract without a
// real KMS.
type mockKMS struct {
	kmsapi.UnimplementedKeyManagementServiceServer
	key []byte
}

func (m *mockKMS) Status(context.Context, *kmsapi.StatusRequest) (*kmsapi.StatusResponse, error) {
	return &kmsapi.StatusResponse{Version: "v2", Healthz: "ok", KeyId: "mock-key"}, nil
}

func (m *mockKMS) Encrypt(_ context.Context, req *kmsapi.EncryptRequest) (*kmsapi.EncryptResponse, error) {
	nonce, ct, err := gcmSeal(m.key, req.Plaintext)
	if err != nil {
		return nil, err
	}
	return &kmsapi.EncryptResponse{Ciphertext: append(nonce, ct...), KeyId: "mock-key"}, nil
}

func (m *mockKMS) Decrypt(_ context.Context, req *kmsapi.DecryptRequest) (*kmsapi.DecryptResponse, error) {
	gcm, err := newGCM(m.key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	pt, err := gcmOpen(m.key, req.Ciphertext[:ns], req.Ciphertext[ns:])
	if err != nil {
		return nil, err
	}
	return &kmsapi.DecryptResponse{Plaintext: pt}, nil
}

func newMockKMSSealer(t *testing.T) *KMSv2Sealer {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	kmsapi.RegisterKeyManagementServiceServer(srv, &mockKMS{key: bytes.Repeat([]byte{0x44}, 32)})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewKMSv2SealerWithClient(kmsapi.NewKeyManagementServiceClient(conn))
}

// TestKMSv2Sealer_RoundTrip: seal→unseal recovers the plaintext through a real KMS v2 gRPC
// wrap/unwrap; the stored ciphertext + wrapped DEK contain none of the plaintext.
func TestKMSv2Sealer_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newMockKMSSealer(t)
	ctx := context.Background()
	pt := []byte("a-users-oauth-token")

	sealed, err := s.Seal(ctx, pt, "tenant-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, pt) || bytes.Contains(sealed.WrappedDEK, pt) {
		t.Fatal("plaintext leaked into sealed material")
	}
	got, err := s.Unseal(ctx, sealed, "tenant-a")
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("Unseal = (%q, %v), want %q", got, err, pt)
	}
}
