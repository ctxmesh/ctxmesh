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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// The controller reads the backend from the same two settings the BFF starts it from, normalised the same
// way, so the two can never disagree about whether there is a queue.
func TestAsyncBackendFromEnv(t *testing.T) {
	env := map[string]string{
		"ASYNC_BACKEND": "  JetStream ", "NATS_URL": " nats://ctxmesh-nats.team.svc:4222 ",
		"NATS_ACCOUNT": " TEAM ", "NATS_CREDENTIALS_FILE": " /etc/nats/creds ",
	}
	b := AsyncBackendFromEnv(func(k string) string { return env[k] })
	assert.Equal(t, AsyncBackend{
		Kind: "jetstream", NATSURL: "nats://ctxmesh-nats.team.svc:4222", Account: "TEAM", CredentialsFile: "/etc/nats/creds",
	}, b)
	reason, why := b.queueDepthUnavailable()
	assert.Empty(t, reason, "a configured JetStream backend has a queue to read")
	assert.Empty(t, why)
	assert.Equal(t, "TEAM", b.natsAccount())
	assert.Equal(t, "$G", AsyncBackend{Kind: "jetstream", NATSURL: "nats://n:4222"}.natsAccount(),
		"a server with no accounts puts every client in $G")
}

// Each way an install can lack a readable queue is named, with the setting that fixes it.
func TestAsyncBackend_QueueDepthUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		backend AsyncBackend
		reason  string
		says    string
	}{
		"no backend (Knative Eventing)": {AsyncBackend{}, reasonQueueDepthNeedsJetStream, "bff.asyncBackend is unset"},
		"unknown backend": {
			AsyncBackend{Kind: "kafka", NATSURL: "nats://n:4222"}, reasonQueueDepthNeedsJetStream, `"kafka"`,
		},
		"jetstream without a URL": {AsyncBackend{Kind: "jetstream"}, reasonQueueDepthNeedsJetStream, "bff.natsURL is unset"},
		"a URL with no host": {
			AsyncBackend{Kind: "jetstream", NATSURL: "nats://:4222"}, reasonQueueDepthNeedsJetStream, "names no NATS host",
		},
		"credentials and no account": {
			AsyncBackend{Kind: "jetstream", NATSURL: "nats://n:4222", CredentialsFile: "/etc/nats/creds"},
			reasonQueueDepthNeedsNATSAccount, "Set bff.natsAccount",
		},
	} {
		t.Run(name, func(t *testing.T) {
			reason, why := tc.backend.queueDepthUnavailable()
			assert.Equal(t, tc.reason, reason)
			assert.Contains(t, why, tc.says)
			assert.Contains(t, why, "bff.", "the message names the chart value that fixes it")
		})
	}
}

// KEDA reads the monitoring port of the server the BFF publishes to. Credentials in NATS_URL never reach
// the ScaledObject or a status message.
func TestAsyncBackend_NATSMonitoringEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		"nats://ctxmesh-nats.ctxmesh.svc:4222":               "ctxmesh-nats.ctxmesh.svc:8222",
		"nats://user:secret@nats.broker.svc:4222":            "nats.broker.svc:8222",
		"nats://a.broker.svc:4222, nats://b.broker.svc:4222": "a.broker.svc:8222",
		"tls://nats.broker.svc":                              "nats.broker.svc:8222",
		"nats.broker.svc:4222":                               "nats.broker.svc:8222",
		"nats://[fd00::1]:4222":                              "[fd00::1]:8222",
	} {
		got, err := AsyncBackend{Kind: "jetstream", NATSURL: raw}.natsMonitoringEndpoint()
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}

	_, why := AsyncBackend{Kind: "jetstream", NATSURL: "nats://user:secret@:4222"}.queueDepthUnavailable()
	assert.NotContains(t, why, "secret", "a password in NATS_URL never reaches a status message")
}

// The trigger reads exactly the consumer the dispatcher delivers the agent's hops from, in the stream the
// BFF creates, sized at ten waiting hops per replica and woken by one.
func TestBuildJetStreamTrigger(t *testing.T) {
	policy := &agentsv1beta1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-scale", Namespace: "team-ns"},
		Spec: agentsv1beta1.AgentScalingPolicySpec{
			AgentRef: "worker",
			Trigger:  triggerQueueDepth,
			// Ignored: the queue is named by the agent.
			QueueRef: &agentsv1beta1.QueueRef{Name: "some-broker", Namespace: "elsewhere"},
		},
	}
	trigger := buildJetStreamTrigger(policy, "ctxmesh-nats.ctxmesh.svc:8222", "$G")
	assert.Equal(t, "nats-jetstream", trigger.Type)
	assert.Equal(t, map[string]string{
		"natsServerMonitoringEndpoint": "ctxmesh-nats.ctxmesh.svc:8222",
		"account":                      "$G",
		"stream":                       "CTXMESH_A2A",
		"consumer":                     "agent_team-ns_worker",
		"lagThreshold":                 "10",
		"activationLagThreshold":       "0",
	}, trigger.Metadata)
}

// A fixed replica count is kept within a policy's bounds, with a floor of one: an agent held at zero with
// no queue metric would never be woken.
func TestClampReplicas(t *testing.T) {
	assert.Equal(t, int32(1), clampReplicas(1, max(int32(0), 1), 5), "min 0 still runs one")
	assert.Equal(t, int32(3), clampReplicas(1, 3, 5), "raised to min")
	assert.Equal(t, int32(4), clampReplicas(7, 1, 4), "lowered to max")
	assert.Equal(t, int32(2), clampReplicas(2, 1, 5), "inside the bounds, unchanged")
}
