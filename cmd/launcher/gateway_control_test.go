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

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ctxmesh/agentry/internal/gateway/budget"
	"github.com/ctxmesh/agentry/internal/runcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeControlStore is a stand-in for the pod-authed /control proxy client: it returns a fixed verb, or a
// fixed error (to exercise the gateway's fail-open on a control-plane blip).
type fakeControlStore struct {
	verb string
	err  error
}

func (f fakeControlStore) Control(context.Context, string) (string, error) {
	return f.verb, f.err
}

// newControlGateway builds a gateway wired with a fake control store + a real capability verifier, and a
// mock upstream (so a forwarded call is observable). Returns the gateway, the signer for minting caps, and
// the mock so a test can assert the provider was (not) called.
func newControlGateway(t *testing.T, control controlStore) (*gatewayProxy, *runcap.Signer, *mockGateway) {
	t.Helper()
	mock := newMockGateway(t, 10)
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)

	u, err := url.Parse(mock.server.URL)
	require.NoError(t, err)

	gp := &gatewayProxy{
		cfg:         gatewayConfig{AgentName: "ag"},
		upstream:    u,
		enforcer:    budget.NewEnforcer(),
		estimator:   budget.NewEstimator(),
		client:      &http.Client{Timeout: gatewayRequestTimeout},
		tracer:      noop.NewTracerProvider().Tracer("test"),
		control:     control,
		capVerifier: runcap.NewVerifier(pub, testCapAudience, nil),
		logf:        func(string, ...any) {},
	}
	signer := runcap.NewSigner(priv, testCapAudience, nil)
	return gp, signer, mock
}

// mintRunCap mints a valid capability scoped to runID (the launcher trusts the run id ONLY from a verified
// capability, never a raw header).
func mintRunCap(t *testing.T, signer *runcap.Signer, runID string) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{User: "u", Agent: "ag", RunID: runID, TTL: 5 * time.Minute})
	require.NoError(t, err)
	return tok
}

// invokeWithCapTok drives a chat/completions request carrying the capability header.
func invokeWithCapTok(gp *gatewayProxy, capTok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if capTok != "" {
		req.Header.Set(runcap.HeaderName, capTok)
	}
	rr := httptest.NewRecorder()
	gp.handler().ServeHTTP(rr, req)
	return rr
}

// extractRunID resolves the run id from a VERIFIED capability, and fails open ("") for a missing/forged
// capability so it can never inject a run id to cancel someone else's run.
func TestExtractRunID(t *testing.T) {
	gp, signer, _ := newControlGateway(t, fakeControlStore{})

	t.Run("valid capability yields its run id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		req.Header.Set(runcap.HeaderName, mintRunCap(t, signer, "run-42"))
		assert.Equal(t, "run-42", gp.extractRunID(req))
	})

	t.Run("absent capability → \"\" (fail-open, no cancel enforcement)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		assert.Equal(t, "", gp.extractRunID(req))
	})

	t.Run("forged capability → \"\" (a bad signature cannot inject a run id)", func(t *testing.T) {
		_, evilPriv, err := runcap.GenerateKeyPair()
		require.NoError(t, err)
		forged := runcap.NewSigner(evilPriv, testCapAudience, nil)
		forgedTok, err := forged.Mint(runcap.MintRequest{User: "u", Agent: "ag", RunID: "victim-run", TTL: time.Minute})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		req.Header.Set(runcap.HeaderName, forgedTok)
		assert.Equal(t, "", gp.extractRunID(req), "a forged capability must not yield a run id")
	})

	t.Run("no control store → \"\" (real-kill off, no reach)", func(t *testing.T) {
		gpNoCtl, sgn, _ := newControlGateway(t, nil)
		gpNoCtl.control = nil
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		req.Header.Set(runcap.HeaderName, mintRunCap(t, sgn, "run-1"))
		assert.Equal(t, "", gpNoCtl.extractRunID(req))
	})
}

// THE REAL KILL: a cancelled run's control marker makes the PRE-CALL check REFUSE the model call with a
// typed 409 run_cancelled — and the provider is NEVER reached (the circuit-break).
func TestGatewayControl_CancelRefusesPreCallAndDoesNotCallProvider(t *testing.T) {
	gp, signer, mock := newControlGateway(t, fakeControlStore{verb: "cancel"})
	capTok := mintRunCap(t, signer, "run-cancelled")

	rr := invokeWithCapTok(gp, capTok)
	assert.Equal(t, http.StatusConflict, rr.Code, "a cancelled run's model call is refused with 409")
	assert.Contains(t, rr.Body.String(), errRunCancelled, "typed run_cancelled body")
	assert.Equal(t, int64(0), mock.calls.Load(), "the provider must NOT be called on a cancelled run")
}

// FAIL-OPEN: a control-plane blip (the control store errors) must NOT kill a live call — the request
// forwards to the provider as normal.
func TestGatewayControl_ControlErrorFailsOpen(t *testing.T) {
	gp, signer, mock := newControlGateway(t, fakeControlStore{err: errors.New("proxy unreachable")})
	capTok := mintRunCap(t, signer, "run-live")

	rr := invokeWithCapTok(gp, capTok)
	assert.Equal(t, http.StatusOK, rr.Code, "a control-plane error must fail OPEN, not kill the call")
	assert.Equal(t, int64(1), mock.calls.Load(), "the call proceeds to the provider on a control blip")
}

// No cancel marker (empty verb) → the call proceeds normally to the provider.
func TestGatewayControl_NoMarkerProceeds(t *testing.T) {
	gp, signer, mock := newControlGateway(t, fakeControlStore{verb: ""})
	capTok := mintRunCap(t, signer, "run-ok")

	rr := invokeWithCapTok(gp, capTok)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int64(1), mock.calls.Load())
}

// An unknown/future verb (not "cancel") is IGNORED — the marker is a verb channel, so a newer BFF verb
// never trips an older launcher's real-kill.
func TestGatewayControl_UnknownVerbIgnored(t *testing.T) {
	gp, signer, mock := newControlGateway(t, fakeControlStore{verb: "nudge"})
	capTok := mintRunCap(t, signer, "run-nudge")

	rr := invokeWithCapTok(gp, capTok)
	assert.Equal(t, http.StatusOK, rr.Code, "only \"cancel\" trips the real kill; an unknown verb is ignored")
	assert.Equal(t, int64(1), mock.calls.Load())
}

// A missing capability (no trusted run id) → cancel enforcement is skipped entirely; even with a cancel
// marker present the call proceeds (there is no trusted run id to key the check on — fail-open).
func TestGatewayControl_NoCapabilitySkipsEnforcement(t *testing.T) {
	gp, _, mock := newControlGateway(t, fakeControlStore{verb: "cancel"})

	rr := invokeWithCapTok(gp, "") // no capability header
	assert.Equal(t, http.StatusOK, rr.Code, "no trusted run id ⇒ cancel enforcement skipped (fail-open)")
	assert.Equal(t, int64(1), mock.calls.Load())
}
