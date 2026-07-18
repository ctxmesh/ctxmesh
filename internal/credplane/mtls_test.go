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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/credplane"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

type testPKI struct {
	caPEM         []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
	clientCertPEM []byte
	clientKeyPEM  []byte
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func ecKeyPEM(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)
	return pemBlock("EC PRIVATE KEY", der)
}

// leaf signs a leaf certificate (server or client) with the CA.
func leaf(t *testing.T, serial int64, cn string, dns []string, eku []x509.ExtKeyUsage, ca *x509.Certificate, caKey *ecdsa.PrivateKey) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return pemBlock("CERTIFICATE", der), ecKeyPEM(t, key)
}

// genPKI mints a CA + a server cert (for serverName) + a client cert, all from one CA.
func genPKI(t *testing.T, serverName string) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	srvCert, srvKey := leaf(t, 2, serverName, []string{serverName}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, caCert, caKey)
	cliCert, cliKey := leaf(t, 3, "egress-sidecar", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, caCert, caKey)

	return testPKI{
		caPEM:         pemBlock("CERTIFICATE", caDER),
		serverCertPEM: srvCert,
		serverKeyPEM:  srvKey,
		clientCertPEM: cliCert,
		clientKeyPEM:  cliKey,
	}
}

// TestMTLSEnforced proves the central token service accepts only callers presenting a
// platform client certificate — a caller without one is rejected at the TLS handshake, so
// the credential API is never reachable unauthenticated (ADR 0030 §1).
func TestMTLSEnforced(t *testing.T) {
	const serverName = "token-service"
	pki := genPKI(t, serverName)

	srvTLS, err := credplane.ServerTLSConfig(pki.serverCertPEM, pki.serverKeyPEM, pki.caPEM)
	require.NoError(t, err)

	ts := httptest.NewUnstartedServer(credplane.NewServer(&mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "OK"}}, logr.Discard()).Handler())
	ts.TLS = srvTLS
	ts.StartTLS()
	t.Cleanup(ts.Close)

	// A sidecar presenting a platform client cert is accepted.
	cliTLS, err := credplane.ClientTLSConfig(pki.clientCertPEM, pki.clientKeyPEM, pki.caPEM, serverName)
	require.NoError(t, err)
	ok := credplane.NewClient(ts.URL, &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 5 * time.Second})
	got, err := ok.Resolve(context.Background(), testNS, "", testServer, "u-a")
	require.NoError(t, err, "a sidecar with a platform client cert is accepted")
	require.Equal(t, "OK", got.Value)

	// A caller with NO client cert (trusts the server, but presents nothing) is rejected by
	// RequireAndVerifyClientCert — the handshake fails before any request is served.
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pki.caPEM))
	noCert := credplane.NewClient(ts.URL, &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}},
		Timeout:   5 * time.Second,
	})
	_, err = noCert.Resolve(context.Background(), testNS, "", testServer, "u-a")
	require.Error(t, err, "a caller without a platform client cert must be rejected by mTLS")
}
