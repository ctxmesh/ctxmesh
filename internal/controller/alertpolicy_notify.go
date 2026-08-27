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

// alertpolicy_notify.go — webhook dispatcher for fired AlertPolicy alerts (m70.5).
//
// HMAC signing: when WebhookChannel.SecretRef is non-empty, the controller reads the named Secret
// (in the AlertPolicy's namespace) and takes the HMAC key from the Secret data key "signingKey".
// If the Secret is missing or the "signingKey" key is absent the dispatch is SKIPPED and an error
// is logged — the safer default over sending unsigned (avoids the receiver accepting a forged payload).
// The signature header is: X-Ctxmesh-Signature: sha256=<hex-encoded HMAC-SHA256>.
//
// Retry policy: up to 2 retries (3 total attempts) with a 200ms / 400ms backoff on transport errors
// or 5xx responses. Non-2xx after all retries → log error and continue; a bad webhook must never
// wedge alerting or fail the reconcile.
//
// HTTPClient injection: AlertPolicyReconciler.HTTPClient — nil defaults to a 5 s-timeout client.
// Tests can point this at an httptest.Server.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get

// webhookNotifyTimeout is the per-attempt HTTP timeout for webhook POSTs.
const webhookNotifyTimeout = 5 * time.Second

// channelTypeWebhook and channelTypeConsole are the canonical channel type strings defined by the
// AlertChannel.type enum in api/v1beta1/alertpolicy_types.go.
const (
	channelTypeWebhook = "webhook"
	channelTypeConsole = "console"
	channelTypeEmail   = "email"
)

// webhookRetryDelays lists the inter-attempt back-off durations. len(webhookRetryDelays)+1 == total
// attempts (1 initial + len retries). A zero-length slice means no retries.
var webhookRetryDelays = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}

// webhookPayload is the JSON body sent to an external webhook on a fired alert.
//
// RunID + Link are set ONLY for the per-run approvalWaiting notification (M75, ADR 0069 §3): Link is a
// POINTER to the AUTHENTICATED console approval view (never the public share link, never an approve-magic-
// link — approval stays caller-scoped via POST /api/runs/{id}/resume). Both are omitempty so aggregate-
// condition payloads are byte-for-byte unchanged.
type webhookPayload struct {
	Policy    string `json:"policy"`
	Namespace string `json:"namespace"`
	Condition string `json:"condition"`
	Type      string `json:"type"`
	Value     string `json:"value"`
	Message   string `json:"message"`
	FiredAt   string `json:"firedAt"`         // RFC3339
	RunID     string `json:"runId,omitempty"` // approvalWaiting: the waiting run's id
	Link      string `json:"link,omitempty"`  // approvalWaiting: deep-link to the console approval view
}

// httpClient returns the effective HTTP client: the injected one if set, otherwise a default with
// webhookNotifyTimeout. The default is constructed on every call (cheap; avoids a global).
func (r *AlertPolicyReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: webhookNotifyTimeout}
}

// notifyChannels dispatches alert notifications for a fired condition to every channel in
// ap.Spec.Route.Channels. It is called from recordFired AFTER the durable alert is appended, so
// persistence succeeds even if a webhook fails.
//
// console channels are no-ops here — the alerts table IS the console pull feed (m70.6 reads it).
// webhook channels POST a JSON payload, optionally HMAC-signed with the key from the named Secret.
//
// notifyChannels never returns an error: any per-channel failure is logged and the loop continues.
func (r *AlertPolicyReconciler) notifyChannels(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	value, msg string,
	firedAt time.Time,
) {
	r.dispatchChannels(ctx, ap, cond, webhookPayload{
		Policy:    ap.Name,
		Namespace: ap.Namespace,
		Condition: cond.Name,
		Type:      cond.Type,
		Value:     value,
		Message:   msg,
		FiredAt:   firedAt.UTC().Format(time.RFC3339),
	})
}

