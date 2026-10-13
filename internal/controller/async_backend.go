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

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	// asyncBackendJetStream is the one async backend whose queue the platform can read. With no backend
	// configured, async hops ride Knative Eventing, whose broker exposes no queue depth.
	asyncBackendJetStream = "jetstream"

	// natsMonitoringPort is the NATS server's default HTTP monitoring port, which the bundled broker
	// serves on and KEDA's nats-jetstream scaler reads.
	natsMonitoringPort = "8222"

	// natsGlobalAccount is the account a NATS server with no accounts configured, such as the bundled
	// broker, puts every client in.
	natsGlobalAccount = "$G"
)

// AsyncBackend is the install's async backend, read from the settings the BFF starts it from: the chart
// sets ASYNC_BACKEND and NATS_URL on the controller from the same values. The controller never connects
// to the broker. It names, in a queue-depth ScaledObject, where KEDA reads an agent's queue.
type AsyncBackend struct {
	// Kind is ASYNC_BACKEND as the BFF reads it: "jetstream", or "" when async hops ride Knative Eventing.
	Kind string
	// NATSURL is NATS_URL, the broker the BFF publishes to and consumes from.
	NATSURL string
	// Account is NATS_ACCOUNT, the account the stream lives in. Empty means $G.
	Account string
	// CredentialsFile is NATS_CREDENTIALS_FILE, the BFF's credentials for the broker. The controller
	// reads only whether it is set: a client that authenticates with one is in a named account.
	CredentialsFile string
}

// AsyncBackendFromEnv reads the backend from ASYNC_BACKEND and NATS_URL, normalised as the BFF does.
func AsyncBackendFromEnv(getenv func(string) string) AsyncBackend {
	return AsyncBackend{
		Kind:            strings.ToLower(strings.TrimSpace(getenv("ASYNC_BACKEND"))),
		NATSURL:         strings.TrimSpace(getenv("NATS_URL")),
		Account:         strings.TrimSpace(getenv("NATS_ACCOUNT")),
		CredentialsFile: strings.TrimSpace(getenv("NATS_CREDENTIALS_FILE")),
	}
}

// queueDepthUnavailable says why this backend has no queue a queue-depth policy can scale on, as a
// condition reason and a message naming the setting that fixes it. Both are empty when the JetStream
// backend is configured completely.
func (b AsyncBackend) queueDepthUnavailable() (reason, why string) {
	switch {
	case b.Kind == "":
		return reasonQueueDepthNeedsJetStream, "this install has no JetStream async backend " +
			"(bff.asyncBackend is unset), so async hops ride Knative Eventing, whose broker exposes no queue " +
			"depth. Set bff.asyncBackend=jetstream and bff.natsURL to scale on queue depth"
	case b.Kind != asyncBackendJetStream:
		return reasonQueueDepthNeedsJetStream, fmt.Sprintf("the async backend %q is not one the platform runs, "+
			"so there is no queue to read; set bff.asyncBackend=jetstream and bff.natsURL to scale on queue "+
			"depth", b.Kind)
	case b.NATSURL == "":
		return reasonQueueDepthNeedsJetStream, "bff.asyncBackend is jetstream but bff.natsURL is unset, so " +
			"the BFF starts no async backend and no queue exists; set bff.natsURL"
	case b.CredentialsFile != "" && b.Account == "":
		return reasonQueueDepthNeedsNATSAccount, "the BFF authenticates to NATS with bff.natsCredentialsFile, " +
			"so the stream lives in a named NATS account, and KEDA would look for it in $G and find nothing. " +
			"Set bff.natsAccount to that account"
	}
	if _, err := b.natsMonitoringEndpoint(); err != nil {
		return reasonQueueDepthNeedsJetStream, err.Error()
	}
	return "", ""
}

// natsAccount is the account KEDA reads the stream's consumers under.
func (b AsyncBackend) natsAccount() string {
	if b.Account != "" {
		return b.Account
	}
	return natsGlobalAccount
}

// natsMonitoringEndpoint is the host:port KEDA reads JetStream consumer state from: the host of the first
// server in NATS_URL, on the monitoring port. Credentials in the URL are never carried into it.
func (b AsyncBackend) natsMonitoringEndpoint() (string, error) {
	first, _, _ := strings.Cut(b.NATSURL, ",")
	first = strings.TrimSpace(first)
	if !strings.Contains(first, "://") {
		first = "nats://" + first
	}
	u, err := url.Parse(first)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("bff.natsURL %q names no NATS host, so there is no monitoring endpoint for KEDA "+
			"to read the queue from; set it to nats://<host>:4222", redactedNATSURL(b.NATSURL))
	}
	return net.JoinHostPort(u.Hostname(), natsMonitoringPort), nil
}

// redactedNATSURL drops any userinfo, so a password in NATS_URL never reaches a status message.
func redactedNATSURL(raw string) string {
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		if scheme := strings.Index(raw, "://"); scheme >= 0 && scheme < at {
			return raw[:scheme+3] + "<redacted>" + raw[at:]
		}
		return "<redacted>" + raw[at:]
	}
	return raw
}
