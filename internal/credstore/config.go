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
	"fmt"
	"net/url"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Config is the token service's credential-backend configuration (ADR 0152 §1). Its fields mirror
// the spec of the retired CredentialStore kind one for one, which is what lets the chart's
// tokenService.credentialBackend values take an old store's YAML unchanged. Exactly one provider is
// set.
type Config struct {
	Provider Provider
}

// Provider is the backend union: exactly one field is set.
type Provider struct {
	// Kubernetes stores grants as Secrets in the credential namespace: the zero-dependency default.
	Kubernetes *KubernetesProvider
	// Postgres stores envelope-encrypted grants in Postgres (the scale profile).
	Postgres *PostgresProvider
	// Remote dials an out-of-tree provider over the JSON-over-mTLS credprovider contract.
	Remote *RemoteProvider
}

// KubernetesProvider selects the built-in Kubernetes-Secret backend. Grants live in the token
// service's credential namespace (MCP_CREDENTIAL_NAMESPACE).
type KubernetesProvider struct{}

// PostgresProvider selects the Postgres reference backend.
type PostgresProvider struct {
	// DSNSecretRef locates the Postgres connection string.
	DSNSecretRef SecretKeyRef
	// Encryption configures envelope encryption of the stored tokens. A Postgres backend refuses
	// to store plaintext, so it is required.
	Encryption *EnvelopeEncryption
}

// EnvelopeEncryption configures the per-record data key's KEK custodian. Exactly one is set.
type EnvelopeEncryption struct {
	// LocalKEKSecretRef locates a 32-byte AES-256 KEK in a Secret.
	LocalKEKSecretRef *SecretKeyRef
	// OpenBaoTransit wraps data keys with a per-tenant OpenBao transit key.
	OpenBaoTransit *OpenBaoTransitKMS
	// KMSv2 points at a Kubernetes KMS v2 provider.
	KMSv2 *KMSv2Provider
}

// OpenBaoTransitKMS configures OpenBao (or Vault) transit as the KEK custodian.
type OpenBaoTransitKMS struct {
	Address        string
	TokenSecretRef SecretKeyRef
	// MountPath is the transit engine mount. Empty ⇒ "transit".
	MountPath string
	// KeyPrefix derives the per-tenant transit key name (<KeyPrefix><tenant>).
	KeyPrefix   string
	CASecretRef *SecretKeyRef
}

// KMSv2Provider references a Kubernetes KMS v2 gRPC provider.
type KMSv2Provider struct {
	Endpoint    string
	KeyIDPrefix string
}

// RemoteProvider selects an out-of-tree backend.
type RemoteProvider struct {
	// Endpoint is the provider's https base URL.
	Endpoint string
	// MTLS is required: a credential provider must be mutually authenticated.
	MTLS *MTLSClientConfig
}

// MTLSClientConfig locates the CA that verifies the provider and the client certificate the token
// service presents to it.
type MTLSClientConfig struct {
	CASecretRef SecretKeyRef
	// ClientTLSSecretName names a kubernetes.io/tls Secret (tls.crt + tls.key).
	ClientTLSSecretName string
}

// SecretKeyRef names one key of a Secret in the credential namespace.
type SecretKeyRef struct {
	Name string
	Key  string
}

// The backends CREDENTIAL_BACKEND accepts.
const (
	BackendKubernetes = "kubernetes"
	BackendPostgres   = "postgres"
	BackendRemote     = "remote"
)

// Backend names the selected provider, for logs.
func (c Config) Backend() string {
	switch {
	case c.Provider.Postgres != nil:
		return BackendPostgres
	case c.Provider.Remote != nil:
		return BackendRemote
	case c.Provider.Kubernetes != nil:
		return BackendKubernetes
	default:
		return "none"
	}
}

