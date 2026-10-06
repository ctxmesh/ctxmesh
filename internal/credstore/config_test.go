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
	"maps"
	"reflect"
	"strings"
	"testing"
)

func environ(kv map[string]string) []string {
	out := make([]string, 0, len(kv)+1)
	for k, v := range kv {
		out = append(out, k+"="+v)
	}
	// Unrelated variables are ignored, whatever they hold.
	return append(out, "PATH=/usr/bin", "CREDENTIALS_FILE=/x")
}

var pgBase = map[string]string{
	EnvBackend:               "postgres",
	EnvPostgresDSNSecretName: "credstore-dsn",
	EnvPostgresDSNSecretKey:  "dsn",
}

func with(base, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// TestConfigFromEnv_DefaultIsKubernetes: no CREDENTIAL_BACKEND, an empty one, or an explicit
// kubernetes all select the kubernetes backend (what every install without configuration ran on).
func TestConfigFromEnv_DefaultIsKubernetes(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":    {},
		"empty":    {EnvBackend: "  "},
		"explicit": {EnvBackend: "kubernetes"},
		"any case": {EnvBackend: "Kubernetes"},
	} {
		cfg, err := ConfigFromEnv(environ(env))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Provider.Kubernetes == nil || cfg.Backend() != BackendKubernetes {
			t.Fatalf("%s: got %+v, want the kubernetes backend", name, cfg)
		}
	}
}

