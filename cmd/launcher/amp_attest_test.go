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

// Attested AMP (ADR 0153), both directions: who a registry member admits, and how a caller gets and
// sends its attestation. The admission matrix runs through the real /invoke proxy, so a refusal is
// proven by the agent's code never seeing the request.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// memberProxy is baseCfg's agent ("orchestrator", registry research-team, namespace team-a) behind the
// /invoke proxy, recording the headers each admitted request reached the agent's code with.
type memberProxy struct {
	handler http.Handler
	rec     *tracetest.SpanRecorder
	mu      sync.Mutex
	seen    []http.Header
}

func newMemberProxy(t *testing.T, cfg ampConfig, verifier *runcap.Verifier) *memberProxy {
	t.Helper()
	rec, tp := newTestTracer(t)
	m := &memberProxy{rec: rec}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen = append(m.seen, r.Header.Clone())
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	var guard inboundGuard
	if cfg.RegistryID != "" {
		guard = newAMPGuard(cfg, tp.Tracer(tracerName), verifier, testLoopback)
	}
	m.handler = buildHandler(tp.Tracer(tracerName), propagation.TraceContext{}, u,
		Config{AgentName: cfg.SelfName, AgentNamespace: cfg.Namespace}, guard, nil)
	return m
}

// serve sends req and reports the status and whether the agent's code saw it.
func (m *memberProxy) serve(req *http.Request) (*httptest.ResponseRecorder, bool) {
	m.mu.Lock()
	before := len(m.seen)
	m.mu.Unlock()
	rr := httptest.NewRecorder()
	m.handler.ServeHTTP(rr, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	return rr, len(m.seen) > before
}

func (m *memberProxy) lastSeen() http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[len(m.seen)-1]
}

// admission returns the amp.admission attribute of the last agent.invoke span.
func (m *memberProxy) admission() string {
	for _, span := range slices.Backward(m.rec.Ended()) {
		if span.Name() != agentInvokeSpanName {
			continue
		}
		for _, a := range span.Attributes() {
			if a.Key == attribute.Key("amp.admission") {
				return a.Value.AsString()
			}
		}
		return ""
	}
	return ""
}