// The environment the token service reads its backend from. The chart renders these from
// tokenService.credentialBackend; each name spells the values path it comes from.
const (
	EnvBackend = "CREDENTIAL_BACKEND"

	EnvPostgresDSNSecretName          = "CREDENTIAL_BACKEND_POSTGRES_DSN_SECRET_NAME"
	EnvPostgresDSNSecretKey           = "CREDENTIAL_BACKEND_POSTGRES_DSN_SECRET_KEY"
	EnvPostgresLocalKEKSecretName     = "CREDENTIAL_BACKEND_POSTGRES_LOCAL_KEK_SECRET_NAME"
	EnvPostgresLocalKEKSecretKey      = "CREDENTIAL_BACKEND_POSTGRES_LOCAL_KEK_SECRET_KEY"
	EnvPostgresTransitAddress         = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_ADDRESS"
	EnvPostgresTransitTokenSecretName = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_TOKEN_SECRET_NAME"
	EnvPostgresTransitTokenSecretKey  = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_TOKEN_SECRET_KEY"
	EnvPostgresTransitMountPath       = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_MOUNT_PATH"
	EnvPostgresTransitKeyPrefix       = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_KEY_PREFIX"
	EnvPostgresTransitCASecretName    = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_CA_SECRET_NAME"
	EnvPostgresTransitCASecretKey     = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_CA_SECRET_KEY"
	EnvPostgresKMSv2Endpoint          = "CREDENTIAL_BACKEND_POSTGRES_KMSV2_ENDPOINT"
	EnvPostgresKMSv2KeyIDPrefix       = "CREDENTIAL_BACKEND_POSTGRES_KMSV2_KEY_ID_PREFIX"
	EnvRemoteEndpoint                 = "CREDENTIAL_BACKEND_REMOTE_ENDPOINT"
	EnvRemoteMTLSCASecretName         = "CREDENTIAL_BACKEND_REMOTE_MTLS_CA_SECRET_NAME"
	EnvRemoteMTLSCASecretKey          = "CREDENTIAL_BACKEND_REMOTE_MTLS_CA_SECRET_KEY"
	EnvRemoteMTLSClientTLSSecretName  = "CREDENTIAL_BACKEND_REMOTE_MTLS_CLIENT_TLS_SECRET_NAME"
)

const (
	envBackendPrefix          = EnvBackend + "_"
	envPostgresLocalKEKPrefix = "CREDENTIAL_BACKEND_POSTGRES_LOCAL_KEK_"
	envPostgresTransitPrefix  = "CREDENTIAL_BACKEND_POSTGRES_OPENBAO_TRANSIT_"
	envPostgresKMSv2Prefix    = "CREDENTIAL_BACKEND_POSTGRES_KMSV2_"

	errMissingVariable   = "%s is required by the %s credential backend"
	errInvalidSecretName = "%s=%q is not a valid Secret name: %s"
	errInvalidURL        = "%s=%q is not a valid %s URL"
	errCustodianCount    = "the postgres credential backend needs exactly one key-encryption custodian, got %s"
	custodianNoneHint    = "none: set " + EnvPostgresLocalKEKSecretName + ", " + EnvPostgresTransitAddress + " or " + EnvPostgresKMSv2Endpoint
)

// backendVars is every variable each backend reads. A CREDENTIAL_BACKEND_* variable the selected
// backend does not read is refused: it is either meant for a backend that is not selected, which is
// exactly the silent misconfiguration ADR 0152 retired, or a typo.
var backendVars = map[string][]string{
	BackendKubernetes: nil,
	BackendPostgres: {
		EnvPostgresDSNSecretName, EnvPostgresDSNSecretKey,
		EnvPostgresLocalKEKSecretName, EnvPostgresLocalKEKSecretKey,
		EnvPostgresTransitAddress, EnvPostgresTransitTokenSecretName, EnvPostgresTransitTokenSecretKey,
		EnvPostgresTransitMountPath, EnvPostgresTransitKeyPrefix,
		EnvPostgresTransitCASecretName, EnvPostgresTransitCASecretKey,
		EnvPostgresKMSv2Endpoint, EnvPostgresKMSv2KeyIDPrefix,
	},
	BackendRemote: {
		EnvRemoteEndpoint, EnvRemoteMTLSCASecretName, EnvRemoteMTLSCASecretKey, EnvRemoteMTLSClientTLSSecretName,
	},
}