// TestConfigFromEnv_EachBackendParses: every backend and custodian parses to the config its
// variables describe.
func TestConfigFromEnv_EachBackendParses(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"postgres + local KEK", with(pgBase, map[string]string{
			EnvPostgresLocalKEKSecretName: "credstore-kek", EnvPostgresLocalKEKSecretKey: "kek",
		}), Config{Provider: Provider{Postgres: &PostgresProvider{
			DSNSecretRef: SecretKeyRef{Name: "credstore-dsn", Key: "dsn"},
			Encryption:   &EnvelopeEncryption{LocalKEKSecretRef: &SecretKeyRef{Name: "credstore-kek", Key: "kek"}},
		}}}},
		{"postgres + openbao transit", with(pgBase, map[string]string{
			EnvPostgresTransitAddress:         "https://openbao.cred.svc:8200",
			EnvPostgresTransitTokenSecretName: "openbao-token",
			EnvPostgresTransitTokenSecretKey:  "token",
			EnvPostgresTransitMountPath:       "transit-ctx",
			EnvPostgresTransitKeyPrefix:       "tenant-",
			EnvPostgresTransitCASecretName:    "openbao-ca",
			EnvPostgresTransitCASecretKey:     "ca.crt",
		}), Config{Provider: Provider{Postgres: &PostgresProvider{
			DSNSecretRef: SecretKeyRef{Name: "credstore-dsn", Key: "dsn"},
			Encryption: &EnvelopeEncryption{OpenBaoTransit: &OpenBaoTransitKMS{
				Address:        "https://openbao.cred.svc:8200",
				TokenSecretRef: SecretKeyRef{Name: "openbao-token", Key: "token"},
				MountPath:      "transit-ctx",
				KeyPrefix:      "tenant-",
				CASecretRef:    &SecretKeyRef{Name: "openbao-ca", Key: "ca.crt"},
			}},
		}}}},
		{"postgres + kms v2", with(pgBase, map[string]string{
			EnvPostgresKMSv2Endpoint: "unix:///var/run/kms/socket", EnvPostgresKMSv2KeyIDPrefix: "kek-",
		}), Config{Provider: Provider{Postgres: &PostgresProvider{
			DSNSecretRef: SecretKeyRef{Name: "credstore-dsn", Key: "dsn"},
			Encryption:   &EnvelopeEncryption{KMSv2: &KMSv2Provider{Endpoint: "unix:///var/run/kms/socket", KeyIDPrefix: "kek-"}},
		}}}},
		{"remote", map[string]string{
			EnvBackend:                       "remote",
			EnvRemoteEndpoint:                "https://cred-backend.acme.svc:8443",
			EnvRemoteMTLSCASecretName:        "cred-backend-ca",
			EnvRemoteMTLSCASecretKey:         "ca.crt",
			EnvRemoteMTLSClientTLSSecretName: "token-service-client",
		}, Config{Provider: Provider{Remote: &RemoteProvider{
			Endpoint: "https://cred-backend.acme.svc:8443",
			MTLS: &MTLSClientConfig{
				CASecretRef:         SecretKeyRef{Name: "cred-backend-ca", Key: "ca.crt"},
				ClientTLSSecretName: "token-service-client",
			},
		}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConfigFromEnv(environ(tc.env))
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestConfigFromEnv_RefusesNamingTheVariable: every unknown backend, stray, missing or malformed
// value refuses, and the error names the variable an operator has to fix. None of them falls back to
// kubernetes.
func TestConfigFromEnv_RefusesNamingTheVariable(t *testing.T) {
	localKEK := map[string]string{EnvPostgresLocalKEKSecretName: "kek", EnvPostgresLocalKEKSecretKey: "kek"}
	remote := map[string]string{
		EnvBackend:                       "remote",
		EnvRemoteEndpoint:                "https://cred-backend:8443",
		EnvRemoteMTLSCASecretName:        "ca",
		EnvRemoteMTLSCASecretKey:         "ca.crt",
		EnvRemoteMTLSClientTLSSecretName: "client",
	}
	cases := []struct {
		name  string
		env   map[string]string
		names []string
	}{
		{"openbao is not a backend", map[string]string{EnvBackend: "openbao"}, []string{EnvBackend}},
		{"unknown backend", map[string]string{EnvBackend: "vault"}, []string{EnvBackend}},
		{
			"postgres value on the default backend",
			map[string]string{EnvPostgresDSNSecretName: "dsn"},
			[]string{EnvPostgresDSNSecretName},
		},
		{
			"remote value on the postgres backend", with(with(pgBase, localKEK), map[string]string{EnvRemoteEndpoint: "https://x"}),
			[]string{EnvRemoteEndpoint},
		},
		{
			"misspelled variable", with(with(pgBase, localKEK), map[string]string{"CREDENTIAL_BACKEND_POSTGRES_DSN_SECRT_NAME": "x"}),
			[]string{"CREDENTIAL_BACKEND_POSTGRES_DSN_SECRT_NAME"},
		},
		{
			"postgres without a DSN name", with(localKEK, map[string]string{EnvBackend: "postgres", EnvPostgresDSNSecretKey: "dsn"}),
			[]string{EnvPostgresDSNSecretName},
		},
		{
			"postgres without a DSN key", with(localKEK, map[string]string{EnvBackend: "postgres", EnvPostgresDSNSecretName: "dsn"}),
			[]string{EnvPostgresDSNSecretKey},
		},
		{
			"postgres DSN secret name malformed", with(with(pgBase, localKEK), map[string]string{EnvPostgresDSNSecretName: "Not_A_Name"}),
			[]string{EnvPostgresDSNSecretName},
		},
		{
			"postgres DSN secret key malformed", with(with(pgBase, localKEK), map[string]string{EnvPostgresDSNSecretKey: "a/b"}),
			[]string{EnvPostgresDSNSecretKey},
		},
		{"postgres without a custodian", pgBase, []string{EnvPostgresLocalKEKSecretName}},
		{
			"postgres with two custodians", with(with(pgBase, localKEK), map[string]string{EnvPostgresKMSv2Endpoint: "unix:///kms"}),
			[]string{EnvPostgresLocalKEKSecretName, EnvPostgresKMSv2Endpoint},
		},
		{
			"local KEK half set", with(pgBase, map[string]string{EnvPostgresLocalKEKSecretName: "kek"}),
			[]string{EnvPostgresLocalKEKSecretKey},
		},
		{
			"transit without a token", with(pgBase, map[string]string{EnvPostgresTransitAddress: "https://openbao:8200"}),
			[]string{EnvPostgresTransitTokenSecretName},
		},
		{"transit address malformed", with(pgBase, map[string]string{
			EnvPostgresTransitAddress: "openbao:8200", EnvPostgresTransitTokenSecretName: "t", EnvPostgresTransitTokenSecretKey: "t",
		}), []string{EnvPostgresTransitAddress}},
		{"transit CA half set", with(pgBase, map[string]string{
			EnvPostgresTransitAddress: "https://openbao:8200", EnvPostgresTransitTokenSecretName: "t", EnvPostgresTransitTokenSecretKey: "t",
			EnvPostgresTransitCASecretName: "ca",
		}), []string{EnvPostgresTransitCASecretKey}},
		{
			"kms v2 prefix without an endpoint", with(pgBase, map[string]string{EnvPostgresKMSv2KeyIDPrefix: "k-"}),
			[]string{EnvPostgresKMSv2Endpoint},
		},
		{"remote without an endpoint", with(remote, map[string]string{EnvRemoteEndpoint: ""}), []string{EnvRemoteEndpoint}},
		{
			"remote over plain http", with(remote, map[string]string{EnvRemoteEndpoint: "http://cred-backend:8443"}),
			[]string{EnvRemoteEndpoint},
		},
		{"remote without a CA", with(remote, map[string]string{EnvRemoteMTLSCASecretName: ""}), []string{EnvRemoteMTLSCASecretName}},
		{
			"remote without a client certificate", with(remote, map[string]string{EnvRemoteMTLSClientTLSSecretName: ""}),
			[]string{EnvRemoteMTLSClientTLSSecretName},
		},
		{
			"remote client certificate name malformed", with(remote, map[string]string{EnvRemoteMTLSClientTLSSecretName: "Bad Name"}),
			[]string{EnvRemoteMTLSClientTLSSecretName},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ConfigFromEnv(environ(tc.env))
			if err == nil {
				t.Fatalf("accepted as %s backend %+v; want a refusal", cfg.Backend(), cfg)
			}
			for _, n := range tc.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("error %q does not name %s", err, n)
				}
			}
		})
	}
}
