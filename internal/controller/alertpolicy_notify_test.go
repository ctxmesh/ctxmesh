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

package controller

// alertpolicy_notify_test.go — plain unit tests for webhook dispatch (m70.5).
//
// No integration build tag: these tests do NOT use envtest. They construct a minimal
// AlertPolicyReconciler with a fake client (controller-runtime/pkg/client/fake) and an
// httptest.Server, exercising notifyChannels directly.
//
// Well-known Secret data key for HMAC signing: "signingKey" (mirrors the type comment in
// api/v1beta1/alertpolicy_types.go).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// notifyTestScheme builds a runtime.Scheme with v1beta1 and corev1 types registered.
func notifyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, agentsv1beta1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// signingSecret returns a corev1.Secret with the given key seeded under data["signingKey"].
func signingSecret(name, namespace string, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"signingKey": key},
	}
}

// mkNotifyAP returns a minimal AlertPolicy with the given channels. The name and namespace
// are configurable so tests can seed independent objects without collisions.
func mkNotifyAP(name, namespace string, channels []agentsv1beta1.AlertChannel) *agentsv1beta1.AlertPolicy {
	return &agentsv1beta1.AlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AlertPolicySpec{
			Conditions: []agentsv1beta1.AlertCondition{
				{Name: "c1", Type: "regressionDetected"},
			},
			Route: agentsv1beta1.AlertRoute{Channels: channels},
		},
	}
}

// notifyCond is a minimal AlertCondition used by dispatch tests.
var notifyCond = agentsv1beta1.AlertCondition{Name: "c1", Type: "regressionDetected"}

// TestAlertPolicyNotify_SignedWebhook verifies that a webhook channel with a secretRef receives a
// POST with the correct X-Ctxmesh-Signature HMAC-SHA256 header and a well-formed JSON body.
func TestAlertPolicyNotify_SignedWebhook(t *testing.T) {
	sigKey := []byte("super-secret-key")
	var receivedBody []byte
	var receivedSig string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		receivedSig = r.Header.Get("X-Ctxmesh-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ap := mkNotifyAP("test-ap", "default", []agentsv1beta1.AlertChannel{
		{
			Type: "webhook",
			Webhook: &agentsv1beta1.WebhookChannel{
				URL:       srv.URL,
				SecretRef: "hmac-secret",
			},
		},
	})

	s := notifyTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	require.NoError(t, fakeClient.Create(context.Background(), signingSecret("hmac-secret", "default", sigKey)))

	r := &AlertPolicyReconciler{Client: fakeClient, HTTPClient: srv.Client()}
	firedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	r.notifyChannels(context.Background(), ap, notifyCond, "agent-a", "fired msg", firedAt)

	require.NotEmpty(t, receivedBody, "expected webhook to receive a POST body")
	require.NotEmpty(t, receivedSig, "expected X-Ctxmesh-Signature header")

	// Verify the signature: sha256=<hex(HMAC-SHA256(body, key))>
	mac := hmac.New(sha256.New, sigKey)
	mac.Write(receivedBody)
	expectedSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expectedSig, receivedSig, "HMAC signature must match payload")
}

// TestAlertPolicyNotify_ConsoleNoHTTP verifies that a console channel does NOT trigger any HTTP
// call — the console feed is a pull model (cpDB alerts table, m70.6).
func TestAlertPolicyNotify_ConsoleNoHTTP(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ap := mkNotifyAP("test-ap", "default", []agentsv1beta1.AlertChannel{
		{Type: "console"},
	})

	s := notifyTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AlertPolicyReconciler{Client: fakeClient, HTTPClient: srv.Client()}

	r.notifyChannels(context.Background(), ap, notifyCond, "", "fired msg", time.Now())

	assert.False(t, called.Load(), "console channel must not trigger any HTTP call")
}

