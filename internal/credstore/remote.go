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

package credstore

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/credprovider"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// remoteTimeout bounds a call to an out-of-tree provider.
const remoteTimeout = 20 * time.Second

// Conventional keys in the client-TLS secret (a kubernetes.io/tls Secret).
const (
	tlsCertKey = "tls.crt"
	tlsKeyKey  = "tls.key"
)

// buildRemoteBackend constructs a credprovider.Client for a `remote` provider, loading its
// mTLS material from the referenced Secrets in the credential namespace. mTLS is REQUIRED
// for a remote backend — a missing/invalid Secret fails closed (no backend is built),
// never a plaintext dial to a credential provider.
func buildRemoteBackend(ctx context.Context, spec *agentsv1alpha1.CredentialProviderRemote, deps Deps) (credresolve.CredentialResolver, error) {
	if spec.MTLS == nil {
		return nil, fmt.Errorf("credstore: remote backend %q requires mtls (a remote credential provider must be mutually authenticated)", spec.Endpoint)
	}
	httpClient, err := remoteHTTPClient(ctx, spec.Endpoint, spec.MTLS, deps.Client, deps.DefaultCredentialNamespace)
	if err != nil {
		return nil, err
	}
	return credprovider.NewClient(spec.Endpoint, httpClient), nil
}

// remoteHTTPClient builds an mTLS http.Client for the provider at endpoint from the CA +
// client-TLS Secrets in credNS.
func remoteHTTPClient(ctx context.Context, endpoint string, mtls *agentsv1alpha1.MTLSClientConfig, reader client.Reader, credNS string) (*http.Client, error) {
	caPEM, err := secretValue(ctx, reader, credNS, mtls.CASecretRef.Name, mtls.CASecretRef.Key)
	if err != nil {
		return nil, fmt.Errorf("credstore: load remote CA: %w", err)
	}
	certPEM, err := secretValue(ctx, reader, credNS, mtls.ClientTLSSecretName, tlsCertKey)
	if err != nil {
		return nil, fmt.Errorf("credstore: load remote client cert: %w", err)
	}
	keyPEM, err := secretValue(ctx, reader, credNS, mtls.ClientTLSSecretName, tlsKeyKey)
	if err != nil {
		return nil, fmt.Errorf("credstore: load remote client key: %w", err)
	}

	serverName := ""
	if u, uErr := url.Parse(endpoint); uErr == nil {
		serverName = u.Hostname()
	}
	tlsCfg, err := credplane.ClientTLSConfig(certPEM, keyPEM, caPEM, serverName)
	if err != nil {
		return nil, fmt.Errorf("credstore: build remote mTLS config: %w", err)
	}
	return &http.Client{
		Timeout:   remoteTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

// secretValue reads one key from a Secret in credNS. A missing Secret/key is a real error
// (fail closed).
func secretValue(ctx context.Context, reader client.Reader, credNS, name, key string) ([]byte, error) {
	var sec corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: credNS, Name: name}, &sec); err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	v, ok := sec.Data[key]
	if !ok || len(v) == 0 {
		return nil, fmt.Errorf("secret %q has no key %q", name, key)
	}
	return v, nil
}
