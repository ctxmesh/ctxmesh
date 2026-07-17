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

package credplane

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// The internal credential API carries per-user tokens between platform components, so it is
// mutually authenticated: the central service REQUIRES a client cert from the platform CA
// (only egress sidecars have one), and each sidecar verifies the server against the same CA.
// This is the SPIFFE/SDS-style service identity the two-tier topology relies on (ADR 0030).

// ServerTLSConfig builds the central service's TLS config: it presents (certPEM, keyPEM)
// and REQUIRES + verifies a client certificate signed by caPEM (mutual TLS), so an
// unauthenticated caller cannot ask the service to resolve a credential.
func ServerTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("credplane: load server keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("credplane: no valid certs in the client CA bundle")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLSConfig builds a delegating sidecar's TLS config: it presents (certPEM, keyPEM)
// and verifies the central service's certificate against caPEM. serverName is the name the
// server cert must match (the central service's in-cluster DNS name).
func ClientTLSConfig(certPEM, keyPEM, caPEM []byte, serverName string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("credplane: load client keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("credplane: no valid certs in the server CA bundle")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