// TestAlertPolicyNotify_5xxRetriedThenGivesUp verifies that a 5xx response causes retries and that
// notifyChannels returns without error even after all retries are exhausted (the reconcile is not
// failed). The test server must receive more than 1 request.
func TestAlertPolicyNotify_5xxRetriedThenGivesUp(t *testing.T) {
	// Override retry delays to zero so the test is fast.
	origDelays := webhookRetryDelays
	webhookRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { webhookRetryDelays = origDelays })

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ap := mkNotifyAP("retry-ap", "ns-retry", []agentsv1beta1.AlertChannel{
		{
			Type:    "webhook",
			Webhook: &agentsv1beta1.WebhookChannel{URL: srv.URL},
		},
	})

	s := notifyTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AlertPolicyReconciler{Client: fakeClient, HTTPClient: srv.Client()}

	// Must complete without panic — notifyChannels is void and must not propagate errors.
	r.notifyChannels(context.Background(), ap, notifyCond, "", "fired msg", time.Now())

	assert.Greater(t, int(attempts.Load()), 1, "5xx should be retried (more than 1 attempt)")
}

// TestAlertPolicyNotify_UnsignedWebhook verifies that a webhook channel with no secretRef POSTs
// without an X-Ctxmesh-Signature header.
func TestAlertPolicyNotify_UnsignedWebhook(t *testing.T) {
	var receivedSig string
	var gotRequest atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest.Store(true)
		receivedSig = r.Header.Get("X-Ctxmesh-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ap := mkNotifyAP("test-ap", "default", []agentsv1beta1.AlertChannel{
		{
			Type:    "webhook",
			Webhook: &agentsv1beta1.WebhookChannel{URL: srv.URL /* no SecretRef */},
		},
	})

	s := notifyTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AlertPolicyReconciler{Client: fakeClient, HTTPClient: srv.Client()}

	r.notifyChannels(context.Background(), ap, notifyCond, "1.0/5.0", "fired msg", time.Now())

	assert.True(t, gotRequest.Load(), "unsigned webhook must still POST")
	assert.Empty(t, receivedSig, "unsigned webhook must not include X-Ctxmesh-Signature header")
}

// TestAlertPolicyNotify_Email verifies an email channel delivers via the injected SMTP sender with the
// configured recipients + a well-formed subject/body (M132, audit V1).
func TestAlertPolicyNotify_Email(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.example.com")
	var gotTo []string
	var gotSubject, gotBody string
	var called atomic.Bool

	ap := mkNotifyAP("email-ap", "default", []agentsv1beta1.AlertChannel{
		{Type: "email", Email: &agentsv1beta1.EmailChannel{To: []string{"ops@example.com", "sre@example.com"}}},
	})
	fakeClient := fake.NewClientBuilder().WithScheme(notifyTestScheme(t)).Build()
	r := &AlertPolicyReconciler{
		Client: fakeClient,
		SMTPSend: func(_ smtpConfig, to []string, subject, body string) error {
			called.Store(true)
			gotTo, gotSubject, gotBody = to, subject, body
			return nil
		},
	}
	r.notifyChannels(context.Background(), ap, notifyCond, "0.9", "regression detected", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))

	require.True(t, called.Load(), "the SMTP sender must be invoked for an email channel with a configured relay")
	assert.Equal(t, []string{"ops@example.com", "sre@example.com"}, gotTo)
	assert.Contains(t, gotSubject, "email-ap")
	assert.Contains(t, gotBody, "regression detected")
	assert.Contains(t, gotBody, "c1")
}

// TestAlertPolicyNotify_EmailUnconfiguredSkips verifies an unconfigured relay (no SMTP_HOST) SKIPS the
// email dispatch (fail-safe — never wedges alerting), leaving the sender uncalled.
func TestAlertPolicyNotify_EmailUnconfiguredSkips(t *testing.T) {
	t.Setenv("SMTP_HOST", "")
	var called atomic.Bool
	ap := mkNotifyAP("email-ap", "default", []agentsv1beta1.AlertChannel{
		{Type: "email", Email: &agentsv1beta1.EmailChannel{To: []string{"ops@example.com"}}},
	})
	fakeClient := fake.NewClientBuilder().WithScheme(notifyTestScheme(t)).Build()
	r := &AlertPolicyReconciler{
		Client:   fakeClient,
		SMTPSend: func(_ smtpConfig, _ []string, _, _ string) error { called.Store(true); return nil },
	}
	r.notifyChannels(context.Background(), ap, notifyCond, "0.9", "msg", time.Now())
	assert.False(t, called.Load(), "an unconfigured SMTP relay must SKIP, not call the sender")
}