// ConfigFromEnv builds the backend configuration from environ (os.Environ() form). An unset or empty
// CREDENTIAL_BACKEND selects kubernetes. It fails closed: an unknown backend, a variable the selected
// backend does not read, or a missing or malformed value returns an error naming the variable, and
// it never falls back to kubernetes when another backend was asked for.
func ConfigFromEnv(environ []string) (Config, error) {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || (k != EnvBackend && !strings.HasPrefix(k, envBackendPrefix)) {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			env[k] = v
		}
	}

	backend := strings.ToLower(env[EnvBackend])
	if backend == "" {
		backend = BackendKubernetes
	}
	allowed, ok := backendVars[backend]
	if !ok {
		return Config{}, fmt.Errorf("%s=%q is not a credential backend; use kubernetes, postgres or remote", EnvBackend, env[EnvBackend])
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k != EnvBackend && !slices.Contains(allowed, k) {
			return Config{}, fmt.Errorf("%s is set, but the %s credential backend does not read it (%s=%q)", k, backend, EnvBackend, backend)
		}
	}

	switch backend {
	case BackendPostgres:
		pg, err := postgresFromEnv(env)
		if err != nil {
			return Config{}, err
		}
		return Config{Provider: Provider{Postgres: pg}}, nil
	case BackendRemote:
		rm, err := remoteFromEnv(env)
		if err != nil {
			return Config{}, err
		}
		return Config{Provider: Provider{Remote: rm}}, nil
	default:
		return Config{Provider: Provider{Kubernetes: &KubernetesProvider{}}}, nil
	}
}

func postgresFromEnv(env map[string]string) (*PostgresProvider, error) {
	dsn, err := requiredSecretRef(env, BackendPostgres, EnvPostgresDSNSecretName, EnvPostgresDSNSecretKey)
	if err != nil {
		return nil, err
	}
	enc := &EnvelopeEncryption{}
	var custodians []string
	if anyWithPrefix(env, envPostgresLocalKEKPrefix) {
		ref, err := requiredSecretRef(env, BackendPostgres, EnvPostgresLocalKEKSecretName, EnvPostgresLocalKEKSecretKey)
		if err != nil {
			return nil, err
		}
		enc.LocalKEKSecretRef = &ref
		custodians = append(custodians, EnvPostgresLocalKEKSecretName)
	}
	if anyWithPrefix(env, envPostgresTransitPrefix) {
		transit, err := transitFromEnv(env)
		if err != nil {
			return nil, err
		}
		enc.OpenBaoTransit = transit
		custodians = append(custodians, EnvPostgresTransitAddress)
	}
	if anyWithPrefix(env, envPostgresKMSv2Prefix) {
		endpoint := env[EnvPostgresKMSv2Endpoint]
		if endpoint == "" {
			return nil, fmt.Errorf(errMissingVariable, EnvPostgresKMSv2Endpoint, BackendPostgres)
		}
		enc.KMSv2 = &KMSv2Provider{Endpoint: endpoint, KeyIDPrefix: env[EnvPostgresKMSv2KeyIDPrefix]}
		custodians = append(custodians, EnvPostgresKMSv2Endpoint)
	}
	switch len(custodians) {
	case 1:
	case 0:
		return nil, fmt.Errorf(errCustodianCount, custodianNoneHint)
	default:
		return nil, fmt.Errorf(errCustodianCount, strings.Join(custodians, ", "))
	}
	return &PostgresProvider{DSNSecretRef: dsn, Encryption: enc}, nil
}

