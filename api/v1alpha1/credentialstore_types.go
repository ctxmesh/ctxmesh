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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// CredentialProviderKubernetes selects the built-in Kubernetes-Secret backend — the
// zero-dependency default (air-gapped self-hosted). Per-user OBO grants are HMAC'd
// Secrets in the locked credential namespace; the plane does the OAuth refresh.
type CredentialProviderKubernetes struct {
	// credentialNamespace overrides the locked namespace that holds per-user grant
	// Secrets. Empty ⇒ the token-service's configured default (TOKEN_SERVICE_CREDENTIAL_NS).
	// +optional
	CredentialNamespace string `json:"credentialNamespace,omitempty"`
}

// CredentialProviderPostgres selects the Postgres reference backend (the scale profile,
// m27.4): encrypted token rows keyed by the HMAC'd grant identity, with the plane's
// refresh decorator on top. Ciphertext is envelope-encrypted per Encryption (Axis B).
type CredentialProviderPostgres struct {
	// dsnSecretRef locates the Postgres connection string (a Secret key). Required.
	DSNSecretRef SecretKeyRef `json:"dsnSecretRef"`

	// encryption configures envelope encryption of the stored tokens (Axis B). When
	// unset the plane refuses to store plaintext — a Postgres backend MUST have a KEK.
	// +optional
	Encryption *EnvelopeEncryption `json:"encryption,omitempty"`
}

// CredentialProviderOpenBao selects an OpenBao/Vault backend (KV store and/or transit).
// Reserved for m27; schema stub so the CRD contract is stable.
type CredentialProviderOpenBao struct {
	// address is the OpenBao/Vault API address (e.g. https://openbao.cred.svc:8200).
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// CredentialProviderGRPC selects an out-of-tree gRPC backend (bring-your-own vault,
// m27.3): the token-service dials it over mTLS and adapts it to the resolver interface.
type CredentialProviderGRPC struct {
	// endpoint is the gRPC target (e.g. dns:///cred-backend.acme.svc:8443). Required.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// mtls configures the client mTLS the token-service presents to the provider.
	// +optional
	MTLS *MTLSClientConfig `json:"mtls,omitempty"`
}

// EnvelopeEncryption configures the plane's envelope encryption for a passive backend:
// a per-record AES-256-GCM data key wrapped by a KEK that never leaves the KMS.
type EnvelopeEncryption struct {
	// kmsV2 points at a Kubernetes KMS v2 provider that wraps/unwraps the data keys.
	KMSv2 *KMSv2Provider `json:"kmsV2"`
}

// KMSv2Provider references a Kubernetes KMS v2 gRPC provider (the same contract used for
// etcd encryption at rest), so any existing KMS plugin (OpenBao, cloud KMS, SoftHSM) fits.
type KMSv2Provider struct {
	// endpoint is the KMS v2 gRPC endpoint (typically a unix socket, e.g.
	// unix:///var/run/kms/socket). Required.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// keyIDPrefix derives the per-tenant KEK id (keyID = <prefix><tenant>) so tenant
	// deletion is a KEK-destroy (crypto-shredding). Empty ⇒ a single install-wide key.
	// +optional
	KeyIDPrefix string `json:"keyIDPrefix,omitempty"`
}

// MTLSClientConfig locates the CA + client cert the token-service uses to authenticate
// to a gRPC backend.
type MTLSClientConfig struct {
	// caSecretRef locates the CA bundle that verifies the provider's server cert. Required.
	CASecretRef SecretKeyRef `json:"caSecretRef"`

	// clientCertSecretRef locates the client cert+key the token-service presents. Required.
	ClientCertSecretRef SecretKeyRef `json:"clientCertSecretRef"`
}

// CredentialStoreProvider is the backend union: exactly one field is set. It mirrors the
// External Secrets Operator SecretStore provider model — the backend is a config choice,
// not a rebuild (ADR 0032).
// +kubebuilder:validation:XValidation:rule="[has(self.kubernetes), has(self.postgres), has(self.openbao), has(self.grpc)].filter(x, x).size() == 1",message="exactly one provider (kubernetes, postgres, openbao, grpc) must be set"
type CredentialStoreProvider struct {
	// kubernetes selects the built-in Kubernetes-Secret backend (the zero-dep default).
	// +optional
	Kubernetes *CredentialProviderKubernetes `json:"kubernetes,omitempty"`

	// postgres selects the Postgres reference backend (the scale profile).
	// +optional
	Postgres *CredentialProviderPostgres `json:"postgres,omitempty"`

	// openbao selects an OpenBao/Vault backend.
	// +optional
	OpenBao *CredentialProviderOpenBao `json:"openbao,omitempty"`

	// grpc selects an out-of-tree gRPC backend (bring-your-own vault).
	// +optional
	GRPC *CredentialProviderGRPC `json:"grpc,omitempty"`
}

// CredentialStoreSpec is the shared spec for CredentialStore and ClusterCredentialStore.
type CredentialStoreSpec struct {
	// provider selects and configures the credential backend for this store.
	// +required
	Provider CredentialStoreProvider `json:"provider"`
}

// CredentialStoreStatus is the shared observed state.
type CredentialStoreStatus struct {
	// conditions reflect backend selection/health. "Ready" is True once the token-service
	// has constructed and health-checked the selected backend.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=credstore
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CredentialStore selects the credential backend for OBO grants in its own namespace,
// overriding the cluster default. Modeled on ESO's namespaced SecretStore (ADR 0032).
type CredentialStore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec selects the backend for this namespace.
	// +required
	Spec CredentialStoreSpec `json:"spec"`

	// status is the observed backend state.
	// +optional
	Status CredentialStoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CredentialStoreList contains a list of CredentialStore.
type CredentialStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CredentialStore `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=clustercredstore
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterCredentialStore is the cluster-wide default credential backend, used for any
// namespace without its own CredentialStore. Modeled on ESO's ClusterSecretStore. When
// no ClusterCredentialStore exists, the token-service defaults to the kubernetes backend
// (existing installs are unchanged).
type ClusterCredentialStore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec selects the default backend.
	// +required
	Spec CredentialStoreSpec `json:"spec"`

	// status is the observed backend state.
	// +optional
	Status CredentialStoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterCredentialStoreList contains a list of ClusterCredentialStore.
type ClusterCredentialStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterCredentialStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&CredentialStore{}, &CredentialStoreList{},
			&ClusterCredentialStore{}, &ClusterCredentialStoreList{},
		)
		return nil
	})
}