// dispatchChannels marshals a payload once and delivers it to every channel in ap.Spec.Route.Channels
// (console = a no-op pull feed; webhook = a signed POST). It is the shared body of notifyChannels
// (aggregate conditions) and notifyApprovalChannels (per-run approvalWaiting). It never returns an
// error: any per-channel failure is logged and the loop continues.
func (r *AlertPolicyReconciler) dispatchChannels(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	payload webhookPayload,
) {
	log := logf.FromContext(ctx)

	body, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal on a plain struct with no custom marshalers cannot fail in practice, but guard.
		log.Error(err, "marshalling webhook payload failed — skipping all webhook channels",
			"alertpolicy", ap.Name, "condition", cond.Name)
		return
	}

	for i, ch := range ap.Spec.Route.Channels {
		switch ch.Type {
		case channelTypeConsole:
			// no-op: the alerts table IS the console pull feed.

		case channelTypeWebhook:
			if ch.Webhook == nil || ch.Webhook.URL == "" {
				log.V(1).Info("webhook channel has no URL — skipping",
					"alertpolicy", ap.Name, "condition", cond.Name, "channelIndex", i)
				continue
			}
			if err := r.dispatchWebhook(ctx, ap, cond, ch.Webhook, body); err != nil {
				// Already logged inside dispatchWebhook with full context.
				log.Error(err, "webhook dispatch failed (continuing)",
					"alertpolicy", ap.Name, "condition", cond.Name, "url", ch.Webhook.URL)
			}

		case channelTypeEmail:
			if ch.Email == nil || len(ch.Email.To) == 0 {
				log.V(1).Info("email channel has no recipients — skipping",
					"alertpolicy", ap.Name, "condition", cond.Name, "channelIndex", i)
				continue
			}
			if err := r.dispatchEmail(ctx, ap, cond, ch.Email, payload); err != nil {
				log.Error(err, "email dispatch failed (continuing)",
					"alertpolicy", ap.Name, "condition", cond.Name, "to", ch.Email.To)
			}

		default:
			log.V(1).Info("unknown channel type — skipping",
				"alertpolicy", ap.Name, "condition", cond.Name, "type", ch.Type)
		}
	}
}

// notifyApprovalChannels dispatches a PER-RUN approvalWaiting notification (M75, ADR 0069 §3). The
// payload carries the waiting run's id + a deep-link to the AUTHENTICATED console approval view (a
// POINTER) — never the public share link, never an approve-magic-link (approval stays caller-scoped via
// POST /api/runs/{id}/resume). It reuses the exact same channel dispatch as the aggregate conditions.
func (r *AlertPolicyReconciler) notifyApprovalChannels(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	w WaitingApprovalRun,
	msg, link string,
	firedAt time.Time,
) {
	r.dispatchChannels(ctx, ap, cond, webhookPayload{
		Policy:    ap.Name,
		Namespace: ap.Namespace,
		Condition: cond.Name,
		Type:      condTypeApprovalWaiting,
		Value:     w.Agent,
		Message:   msg,
		FiredAt:   firedAt.UTC().Format(time.RFC3339),
		RunID:     w.ID,
		Link:      link,
	})
}

