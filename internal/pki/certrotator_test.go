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

package pki

import (
	"testing"
	"time"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseCfg() WebhookCertConfig {
	return WebhookCertConfig{
		Namespace:          "agentry",
		ServiceName:        "webhook-service",
		CertDir:            "/tmp/k8s-webhook-server/serving-certs",
		SecretName:         "agentry-webhook-server-cert",
		CAName:             "agentry-ca",
		CAOrganization:     "agentry",
		CADuration:         5 * 365 * 24 * time.Hour,
		ServerCertDuration: 90 * 24 * time.Hour,
	}
}

func TestDNSName_ShortServiceForm_NoClusterLocal(t *testing.T) {
	// ADR 0102 one-way door: the SAN is the SHORT `<svc>.<ns>.svc` form and must NEVER
	// hardcode the operator-configurable cluster domain.
	got := baseCfg().dnsName()
	assert.Equal(t, "webhook-service.agentry.svc", got)
	assert.NotContains(t, got, "cluster.local", "must not hardcode the cluster domain")
}

func TestRotatorFor_MapsConfig(t *testing.T) {
	ready := make(chan struct{})
	cr := baseCfg().rotatorFor(ready)

	assert.Equal(t, "agentry", cr.SecretKey.Namespace)
	assert.Equal(t, "agentry-webhook-server-cert", cr.SecretKey.Name)
	assert.Equal(t, "/tmp/k8s-webhook-server/serving-certs", cr.CertDir)
	assert.Equal(t, "webhook-service.agentry.svc", cr.DNSName)
	assert.Equal(t, "agentry-ca", cr.CAName)
	assert.False(t, cr.RestartOnSecretRefresh, "certwatcher hot-reloads — no pod restart on rotation")
	assert.Equal(t, 5*365*24*time.Hour, cr.CaCertDuration)
	assert.Equal(t, 90*24*time.Hour, cr.ServerCertDuration)
}

func TestRotatorFor_NoWebhookUntilNamed(t *testing.T) {
	// m128.4 provisions the cert only (no VWC injection); m128.5 sets ValidatingWebhookName.
	cr := baseCfg().rotatorFor(make(chan struct{}))
	assert.Empty(t, cr.Webhooks, "no VWC injection until a webhook is named (m128.5)")
}

func TestRotatorFor_InjectsNamedValidatingWebhook(t *testing.T) {
	cfg := baseCfg()
	cfg.ValidatingWebhookName = "tenant-label-validator"
	cr := cfg.rotatorFor(make(chan struct{}))
	require.Len(t, cr.Webhooks, 1)
	assert.Equal(t, "tenant-label-validator", cr.Webhooks[0].Name)
	assert.Equal(t, rotator.Validating, cr.Webhooks[0].Type,
		"the tenant-label webhook is a ValidatingWebhook — its caBundle gets injected")
}
