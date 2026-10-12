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

package main

// Async deliveries are admitted as attested calls are (ADR 0153): the consume path is reached by
// anything that can post to a member, so a CloudEvent is admitted only with the platform's attestation
// of its sender, made for this agent, vouching for the envelope.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// deliveryConsumer is "worker-agent" in registry research-team, namespace team-a, consuming with a
// counting invoker and a counting seen-set.
func deliveryConsumer(t *testing.T, accept bool, verifier *runcap.Verifier) (*asyncConsumer, *int32, *fakeSeenSet) {
	t.Helper()
	var invoked int32
	seen := newFakeSeenSet()
	cfg := ampConfig{
		RegistryID: "research-team", SelfName: "worker-agent", Role: roleWorker, Namespace: "team-a",
		AcceptUnattested: accept,
	}
	c := &asyncConsumer{
		cfg:    asyncConfig{SelfName: "worker-agent"},
		seen:   seen,
		tracer: noop.NewTracerProvider().Tracer("test"),
		invoke: func(context.Context, envelope) (int, error) {
			atomic.AddInt32(&invoked, 1)
			return http.StatusOK, nil
		},
		guard: newAMPGuard(cfg, noop.NewTracerProvider().Tracer("test"), verifier, testLoopback),
	}
	return c, &invoked, seen
}

// deliveryAttestation is what the dispatcher mints for a hop from orchestrator (the sampleEnvelope's
// sender) to receiver.
func deliveryAttestation(t *testing.T, ns, sender, registry, role, receiver string) string {
	t.Helper()
	return attestationIn(t, ns, sender, registry, role, receiver)
}

func deliveryWith(t *testing.T, msgID, attestation string) *http.Request {
	t.Helper()
	req := cloudEventRequest(t, sampleEnvelope(msgID, "worker-agent"))
	if attestation != "" {
		req.Header.Set(runcap.CloudEventAttestationHeader, attestation)
	}
	return req
}

func TestADeliveryIsAdmittedOnlyWithItsSendersAttestation(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)
	_, otherPriv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	forged, err := runcap.NewSigner(otherPriv, runcap.DefaultAudience, nil).MintAttestation(runcap.AttestationRequest{
		Namespace: "team-a", Agent: "orchestrator", RegistryID: "research-team", Role: roleOrchestrator,
		Receiver: "worker-agent", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	capability := capabilityFor(t, "team-a/orchestrator", 0)

	for _, tc := range []struct {
		name        string
		attestation string
		code        int
		error       string
	}{
		{"its sender's attestation", deliveryAttestation(t, "team-a", "orchestrator", "research-team",
			roleOrchestrator, "worker-agent"), http.StatusNoContent, ""},
		{"no attestation", "", http.StatusForbidden, errCallerNotAllowed},
		{"an attestation of another sender", deliveryAttestation(t, "team-a", "intruder", "research-team",
			roleOrchestrator, "worker-agent"), http.StatusForbidden, errCallerNotAllowed},
		{"an attestation with another role", deliveryAttestation(t, "team-a", "orchestrator", "research-team",
			roleWorker, "worker-agent"), http.StatusForbidden, errCallerNotAllowed},
		{"an attestation made for another receiver", deliveryAttestation(t, "team-a", "orchestrator",
			"research-team", roleOrchestrator, "other-agent"), http.StatusForbidden, errCallerNotAllowed},
		{"an attested sender in another namespace", deliveryAttestation(t, "team-b", "orchestrator",
			"research-team", roleOrchestrator, "worker-agent"), http.StatusForbidden, errCrossRegistry},
		{"an attested sender in another registry", deliveryAttestation(t, "team-a", "orchestrator",
			"other-team", roleOrchestrator, "worker-agent"), http.StatusForbidden, errCallerNotAllowed},
		{"an attestation signed by another key", forged, http.StatusForbidden, errCallerNotAllowed},
		{"a run capability in its place", capability, http.StatusForbidden, errCallerNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, invoked, seen := deliveryConsumer(t, false, verifier)
			rr := httptest.NewRecorder()
			c.consume(rr, deliveryWith(t, "msg-"+tc.name, tc.attestation))
			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.code, rr.Body.String())
			}
			admitted := tc.code == http.StatusNoContent
			if got := atomic.LoadInt32(invoked) == 1; got != admitted {
				t.Errorf("agent invoked = %v, want %v", got, admitted)
			}
			if tc.error != "" {
				assertAMPError(t, rr.Body.Bytes(), tc.error)
				if seen.calls.Load() != 0 {
					t.Error("a refused delivery spent its message id in the seen-set")
				}
			}
		})
	}
}