func invokeWith(headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// capabilityFor mints the run capability the BFF mints for invoking agent (a "<ns>/<name>"), issued age
// ago with a one-minute life.
func capabilityFor(t *testing.T, agent string, age time.Duration) string {
	t.Helper()
	ampTestKeys(t)
	signer := runcap.NewSigner(ampTestPrivate, runcap.DefaultAudience, func() time.Time { return time.Now().Add(-age) })
	tok, err := signer.Mint(runcap.MintRequest{User: "u-hash", Agent: agent, RunID: "run-1", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMemberAdmission(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)
	cfg := baseCfg()
	cfg.AllowedCallers = []string{"research"}
	env := inboundEnvelopeIn("research-team", "research", "worker")
	att := attestationOf(t, "research", "research-team", "worker", "orchestrator")

	_, otherPriv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	foreignAtt, err := runcap.NewSigner(otherPriv, runcap.DefaultAudience, nil).MintAttestation(runcap.AttestationRequest{
		Namespace: "team-a", Agent: "research", RegistryID: "research-team", Role: "worker",
		Receiver: "orchestrator", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ampTestKeys(t)
	expiredAtt, err := runcap.NewSigner(ampTestPrivate, runcap.DefaultAudience,
		func() time.Time { return time.Now().Add(-time.Hour) }).MintAttestation(runcap.AttestationRequest{
		Namespace: "team-a", Agent: "research", RegistryID: "research-team", Role: "worker",
		Receiver: "orchestrator", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	voucher, err := signer.MintApprovalVoucher("run-1", "tool", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
		code    int
		error   string
		how     string
	}{
		{"an attested call from an allowed caller", map[string]string{
			legacyEnvelopeHeader: env, runcap.AttestationHeaderName: att,
		}, http.StatusOK, "", admissionAttested},
		{"an attested call under the AMP header name", map[string]string{
			ampEnvelopeHeader: env, runcap.AttestationHeaderName: att,
		}, http.StatusOK, "", admissionAttested},
		{"an attested call from a caller not on allowedCallers", map[string]string{
			legacyEnvelopeHeader:         inboundEnvelopeIn("research-team", "writer", "worker"),
			runcap.AttestationHeaderName: attestationOf(t, "writer", "research-team", "worker", "orchestrator"),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"nothing at all", nil, http.StatusForbidden, errCallerNotAllowed, ""},
		{
			"a forged envelope with no attestation",
			map[string]string{legacyEnvelopeHeader: env},
			http.StatusForbidden, errCallerNotAllowed, "",
		},
		{
			"an attestation with no envelope",
			map[string]string{runcap.AttestationHeaderName: att},
			http.StatusForbidden, errCallerNotAllowed, "",
		},
		{"an attestation of another agent than the envelope's sender", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "intruder", "research-team", "worker", "orchestrator"),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an attestation in another registry than the envelope's", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "research", "other-team", "worker", "orchestrator"),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an attestation with another role than the envelope's", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "research", "research-team", "orchestrator", "orchestrator"),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an attestation made for another receiver", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "research", "research-team", "worker", "writer"),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an attested caller in another namespace with the same registry id", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationIn(t, "team-b", "research", "research-team", "worker", "orchestrator"),
		}, http.StatusForbidden, errCrossRegistry, ""},
		{"an attested caller from another registry", map[string]string{
			legacyEnvelopeHeader:         inboundEnvelopeIn("other-team", "research", "worker"),
			runcap.AttestationHeaderName: attestationOf(t, "research", "other-team", "worker", "orchestrator"),
		}, http.StatusForbidden, errCrossRegistry, ""},
		{"an expired attestation", map[string]string{
			legacyEnvelopeHeader: env, runcap.AttestationHeaderName: expiredAtt,
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an attestation signed by another key", map[string]string{
			legacyEnvelopeHeader: env, runcap.AttestationHeaderName: foreignAtt,
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"a run capability presented as an attestation", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: capabilityFor(t, "team-a/research", 0),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an approval voucher presented as an attestation", map[string]string{
			legacyEnvelopeHeader: env, runcap.AttestationHeaderName: voucher,
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{
			"its own async consumer",
			map[string]string{ampLoopbackHeader: testLoopback, legacyEnvelopeHeader: env},
			http.StatusOK, "", admissionLoopback,
		},
		{"its own async consumer delivering a foreign registry's envelope", map[string]string{
			ampLoopbackHeader: testLoopback, legacyEnvelopeHeader: inboundEnvelopeIn("other-team", "research", "worker"),
		}, http.StatusForbidden, errCrossRegistry, ""},
		{
			"a guessed loopback marker",
			map[string]string{ampLoopbackHeader: "guess", legacyEnvelopeHeader: env},
			http.StatusForbidden, errCallerNotAllowed, "",
		},
		{"a run capability minted for it", map[string]string{
			runcap.HeaderName: capabilityFor(t, "team-a/orchestrator", 0),
		}, http.StatusOK, "", admissionCapability},
		{"a run capability minted for another agent", map[string]string{
			runcap.HeaderName: capabilityFor(t, "team-a/research", 0),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"a run capability for its name in another namespace", map[string]string{
			runcap.HeaderName: capabilityFor(t, "team-b/orchestrator", 0),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"an expired run capability", map[string]string{
			runcap.HeaderName: capabilityFor(t, "team-a/orchestrator", time.Hour),
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{"a run capability with an unattested envelope", map[string]string{
			runcap.HeaderName:    capabilityFor(t, "team-a/orchestrator", 0),
			legacyEnvelopeHeader: env,
		}, http.StatusForbidden, errCallerNotAllowed, ""},
		{
			"an attestation presented as a run capability",
			map[string]string{runcap.HeaderName: att},
			http.StatusForbidden, errCallerNotAllowed, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMemberProxy(t, cfg, verifier)
			rr, reached := m.serve(invokeWith(tc.headers))
			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.code, rr.Body.String())
			}
			if reached != (tc.code == http.StatusOK) {
				t.Fatalf("agent's code reached = %v, want %v", reached, tc.code == http.StatusOK)
			}
			if tc.error != "" {
				assertAMPError(t, rr.Body.Bytes(), tc.error)
			}
			if tc.how != "" {
				if got := m.admission(); got != tc.how {
					t.Errorf("amp.admission = %q, want %q", got, tc.how)
				}
				if h := m.lastSeen(); h.Get(runcap.AttestationHeaderName) != "" || h.Get(ampLoopbackHeader) != "" {
					t.Errorf("the agent's code saw the attestation or the loopback marker: %v", h)
				}
			}
		})
	}
}

func attestationIn(t *testing.T, ns, agent, registry, role, receiver string) string {
	t.Helper()
	signer, _ := ampTestKeys(t)
	tok, err := signer.MintAttestation(runcap.AttestationRequest{
		Namespace: ns, Agent: agent, RegistryID: registry, Role: role, Receiver: receiver, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// The accept posture admits what was admitted before ADR 0153, and nothing a present attestation does
// not vouch for: an invalid one is refused whatever the posture.
func TestAcceptPostureAdmitsTheOldTrafficOnly(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)
	cfg := baseCfg()
	cfg.AcceptUnattested = true
	env := inboundEnvelopeIn("research-team", "research", "worker")

	for _, tc := range []struct {
		name    string
		headers map[string]string
		code    int
		how     string
	}{
		{"a plain invoke", nil, http.StatusOK, admissionUnattested},
		{
			"an unattested same-registry envelope",
			map[string]string{legacyEnvelopeHeader: env},
			http.StatusOK, admissionUnattested,
		},
		{"an unattested cross-registry envelope", map[string]string{
			legacyEnvelopeHeader: inboundEnvelopeIn("other-team", "research", "worker"),
		}, http.StatusForbidden, ""},
		{"an attestation that does not match the envelope", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "intruder", "research-team", "worker", "orchestrator"),
		}, http.StatusForbidden, ""},
		{"a valid attestation", map[string]string{
			legacyEnvelopeHeader:         env,
			runcap.AttestationHeaderName: attestationOf(t, "research", "research-team", "worker", "orchestrator"),
		}, http.StatusOK, admissionAttested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMemberProxy(t, cfg, verifier)
			rr, reached := m.serve(invokeWith(tc.headers))
			if rr.Code != tc.code || reached != (tc.code == http.StatusOK) {
				t.Fatalf("status = %d reached = %v, want %d; body=%s", rr.Code, reached, tc.code, rr.Body.String())
			}
			if tc.how != "" && m.admission() != tc.how {
				t.Errorf("amp.admission = %q, want %q", m.admission(), tc.how)
			}
		})
	}
}

// A member without the platform key can verify nothing, so it admits only its own consumer.
func TestAMemberWithoutTheKeyAdmitsOnlyItsConsumer(t *testing.T) {
	t.Parallel()
	m := newMemberProxy(t, baseCfg(), nil)
	env := inboundEnvelopeIn("research-team", "research", "worker")

	if rr, _ := m.serve(invokeWith(map[string]string{
		legacyEnvelopeHeader:         env,
		runcap.AttestationHeaderName: attestationOf(t, "research", "research-team", "worker", "orchestrator"),
	})); rr.Code != http.StatusForbidden {
		t.Errorf("attested call without a key: %d, want 403", rr.Code)
	}
	if rr, _ := m.serve(invokeWith(map[string]string{
		runcap.HeaderName: capabilityFor(t, "team-a/orchestrator", 0),
	})); rr.Code != http.StatusForbidden {
		t.Errorf("run capability without a key: %d, want 403", rr.Code)
	}
	if rr, reached := m.serve(invokeWith(map[string]string{
		ampLoopbackHeader: testLoopback, legacyEnvelopeHeader: env,
	})); rr.Code != http.StatusOK || !reached {
		t.Errorf("own consumer without a key: %d, want 200", rr.Code)
	}
}

// An agent in no registry is unchanged: any /invoke reaches it. It still never hands an attestation or
// a loopback marker to its code, because a registry member may call it by name and its code could
// otherwise replay the caller's attestation.
func TestANonMemberIsUnchangedButNeverSeesAnAttestation(t *testing.T) {
	t.Parallel()
	cfg := ampConfig{SelfName: "loner", Namespace: "team-a"}
	m := newMemberProxy(t, cfg, nil)

	if rr, reached := m.serve(invokeWith(nil)); rr.Code != http.StatusOK || !reached {
		t.Fatalf("plain invoke of a non-member: %d, want 200", rr.Code)
	}
	rr, reached := m.serve(invokeWith(map[string]string{
		runcap.AttestationHeaderName: "a-callers-attestation", ampLoopbackHeader: "x",
		legacyEnvelopeHeader: inboundEnvelope("research", "worker"),
	}))
	if rr.Code != http.StatusOK || !reached {
		t.Fatalf("enveloped invoke of a non-member: %d, want 200", rr.Code)
	}
	if h := m.lastSeen(); h.Get(runcap.AttestationHeaderName) != "" || h.Get(ampLoopbackHeader) != "" {
		t.Errorf("a non-member's code saw the attestation or loopback marker")
	}
}

// ── caller side ─────────────────────────────────────────────────────────────

// A forwarded hop carries the attestation for its receiver; a hop with none is never sent.
func TestOutboundHopCarriesTheReceiversAttestation(t *testing.T) {
	t.Parallel()
	s, _, _, peerCh := newTestAMPServer(t, baseCfg(), http.StatusOK, `{}`)
	stub := &stubAttestor{}
	s.attest = stub

	rr := callAMP(t, s, context.Background(), "analyst", `{}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := (<-peerCh).attestation; got != "attestation-for-analyst" {
		t.Errorf("peer received attestation %q, want the one for analyst", got)
	}
	if len(stub.receivers) != 1 || stub.receivers[0] != "analyst" {
		t.Errorf("attestation asked for %v, want [analyst]", stub.receivers)
	}

	stub.err = errors.New("the BFF refused (HTTP 403)")
	rr = callAMP(t, s, context.Background(), "analyst", `{}`, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no attestation: status = %d, want 503", rr.Code)
	}
	assertAMPError(t, rr.Body.Bytes(), errAttestationUnavailable)
	select {
	case <-peerCh:
		t.Error("a hop was sent without an attestation")
	default:
	}
}

// A chained hop claims this agent's own registry, whatever the envelope its code echoed says: the callee
// checks the claim against this agent's attestation, so a hop out of another registry is refused there.
func TestChainedHopKeepsItsOwnRegistry(t *testing.T) {
	t.Parallel()
	s, _, _, peerCh := newTestAMPServer(t, baseCfg(), http.StatusOK, `{}`)
	incoming, err := json.Marshal(envelope{
		RegistryID: "someone-elses-registry", SenderAgentID: "upstream", Role: "orchestrator",
		Depth: 1, Path: []string{"upstream"}, BudgetRemaining: 5, ConversationID: "c",
	})
	if err != nil {
		t.Fatal(err)
	}
	callAMP(t, s, context.Background(), "analyst", `{}`, map[string]string{ampEnvelopeHeader: string(incoming)})
	env := (<-peerCh).envelope
	if env.RegistryID != "research-team" || env.SenderAgentID != "orchestrator" || env.Role != "orchestrator" {
		t.Errorf("chained hop claims %s/%s/%s, want this agent's research-team/orchestrator/orchestrator",
			env.RegistryID, env.SenderAgentID, env.Role)
	}
	if env.Depth != 2 || env.ConversationID != "c" {
		t.Errorf("chained hop lost its conversation: depth %d conversation %q", env.Depth, env.ConversationID)
	}
}

// fakeBFF serves the attestation edge: it requires the pod token and mints for the receiver asked for.
func fakeBFF(t *testing.T, agent string) (*httptest.Server, *int) {
	t.Helper()
	signer, _ := ampTestKeys(t)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.URL.Path != ampAttestationPath || r.Header.Get("Authorization") != "Bearer pod-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Receiver string `json:"receiver"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Receiver == "" {
			http.Error(w, "no receiver", http.StatusBadRequest)
			return
		}
		tok, err := signer.MintAttestation(runcap.AttestationRequest{
			Namespace: "team-a", Agent: agent, RegistryID: "research-team", Role: "worker",
			Receiver: req.Receiver, TTL: 5 * time.Minute,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"attestation": tok, "expiresIn": 300})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestAttestationSourceCachesPerReceiverUntilRefresh(t *testing.T) {
	t.Parallel()
	bff, calls := fakeBFF(t, "research")
	src := newAttestationSource(bff.URL+"/", func() string { return "pod-token" })
	now := time.Now()
	src.now = func() time.Time { return now }
	_, verifier := ampTestKeys(t)

	first, err := src.get(context.Background(), "Orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	att, err := verifier.VerifyAttestation(first)
	if err != nil {
		t.Fatal(err)
	}
	if att.Receiver != "orchestrator" {
		t.Errorf("receiver %q, want the lower-cased agent name", att.Receiver)
	}
	if again, _ := src.get(context.Background(), "orchestrator"); again != first || *calls != 1 {
		t.Errorf("a fresh attestation was refetched (%d calls)", *calls)
	}
	if _, err := src.get(context.Background(), "writer"); err != nil || *calls != 2 {
		t.Errorf("another receiver needs its own attestation (%d calls, err %v)", *calls, err)
	}

	now = now.Add(5*time.Minute - ampAttestationRefreshMargin)
	if _, err := src.get(context.Background(), "orchestrator"); err != nil || *calls != 3 {
		t.Errorf("an attestation inside the refresh margin was not replaced (%d calls, err %v)", *calls, err)
	}
}

func TestAttestationSourceFailsClosed(t *testing.T) {
	t.Parallel()
	bff, _ := fakeBFF(t, "research")
	for name, src := range map[string]*attestationSource{
		"no BFF":       newAttestationSource("", func() string { return "pod-token" }),
		"no pod token": newAttestationSource(bff.URL, func() string { return "" }),
		"refused":      newAttestationSource(bff.URL, func() string { return "stolen" }),
		"unreachable":  newAttestationSource("http://127.0.0.1:1", func() string { return "pod-token" }),
	} {
		if tok, err := src.get(context.Background(), "orchestrator"); err == nil || tok != "" {
			t.Errorf("%s: got %q, %v; want an error and no attestation", name, tok, err)
		}
	}
}

// Two launchers end to end: the caller gets its attestation from the BFF edge and the callee admits the
// hop, or refuses it when the caller is not on its allowedCallers.
func TestAttestedHopEndToEnd(t *testing.T) {
	t.Parallel()
	_, verifier := ampTestKeys(t)
	callee := baseCfg() // orchestrator in research-team
	callee.AllowedCallers = []string{"research"}
	member := newMemberProxy(t, callee, verifier)
	peer := httptest.NewServer(member.handler)
	t.Cleanup(peer.Close)

	call := func(callerName string) *httptest.ResponseRecorder {
		bff, _ := fakeBFF(t, callerName)
		cfg := baseCfg()
		cfg.SelfName, cfg.Role = callerName, "worker"
		_, tp := newTestTracer(t)
		s := newAMPServer(cfg, tp.Tracer(tracerName), propagation.TraceContext{}, nil)
		s.attest = newAttestationSource(bff.URL, func() string { return "pod-token" })
		s.resolveHost = func(string) string { return peer.URL }
		return callAMP(t, s, context.Background(), "orchestrator", `{"q":1}`, nil)
	}

	if rr := call("research"); rr.Code != http.StatusOK {
		t.Fatalf("an allowed caller's attested hop: %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := member.admission(); got != admissionAttested {
		t.Errorf("amp.admission = %q, want attested", got)
	}
	rr := call("writer")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a caller not on allowedCallers: %d, want 403", rr.Code)
	}
	assertAMPError(t, rr.Body.Bytes(), errCallerNotAllowed)
}

func TestRequireAttestationParsing(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": true, enabledTrue: true, "yes": true, "false": false, " FALSE ": false} {
		if got := requireAttestation(raw); got != want {
			t.Errorf("requireAttestation(%q) = %v, want %v", raw, got, want)
		}
	}
	cfg, err := loadConfig(envMap(map[string]string{
		"AGENT_ENTRYPOINT": "/bin/agent", "AGENT_REGISTRY_ID": "r", envAMPRequireAttestation: "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AMP.AcceptUnattested {
		t.Error("AMP_REQUIRE_ATTESTATION=false did not select the accept posture")
	}
	cfg, err = loadConfig(envMap(map[string]string{"AGENT_ENTRYPOINT": "/bin/agent", "AGENT_REGISTRY_ID": "r"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AMP.AcceptUnattested {
		t.Error("an unset AMP_REQUIRE_ATTESTATION must require")
	}
}

func TestCapabilityVerifierDefaultsTheAudience(t *testing.T) {
	t.Parallel()
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := runcap.NewSigner(priv, runcap.DefaultAudience, nil).Mint(runcap.MintRequest{
		User: "u", Agent: "team-a/orchestrator", RunID: "r", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := capabilityVerifierFromEnv(envMap(map[string]string{
		"MCP_CAPABILITY_PUBLIC_KEY": runcap.EncodePublicKey(pub),
	}))
	if err != nil || v == nil {
		t.Fatalf("verifier: %v", err)
	}
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("a capability for the default audience was refused without MCP_CAPABILITY_AUDIENCE: %v", err)
	}
	if v, err := capabilityVerifierFromEnv(envMap(nil)); v != nil || err != nil {
		t.Errorf("no key: got %v, %v; want nil, nil", v, err)
	}
	bad := envMap(map[string]string{"MCP_CAPABILITY_PUBLIC_KEY": "bm9wZQ=="})
	if _, err := capabilityVerifierFromEnv(bad); err == nil {
		t.Error("a malformed key must be reported")
	}
}

func TestAsyncInvokerMarksItsLoopbackCall(t *testing.T) {
	t.Parallel()
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	invoke := newProxyInvoker(port, testLoopback, srv.Client())
	if _, err := invoke(context.Background(), envelope{MessageID: "m"}); err != nil {
		t.Fatal(err)
	}
	if h := <-got; h.Get(ampLoopbackHeader) != testLoopback {
		t.Errorf("loopback marker = %q, want the process secret", h.Get(ampLoopbackHeader))
	}
}
