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

package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/run"
	"github.com/ctxmesh/agentry/internal/runcap"
)

// ctxCapturingApprovalAdapter models a HITL agent that pauses on a require-approval TOOL: the first
// invoke pauses with key "tool:send_email"; the re-invoke (which carries the granted approvals) both
// captures its ctx (so the test can inspect the minted voucher + capability) and returns success.
type ctxCapturingApprovalAdapter struct {
	resumeCtx context.Context
}

func (a *ctxCapturingApprovalAdapter) Invoke(ctx context.Context, _ string, body []byte) ([]byte, string, error) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	if _, approved := m["approvals"]; approved {
		a.resumeCtx = ctx
		return []byte(`{"output":"email sent","consent_required":[]}`), "tr-appr", nil
	}
	return []byte(`{"output":"awaiting approval",` +
		`"approval_required":{"key":"tool:send_email","summary":"Run tool send_email?"},` +
		`"consent_required":[]}`), "tr-appr", nil
}

// TestResumeApprovalMintsVoucher proves the BFF mint-on-approval hook (ADR 0074 §3, m82.4): when a
// require-approval TOOL is GRANTED on resume, the BFF mints a signed voucher bound to {rn.ID, tool}
// and threads it onto the resumed run — and the run capability is pinned to rn.ID so the voucher's run
// matches the runcap's run (the sidecar's binding check). The voucher verifies under the platform key.
func TestResumeApprovalMintsVoucher(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)

	agent := readyAgent("mailer", "prod", "http://mailer.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(agent).
		WithInterceptorFuncs(ssrInterceptor("alice@example.com", nil)).
		Build()
	inv := &ctxCapturingApprovalAdapter{}
	s := NewServer(Options{
		CallerClients:               newFakeFactory(c),
		Scheme:                      testScheme(t),
		Auth:                        AllowAll{},
		Adapters:                    Adapters{Invoke: inv},
		Version:                     "test",
		MCPEnabled:                  true,
		MCPCapabilityPrivateSeedB64: runcap.EncodePrivateSeed(priv),
		MCPCapabilityAudience:       "test-plane",
		Log:                         logr.Discard(),
	})

	created := createRun(t, s, InvokeRequest{Agent: "mailer", Namespace: "prod", Input: json.RawMessage(`{"input":"email the customer"}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})
	require.Equal(t, run.StatusRequiresAction, got.Status)
	require.NotNil(t, got.RequiresAction)
	require.Equal(t, "tool:send_email", got.RequiresAction.Key)

	// Approve → the run re-invokes; the adapter captures its ctx.
	require.Equal(t, http.StatusAccepted, resumeRun(t, s, created.ID, `{"decision":"approve"}`).Code)
	final := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	require.Equal(t, run.StatusSucceeded, final.Status)
	require.NotNil(t, inv.resumeCtx, "the resume must have re-invoked the agent")

	// The run capability minted on resume is pinned to the run's id.
	capTok := runCapabilityFromContext(inv.resumeCtx)
	require.NotEmpty(t, capTok)
	verifier := runcap.NewVerifier(pub, "test-plane", nil)
	capb, err := verifier.Verify(capTok)
	require.NoError(t, err)
	assert.Equal(t, created.ID, capb.RunID, "the resumed run's capability is pinned to rn.ID (so the voucher's run matches)")

	// The approval voucher is minted, threaded, and verifies for {rn.ID, send_email} under the SAME key.
	voucher := approvalVoucherFromContext(inv.resumeCtx)
	require.NotEmpty(t, voucher, "a granted require-approval tool mints a voucher on resume")
	v, err := verifier.VerifyVoucher(voucher, capb.RunID, "send_email")
	require.NoError(t, err, "the minted voucher verifies for the runcap's run + the granted tool")
	assert.Equal(t, created.ID, v.RunID)
	assert.Equal(t, "send_email", v.ToolName)

	// And a runcap must NOT be usable in the voucher's place (the discriminator), even here.
	_, err = verifier.VerifyVoucher(capTok, capb.RunID, "send_email")
	assert.ErrorIs(t, err, runcap.ErrVoucherMalformed, "the run capability is not a voucher")
}

// TestApprovalToolNameParsing covers the approval-key → wire-tool-name extraction.
func TestApprovalToolNameParsing(t *testing.T) {
	name, ok := approvalToolName("tool:send_email")
	assert.True(t, ok)
	assert.Equal(t, "send_email", name)

	_, ok = approvalToolName("send-email") // a non-tool HITL key (no prefix) → no voucher
	assert.False(t, ok)
	_, ok = approvalToolName("tool:") // empty tool name → no voucher
	assert.False(t, ok)
	_, ok = approvalToolName("tool:   ") // whitespace-only → no voucher
	assert.False(t, ok)
}

// TestInvokeAdapterStampsApprovalVoucher proves the pure-HTTP adapter stamps the X-Ctxmesh-Approval
// header from the context onto the outbound /invoke (so the SDK can relay it), and omits it when none.
func TestInvokeAdapterStampsApprovalVoucher(t *testing.T) {
	var gotVoucher string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVoucher = r.Header.Get(hdrApproval)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	adapter := NewInvokeAdapter(InvokeAdapterConfig{Timeout: 5 * time.Second})

	// With a voucher on the context → stamped.
	ctx := contextWithApprovalVoucher(context.Background(), "VOUCHER-TOKEN")
	_, _, err := adapter.Invoke(ctx, srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "VOUCHER-TOKEN", gotVoucher, "the adapter stamps the approval voucher from the context")

	// Without one → no header.
	gotVoucher = "sentinel"
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "", gotVoucher, "a run with no granted require-approval tool stamps no voucher header")
}
