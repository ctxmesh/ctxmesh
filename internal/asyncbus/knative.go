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

package asyncbus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Knative Eventing backend (M141, ADR 0121) — the path that already existed, brought behind the seam
// so it is one CHOICE among backends rather than the only way to be asynchronous.
//
// It is publish-only by construction. With Knative the consumer is a Trigger, which pushes the CloudEvent
// over HTTP to the callee's ksvc from outside this process, so there is nothing in-process to subscribe
// with — Subscribe says so with ErrPushDelivered instead of pretending to be a puller. Preserving that
// asymmetry in the type is the honest move: a caller that needs to pull finds out at wiring time, not by
// silently receiving nothing.

const knativePublishTimeout = 10 * time.Second

// KnativeBus publishes CloudEvents to a Knative Broker's addressable URL.
type KnativeBus struct {
	brokerURL string
	client    *http.Client
}

// NewKnative returns the Knative-Eventing Publisher for a Broker URL. A nil client gets a bounded default
// — an unbounded publish would stall the AMP hop that triggered it.
func NewKnative(brokerURL string, client *http.Client) (*KnativeBus, error) {
	if strings.TrimSpace(brokerURL) == "" {
		return nil, fmt.Errorf("asyncbus: a Knative Broker URL is required")
	}
	if client == nil {
		client = &http.Client{Timeout: knativePublishTimeout}
	}
	return &KnativeBus{brokerURL: strings.TrimRight(brokerURL, "/"), client: client}, nil
}

// Publish POSTs the encoded CloudEvent to the Broker. The Broker persists it before answering 2xx, which
// is what lets this satisfy the seam's durable-accept contract.
func (b *KnativeBus) Publish(ctx context.Context, msg Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.brokerURL, bytes.NewReader(msg.Data))
	if err != nil {
		return fmt.Errorf("asyncbus: building the broker request: %w", err)
	}
	for k, v := range msg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("asyncbus: publishing to broker %q: %w", b.brokerURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("asyncbus: broker rejected %q: status %d", msg.ID, resp.StatusCode)
	}
	return nil
}

// Subscribe reports that this backend delivers by push — see ErrPushDelivered.
func (b *KnativeBus) Subscribe(context.Context, string, string, Handler) error {
	return ErrPushDelivered
}

// Close is a no-op: an HTTP publisher holds no long-lived connection of its own.
func (b *KnativeBus) Close() error { return nil }