// The accept posture admits an unattested delivery, as before ADR 0153, but never a bad attestation.
func TestAcceptPostureAdmitsAnUnattestedDeliveryOnly(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)

	c, invoked, _ := deliveryConsumer(t, true, verifier)
	rr := httptest.NewRecorder()
	c.consume(rr, deliveryWith(t, "msg-unattested", ""))
	if rr.Code != http.StatusNoContent || atomic.LoadInt32(invoked) != 1 {
		t.Fatalf("unattested under accept: %d, invoked %d; want 204 and invoked", rr.Code, atomic.LoadInt32(invoked))
	}

	c, invoked, _ = deliveryConsumer(t, true, verifier)
	rr = httptest.NewRecorder()
	c.consume(rr, deliveryWith(t, "msg-bad", deliveryAttestation(t, "team-a", "intruder", "research-team",
		roleOrchestrator, "worker-agent")))
	if rr.Code != http.StatusForbidden || atomic.LoadInt32(invoked) != 0 {
		t.Fatalf("a bad attestation under accept: %d, invoked %d; want 403", rr.Code, atomic.LoadInt32(invoked))
	}
}

// A consumer with no admission policy refuses every delivery rather than admit one unchecked.
func TestAConsumerWithoutAGuardRefuses(t *testing.T) {
	t.Parallel()
	c, invoked, _ := deliveryConsumer(t, true, nil)
	c.guard = nil
	rr := httptest.NewRecorder()
	c.consume(rr, deliveryWith(t, "msg-noguard", ""))
	if rr.Code != http.StatusForbidden || atomic.LoadInt32(invoked) != 0 {
		t.Fatalf("no guard: %d, invoked %d; want 403", rr.Code, atomic.LoadInt32(invoked))
	}
}

// The bypass, end to end: any pod could add Ce-Id and a self-asserted envelope to a member's /invoke and
// reach its code through its own consumer's loopback call. Through the real proxy and the real loopback
// invoker, that delivery is refused, and an attested one still arrives.
func TestACloudEventFromAnOrdinaryPodNeverReachesTheAgent(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := ampConfig{RegistryID: "research-team", SelfName: "worker-agent", Role: roleWorker, Namespace: "team-a"}
	_, tp := newTestTracer(t)
	guard := newAMPGuard(cfg, tp.Tracer(tracerName), verifier, testLoopback)
	consumer := &asyncConsumer{
		cfg: asyncConfig{SelfName: "worker-agent"}, seen: newFakeSeenSet(), tracer: tp.Tracer(tracerName),
		guard: guard,
	}
	handler := buildHandler(tp.Tracer(tracerName), propagation.TraceContext{}, upstreamURL,
		Config{AgentName: "worker-agent", AgentNamespace: "team-a"}, guard, consumer)
	member := httptest.NewServer(handler)
	t.Cleanup(member.Close)
	port := member.Listener.Addr().(*net.TCPAddr).Port
	consumer.invoke = newProxyInvoker(port, testLoopback, member.Client())

	post := func(req *http.Request) int {
		t.Helper()
		req.URL, _ = url.Parse(member.URL + "/invoke")
		req.RequestURI = ""
		resp, err := member.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(deliveryWith(t, "msg-forged", "")); code != http.StatusForbidden || reached.Load() != 0 {
		t.Fatalf("an unattested CloudEvent: %d, reached the agent %d times; want 403 and never", code, reached.Load())
	}
	att := deliveryAttestation(t, "team-a", "orchestrator", "research-team", roleOrchestrator, "worker-agent")
	if code := post(deliveryWith(t, "msg-attested", att)); code != http.StatusNoContent || reached.Load() != 1 {
		t.Fatalf("an attested CloudEvent: %d, reached the agent %d times; want 204 and once", code, reached.Load())
	}
}
