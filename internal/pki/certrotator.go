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

// Package pki wires the in-process platform cert-controller (M128/Gate E, ADR 0102):
// the manager generates + rotates its own CA + webhook serving cert (NO cert-manager
// dependency) via open-policy-agent/cert-controller, writing them to a Secret + the
// webhook server's CertDir and injecting the caBundle into the ValidatingWebhookConfiguration.
package pki

import (
	"fmt"
	"time"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// WebhookCertConfig is the (testable) input to the webhook cert-controller.
type WebhookCertConfig struct {
	// Namespace is the manager's install namespace — where the cert Secret lives and the
	// webhook Service resolves.
	Namespace string
	// ServiceName is the webhook Service (the serving cert's SAN + the VWC clientConfig target).
	ServiceName string
	// CertDir is the local directory the serving cert is written to; the controller-runtime
	// webhook server reads + hot-reloads from it (certwatcher). MUST match the manager's
	// --webhook-cert-path so the server sees the rotator's writes.
	CertDir string
	// SecretName is the durable Secret holding the CA + serving cert (synced across replicas).
	SecretName string
	// CAName / CAOrganization identify the self-signed platform CA.
	CAName         string
	CAOrganization string
	// ValidatingWebhookName, when set, is the VWC the rotator injects the caBundle into.
	// Empty (m128.4) provisions the cert only; m128.5 wires the VWC.
	ValidatingWebhookName string
	// CADuration / ServerCertDuration are the cert lifetimes (ADR 0102: CA ~5y, leaf ~90d).
	CADuration         time.Duration
	ServerCertDuration time.Duration
}

// dnsName is the serving cert's SAN — the SHORT service form `<svc>.<ns>.svc` ONLY.
// Per ADR 0102 (one-way door) we NEVER hardcode `cluster.local`: the cluster domain is
// operator-configurable, and a service-clientConfig VWC has the API server connect via the
// short name, so the short SAN is both sufficient and portable.
func (c WebhookCertConfig) dnsName() string {
	return fmt.Sprintf("%s.%s.svc", c.ServiceName, c.Namespace)
}

// rotator builds the CertRotator from the config (extracted so the wiring is unit-testable
// without a live manager).
func (c WebhookCertConfig) rotatorFor(ready chan struct{}) *rotator.CertRotator {
	var webhooks []rotator.WebhookInfo
	if c.ValidatingWebhookName != "" {
		webhooks = []rotator.WebhookInfo{{Name: c.ValidatingWebhookName, Type: rotator.Validating}}
	}
	cr := &rotator.CertRotator{
		SecretKey:      types.NamespacedName{Namespace: c.Namespace, Name: c.SecretName},
		CertDir:        c.CertDir,
		CAName:         c.CAName,
		CAOrganization: c.CAOrganization,
		DNSName:        c.dnsName(),
		IsReady:        ready,
		Webhooks:       webhooks,
		// The controller-runtime webhook server hot-reloads the CertDir via certwatcher, so a
		// rotation does NOT require a pod restart (rotation binds at the next TLS handshake).
		RestartOnSecretRefresh: false,
		// RequireLeaderElection stays FALSE (unset) — LOAD-BEARING, do not "harden" to leader-only.
		// cmd/main.go closes `certReady` (→ registers the webhook handlers + passes the webhook-server
		// readyz check) on EVERY replica. If the rotator ran only on the leader, non-leader replicas
		// would never see the cert on disk, never pass readyz, and every rollout would wedge on
		// readiness. Multi-replica first-boot races on the Secret are benign (validCACert guard +
		// Update-conflict retry). The VWC caBundle apply is separately leader-gated (mgr.Add).
	}
	if c.CADuration > 0 {
		cr.CaCertDuration = c.CADuration
	}
	if c.ServerCertDuration > 0 {
		cr.ServerCertDuration = c.ServerCertDuration
	}
	return cr
}

// SetupWebhookCertRotator registers the cert-controller on the manager and returns a channel
// that is CLOSED once the CA + serving cert are bootstrapped into CertDir — callers gate the
// ValidatingWebhookConfiguration creation on it so a webhook is never wired before its cert
// exists (ADR 0102 §1: no first-boot race).
func SetupWebhookCertRotator(mgr manager.Manager, c WebhookCertConfig) (<-chan struct{}, error) {
	ready := make(chan struct{})
	if err := rotator.AddRotator(mgr, c.rotatorFor(ready)); err != nil {
		return nil, fmt.Errorf("pki: add webhook cert rotator: %w", err)
	}
	return ready, nil
}