func transitFromEnv(env map[string]string) (*OpenBaoTransitKMS, error) {
	addr := env[EnvPostgresTransitAddress]
	if addr == "" {
		return nil, fmt.Errorf(errMissingVariable, EnvPostgresTransitAddress, BackendPostgres)
	}
	if !validURL(addr, "http", "https") {
		return nil, fmt.Errorf(errInvalidURL, EnvPostgresTransitAddress, addr, "http(s)")
	}
	token, err := requiredSecretRef(env, BackendPostgres, EnvPostgresTransitTokenSecretName, EnvPostgresTransitTokenSecretKey)
	if err != nil {
		return nil, err
	}
	ca, err := optionalSecretRef(env, BackendPostgres, EnvPostgresTransitCASecretName, EnvPostgresTransitCASecretKey)
	if err != nil {
		return nil, err
	}
	return &OpenBaoTransitKMS{
		Address:        addr,
		TokenSecretRef: token,
		MountPath:      env[EnvPostgresTransitMountPath],
		KeyPrefix:      env[EnvPostgresTransitKeyPrefix],
		CASecretRef:    ca,
	}, nil
}

func remoteFromEnv(env map[string]string) (*RemoteProvider, error) {
	endpoint := env[EnvRemoteEndpoint]
	if endpoint == "" {
		return nil, fmt.Errorf(errMissingVariable, EnvRemoteEndpoint, BackendRemote)
	}
	if !validURL(endpoint, "https") {
		return nil, fmt.Errorf(errInvalidURL, EnvRemoteEndpoint, endpoint, "https")
	}
	ca, err := requiredSecretRef(env, BackendRemote, EnvRemoteMTLSCASecretName, EnvRemoteMTLSCASecretKey)
	if err != nil {
		return nil, err
	}
	clientTLS := env[EnvRemoteMTLSClientTLSSecretName]
	if clientTLS == "" {
		return nil, fmt.Errorf(errMissingVariable, EnvRemoteMTLSClientTLSSecretName, BackendRemote)
	}
	if errs := validation.IsDNS1123Subdomain(clientTLS); len(errs) > 0 {
		return nil, fmt.Errorf(errInvalidSecretName, EnvRemoteMTLSClientTLSSecretName, clientTLS, strings.Join(errs, "; "))
	}
	return &RemoteProvider{
		Endpoint: endpoint,
		MTLS:     &MTLSClientConfig{CASecretRef: ca, ClientTLSSecretName: clientTLS},
	}, nil
}

// requiredSecretRef reads a Secret reference both of whose variables must be set and well formed.
func requiredSecretRef(env map[string]string, backend, nameVar, keyVar string) (SecretKeyRef, error) {
	for _, v := range []string{nameVar, keyVar} {
		if env[v] == "" {
			return SecretKeyRef{}, fmt.Errorf(errMissingVariable, v, backend)
		}
	}
	ref := SecretKeyRef{Name: env[nameVar], Key: env[keyVar]}
	if errs := validation.IsDNS1123Subdomain(ref.Name); len(errs) > 0 {
		return SecretKeyRef{}, fmt.Errorf(errInvalidSecretName, nameVar, ref.Name, strings.Join(errs, "; "))
	}
	if errs := validation.IsConfigMapKey(ref.Key); len(errs) > 0 {
		return SecretKeyRef{}, fmt.Errorf("%s=%q is not a valid Secret key: %s", keyVar, ref.Key, strings.Join(errs, "; "))
	}
	return ref, nil
}

// optionalSecretRef reads a Secret reference that may be absent, but not half set.
func optionalSecretRef(env map[string]string, backend, nameVar, keyVar string) (*SecretKeyRef, error) {
	if env[nameVar] == "" && env[keyVar] == "" {
		return nil, nil
	}
	ref, err := requiredSecretRef(env, backend, nameVar, keyVar)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func anyWithPrefix(env map[string]string, prefix string) bool {
	for k := range env {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func validURL(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && slices.Contains(schemes, u.Scheme)
}