// dispatchWebhook POSTs body to the configured URL with optional HMAC signing. It retries on
// transport errors and 5xx responses (see webhookRetryDelays). Returns an error only for logging —
// the caller (notifyChannels) never propagates it up to the reconcile loop.
func (r *AlertPolicyReconciler) dispatchWebhook(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	wh *agentsv1beta1.WebhookChannel,
	body []byte,
) error {
	log := logf.FromContext(ctx)

	// Resolve the signing key (if any) before the retry loop — we fail-safe skip if the Secret
	// or key is missing.
	var signingKey []byte
	signed := false
	if wh.SecretRef != "" {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: wh.SecretRef, Namespace: ap.Namespace}, &secret); err != nil {
			log.Error(err, "reading webhook signing Secret — skipping dispatch",
				"alertpolicy", ap.Name, "condition", cond.Name,
				"secretRef", wh.SecretRef, "namespace", ap.Namespace)
			return fmt.Errorf("reading signing Secret %q: %w", wh.SecretRef, err)
		}
		key, ok := secret.Data["signingKey"]
		if !ok || len(key) == 0 {
			err := fmt.Errorf("secret %q has no \"signingKey\" data key", wh.SecretRef)
			log.Error(err, "signing key absent — skipping dispatch",
				"alertpolicy", ap.Name, "condition", cond.Name, "secretRef", wh.SecretRef)
			return err
		}
		signingKey = key
		signed = true
	}

	// Compute HMAC once (body is immutable across retries).
	var sigHeader string
	if signed {
		mac := hmac.New(sha256.New, signingKey)
		mac.Write(body) //nolint:errcheck // hash.Hash.Write never returns an error
		sigHeader = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	hc := r.httpClient()

	// Attempt loop: 1 initial attempt + up to len(webhookRetryDelays) retries.
	var lastErr error
	for attempt := 0; attempt <= len(webhookRetryDelays); attempt++ {
		if attempt > 0 {
			delay := webhookRetryDelays[attempt-1]
			log.V(1).Info("retrying webhook POST",
				"alertpolicy", ap.Name, "condition", cond.Name,
				"url", wh.URL, "attempt", attempt+1, "backoff", delay)
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during webhook retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
		if err != nil {
			// URL is malformed — no point retrying.
			return fmt.Errorf("building webhook request for %q: %w", wh.URL, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if signed {
			req.Header.Set("X-Ctxmesh-Signature", sigHeader)
		}

		resp, err := hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("POST to %q (attempt %d): %w", wh.URL, attempt+1, err)
			log.V(1).Info("webhook POST transport error",
				"alertpolicy", ap.Name, "condition", cond.Name,
				"url", wh.URL, "attempt", attempt+1, "err", err.Error())
			continue // retry
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.V(1).Info("webhook POST succeeded",
				"alertpolicy", ap.Name, "condition", cond.Name,
				"url", wh.URL, "status", resp.StatusCode)
			return nil
		}

		lastErr = fmt.Errorf("POST to %q (attempt %d): non-2xx status %d", wh.URL, attempt+1, resp.StatusCode)
		log.V(1).Info("webhook POST non-2xx",
			"alertpolicy", ap.Name, "condition", cond.Name,
			"url", wh.URL, "attempt", attempt+1, "status", resp.StatusCode)

		if resp.StatusCode < 500 {
			// 4xx — retrying won't help.
			break
		}
		// 5xx — continue to retry
	}

	return lastErr
}

// ── Email channel (M132, audit V1) ─────────────────────────────────────────────

// smtpConfig is the platform SMTP relay config for email alert delivery. Read from the controller
// env; an empty Host means "no relay configured" → email channels skip (logged, never wedge alerting).
type smtpConfig struct {
	Host, Port, From, Username, Password string
}

// smtpConfigFromEnv reads SMTP_* into an smtpConfig (Host empty ⇒ unconfigured; Port defaults to 587).
func smtpConfigFromEnv() smtpConfig {
	port := os.Getenv("SMTP_PORT")
	if port == "" {
		port = "587"
	}
	return smtpConfig{
		Host:     os.Getenv("SMTP_HOST"),
		Port:     port,
		From:     os.Getenv("SMTP_FROM"),
		Username: os.Getenv("SMTP_USERNAME"),
		Password: os.Getenv("SMTP_PASSWORD"),
	}
}

// smtpSender returns the injected sender or the default net/smtp sender.
func (r *AlertPolicyReconciler) smtpSender() func(cfg smtpConfig, to []string, subject, body string) error {
	if r.SMTPSend != nil {
		return r.SMTPSend
	}
	return defaultSMTPSend
}

// defaultSMTPSend delivers via net/smtp, using PLAIN auth when credentials are present. From falls back
// to SMTP_USERNAME then a platform default.
func defaultSMTPSend(cfg smtpConfig, to []string, subject, body string) error {
	from := cfg.From
	if from == "" {
		from = cfg.Username
	}
	if from == "" {
		from = "alerts@agent-engine.local"
	}
	msg := buildEmailMessage(from, to, subject, body)
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	return smtp.SendMail(net.JoinHostPort(cfg.Host, cfg.Port), auth, from, to, []byte(msg))
}

// buildEmailMessage assembles a minimal RFC 5322 text/plain message (headers + body).
func buildEmailMessage(from string, to []string, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(body)
	return b.String()
}

// dispatchEmail delivers a fired-alert notification via the platform SMTP relay. An unconfigured relay
// (no SMTP_HOST) SKIPS with a log — the same fail-safe posture as a bad webhook, never wedging alerting.
func (r *AlertPolicyReconciler) dispatchEmail(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	em *agentsv1beta1.EmailChannel,
	payload webhookPayload,
) error {
	log := logf.FromContext(ctx)
	cfg := smtpConfigFromEnv()
	if cfg.Host == "" {
		log.V(1).Info("SMTP relay not configured (SMTP_HOST unset) — skipping email channel",
			"alertpolicy", ap.Name, "condition", cond.Name, "to", em.To)
		return nil
	}
	subject := em.Subject
	if subject == "" {
		subject = fmt.Sprintf("[agent-engine] alert: %s/%s (%s)", payload.Namespace, payload.Policy, payload.Condition)
	}
	body := fmt.Sprintf(
		"AlertPolicy %s/%s fired.\n\nCondition: %s (%s)\nValue: %s\nMessage: %s\nFired at: %s\n",
		payload.Namespace, payload.Policy, payload.Condition, payload.Type, payload.Value, payload.Message, payload.FiredAt,
	)
	if payload.RunID != "" {
		body += fmt.Sprintf("Run: %s\n", payload.RunID)
	}
	if payload.Link != "" {
		body += fmt.Sprintf("Review: %s\n", payload.Link)
	}
	if err := r.smtpSender()(cfg, em.To, subject, body); err != nil {
		return fmt.Errorf("sending alert email to %v: %w", em.To, err)
	}
	log.V(1).Info("alert email sent", "alertpolicy", ap.Name, "condition", cond.Name, "to", em.To)
	return nil
}
