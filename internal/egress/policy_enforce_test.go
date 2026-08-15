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

package egress

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// policyHarness wires a Proxy over a mock upstream + always-resolving OBO resolver with a hot-swap
// PolicyHolder, so the tests exercise the WIRE enforcement (M82.2, ADR 0074 §2/§5). The route is a
// single ServerName ("weather") that fronts MANY tools — the per-tool decision is driven by the wire
// params.name, not the route segment.
type policyHarness struct {
	proxy  *Proxy
	signer *runcap.Signer
	priv   ed25519.PrivateKey
	holder *PolicyHolder
	up     *upstream
}

func newPolicyHarness(t *testing.T, policy *ToolPolicy) *policyHarness {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	up := newUpstream(t)
	routes, err := ParseRouteTable(fmt.Appendf(nil, `[{"name":%q,"targetURL":%q,"oauth":true}]`, testServer, up.server.URL))
	require.NoError(t, err)

	holder := &PolicyHolder{}
	holder.Store(policy, "")

	proxy := NewProxy(ProxyConfig{
		Verifier:      runcap.NewVerifier(pub, testAudience, nil),
		Resolver:      &mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "FRESH-USER-TOKEN"}},
		Namespace:     testNS,
		ExpectedAgent: testAgent,
		Routes:        routes,
		Policy:        holder,
		Log:           logr.Discard(),
	})
	return &policyHarness{proxy: proxy, signer: runcap.NewSigner(priv, testAudience, nil), priv: priv, holder: holder, up: up}
}

func (h *policyHarness) mint(t *testing.T) string {
	t.Helper()
	tok, err := h.signer.Mint(runcap.MintRequest{User: "u-alice", Agent: testAgent, RunID: "run-1", TTL: 5 * time.Minute})
	require.NoError(t, err)
	return tok
}

// send drives one request with an arbitrary raw body through the sidecar with a valid capability.
func (h *policyHarness) send(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+testServer, strings.NewReader(body))
	req.Header.Set(runcap.HeaderName, h.mint(t))
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, req)
	return rec
}

// sendWithVoucher drives one request with a valid capability AND the given raw X-Ctxmesh-Approval
// voucher header (m82.4). The capability's run is "run-1" (see mint), so a voucher for "run-1" +
// matching tool is the happy path.
func (h *policyHarness) sendWithVoucher(t *testing.T, body, voucher string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+testServer, strings.NewReader(body))
	req.Header.Set(runcap.HeaderName, h.mint(t))
	req.Header.Set(runcap.ApprovalHeaderName, voucher)
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, req)
	return rec
}

// mintVoucher mints an approval voucher for {run, tool} with the harness's platform signer (the SAME
// key the sidecar's verifier holds).
func (h *policyHarness) mintVoucher(t *testing.T, run, tool string, ttl time.Duration) string {
	t.Helper()
	v, err := h.signer.MintApprovalVoucher(run, tool, ttl)
	require.NoError(t, err)
	return v
}

// toolCallBody builds a JSON-RPC tools/call body naming a tool (or empty/absent name variants).
func toolCallBody(name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, name)
}

// denyPolicy is a restrictive policy: default allow, but tool "danger" denied and "review"
// require-approval — so the route is restrictive (fail-closed regime engaged) while "safe" is allowed.
func denyPolicy() *ToolPolicy {
	return &ToolPolicy{
		Default: RuleAllow,
		Overrides: []ToolPolicyOverride{
			{Name: "danger", Rule: RuleDeny},
			{Name: "review", Rule: RuleRequireApproval},
		},
	}
}

// (a) A denied tool's tools/call → 403, not forwarded.
func TestPolicyDenyBlocks(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	rec := h.send(t, toolCallBody("danger"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 0, h.up.hits, "a denied tool must be unreachable through the sidecar")
}

// (b) An allowed tool → forwarded (and the OBO credential still injected — the enforcement path is
// upstream of resolution but does not disturb it).
func TestPolicyAllowForwards(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	rec := h.send(t, toolCallBody("safe"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, h.up.hits)
	assert.Equal(t, "Bearer FRESH-USER-TOKEN", h.up.gotAuth, "an allowed tool still gets the OBO credential injected")
}

// (c) On a restrictive route: a batch array body is rejected — the §5 smuggling bypass closed.
func TestPolicyBatchRejected(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	// A batch that hides the denied "danger" call among allowed ones.
	batch := fmt.Sprintf(`[%s,%s]`, toolCallBody("safe"), toolCallBody("danger"))
	rec := h.send(t, batch)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "batch_not_allowed")
	assert.Equal(t, 0, h.up.hits, "a batch must never be forwarded on a restrictive route")

	// Leading-whitespace batch is still detected structurally.
	rec = h.send(t, "  \n\t"+batch)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "batch_not_allowed")
	assert.Equal(t, 0, h.up.hits)
}

// (c) A tools/call with an empty or absent params.name → 403 (fail closed), not forwarded.
func TestPolicyMissingToolNameFailsClosed(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())

	// Empty name.
	rec := h.send(t, toolCallBody(""))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 0, h.up.hits)

	// Absent params.name entirely.
	rec = h.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 0, h.up.hits)

	// Absent params object entirely.
	rec = h.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits)

	// A completely unparseable (non-batch) body also fails closed.
	rec = h.send(t, `{not json`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 0, h.up.hits)

	// An empty body fails closed too.
	rec = h.send(t, ``)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits)
}

// (c) A non-tools/call method NOT in the allow-list → 403; handshake/discovery methods → allowed.
func TestPolicyMethodAllowlist(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())

	// Allow-listed handshake/discovery/liveness pass without a tool decision.
	for _, m := range []string{"initialize", "tools/list", "ping", "notifications/initialized", "notifications/cancelled"} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, m)
		rec := h.send(t, body)
		assert.Equalf(t, http.StatusOK, rec.Code, "method %q should be allow-listed", m)
	}
	assert.Equal(t, 5, h.up.hits, "all allow-listed methods were forwarded")

	// A method that is neither tools/call nor allow-listed is rejected (it could be a tool-adjacent
	// action the policy can't reason about).
	rec := h.send(t, `{"jsonrpc":"2.0","id":1,"method":"resources/read"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 5, h.up.hits, "a non-allowlisted method is not forwarded")
}

// (d) A pure-allow route stays permissive/unchanged: a batch and an unparseable body are still
// forwarded verbatim (no new rejection) — no security need, must not break existing agents.
func TestPolicyPureAllowIsPermissive(t *testing.T) {
	// A pure-allow policy: default allow, an override that is ALSO allow (still not restrictive).
	allowPolicy := &ToolPolicy{Default: RuleAllow, Overrides: []ToolPolicyOverride{{Name: "danger", Rule: RuleAllow}}}
	h := newPolicyHarness(t, allowPolicy)

	// A tools/call is forwarded.
	rec := h.send(t, toolCallBody("danger"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// A batch is forwarded (NOT rejected) on a pure-allow route.
	rec = h.send(t, fmt.Sprintf(`[%s]`, toolCallBody("danger")))
	assert.Equal(t, http.StatusOK, rec.Code, "a pure-allow route does not reject batches")

	// An unparseable body is forwarded (NOT failed closed) on a pure-allow route.
	rec = h.send(t, `{not json`)
	assert.Equal(t, http.StatusOK, rec.Code, "a pure-allow route does not fail closed")

	assert.Equal(t, 3, h.up.hits, "all three were forwarded byte-for-byte on a pure-allow route")

	// A NIL policy (holder present, no policy) is likewise permissive.
	h.holder.Store(nil, "")
	rec = h.send(t, `[garbage`)
	assert.Equal(t, http.StatusOK, rec.Code, "a nil policy is permissive")
}

// (a-m82.4) A require-approval tool with NO voucher → typed 403 approval_required carrying {tool, run,
// server}, not forwarded.
func TestPolicyRequireApprovalNoVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	rec := h.send(t, toolCallBody("review"))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "require-approval without a voucher is not forwarded")

	var body errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "approval_required", body.Error, "the 403 is TYPED approval_required, not a bare tool_denied")
	assert.Equal(t, "review", body.Tool, "it names the tool the human must approve")
	assert.Equal(t, "run-1", body.Run, "it names the verified run so the SDK can pause on this exact decision point")
	assert.Equal(t, testServer, body.Server)
}

// (b-m82.4) A VALID voucher {matching run, matching tool, unexpired} → the require-approval tool is
// forwarded (and still gets the OBO credential injected).
func TestPolicyRequireApprovalValidVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	voucher := h.mintVoucher(t, "run-1", "review", 5*time.Minute)
	rec := h.sendWithVoucher(t, toolCallBody("review"), voucher)
	require.Equal(t, http.StatusOK, rec.Code, "a valid voucher forwards the require-approval tool")
	assert.Equal(t, 1, h.up.hits)
	assert.Equal(t, "Bearer FRESH-USER-TOKEN", h.up.gotAuth, "a voucher-approved tool still gets the OBO credential injected")
}

// (c-m82.4) A forged (bad-signature) voucher → typed 403 approval_required, not forwarded.
func TestPolicyRequireApprovalForgedVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	// A voucher signed by a DIFFERENT platform key (an attacker's key) for the right run + tool.
	_, otherPriv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	forgedSigner := runcap.NewSigner(otherPriv, testAudience, nil)
	forged, err := forgedSigner.MintApprovalVoucher("run-1", "review", 5*time.Minute)
	require.NoError(t, err)

	rec := h.sendWithVoucher(t, toolCallBody("review"), forged)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "a forged voucher is never forwarded")
	assert.Contains(t, rec.Body.String(), "approval_required")
}

// (c-m82.4) An EXPIRED voucher → typed 403 approval_required, not forwarded.
func TestPolicyRequireApprovalExpiredVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	// Mint with a signer over the SAME platform key but a clock far in the past, so the voucher is
	// already expired when the sidecar (real-time clock) verifies it.
	pastSigner := runcap.NewSigner(h.priv, testAudience, func() time.Time {
		return time.Now().Add(-time.Hour)
	})
	expired, err := pastSigner.MintApprovalVoucher("run-1", "review", time.Minute)
	require.NoError(t, err)

	rec := h.sendWithVoucher(t, toolCallBody("review"), expired)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "an expired voucher is never forwarded")
	assert.Contains(t, rec.Body.String(), "approval_required")
}

// (c-m82.4) A WRONG-TOOL voucher (for tool A, presented on a call to tool B) → 403, not forwarded.
func TestPolicyRequireApprovalWrongToolVoucher(t *testing.T) {
	// A policy where BOTH "review" and "audit" are require-approval, so tool B is itself gated.
	policy := &ToolPolicy{
		Default: RuleAllow,
		Overrides: []ToolPolicyOverride{
			{Name: "review", Rule: RuleRequireApproval},
			{Name: "audit", Rule: RuleRequireApproval},
		},
	}
	h := newPolicyHarness(t, policy)
	// Voucher approves "review" but the call is for "audit".
	voucher := h.mintVoucher(t, "run-1", "review", 5*time.Minute)
	rec := h.sendWithVoucher(t, toolCallBody("audit"), voucher)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "a voucher for tool A cannot approve tool B")
	assert.Contains(t, rec.Body.String(), "approval_required")
}

// (c-m82.4) A WRONG-RUN voucher (for a different run than the verified capability) → 403, not forwarded.
func TestPolicyRequireApprovalWrongRunVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	// The capability's run is "run-1"; this voucher is for "run-2".
	voucher := h.mintVoucher(t, "run-2", "review", 5*time.Minute)
	rec := h.sendWithVoucher(t, toolCallBody("review"), voucher)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "a voucher for run X cannot approve run Y")
	assert.Contains(t, rec.Body.String(), "approval_required")
}

// (d-m82.4) A RUN CAPABILITY presented as the approval voucher → 403 (the typ/audience discriminator):
// even a valid runcap for this run cannot self-approve a require-approval tool.
func TestPolicyRequireApprovalRuncapAsVoucher(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())
	// A perfectly valid runcap for run-1 — but it is NOT a voucher.
	runcapTok, err := h.signer.Mint(runcap.MintRequest{User: "u-alice", Agent: testAgent, RunID: "run-1", TTL: 5 * time.Minute})
	require.NoError(t, err)
	rec := h.sendWithVoucher(t, toolCallBody("review"), runcapTok)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits, "a runcap must never be redeemable as an approval voucher")
	assert.Contains(t, rec.Body.String(), "approval_required")
}

// (f) The WIRE params.name drives the per-tool decision (not the route segment): on the SAME route
// (server "weather"), tool "danger" is denied while tool "safe" is allowed. A default-deny policy
// with a per-tool allow proves the same in reverse.
func TestPolicyPerToolByWireName(t *testing.T) {
	h := newPolicyHarness(t, denyPolicy())

	// Same route, tool A denied.
	rec := h.send(t, toolCallBody("danger"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, h.up.hits)

	// Same route, tool B allowed and forwarded.
	rec = h.send(t, toolCallBody("safe"))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, h.up.hits, "the wire params.name, not the route segment, selected the rule")

	// A default-DENY policy with one allowed tool: the default denies everything else on the route.
	defaultDeny := &ToolPolicy{Default: RuleDeny, Overrides: []ToolPolicyOverride{{Name: "safe", Rule: RuleAllow}}}
	h2 := newPolicyHarness(t, defaultDeny)
	rec = h2.send(t, toolCallBody("safe"))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, h2.up.hits)

	rec = h2.send(t, toolCallBody("anything-else"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "the deny default blocks any non-allowlisted tool")
	assert.Equal(t, 1, h2.up.hits)
}

// Restricts() unit coverage — the gate that engages the fail-closed regime.
func TestPolicyRestrictsGate(t *testing.T) {
	assert.False(t, (*ToolPolicy)(nil).Restricts(), "nil policy is not restrictive")
	assert.False(t, (&ToolPolicy{}).Restricts(), "empty policy (default allow) is not restrictive")
	assert.False(t, (&ToolPolicy{Default: RuleAllow, Overrides: []ToolPolicyOverride{{Name: "x", Rule: RuleAllow}}}).Restricts(),
		"all-allow policy is not restrictive")
	assert.True(t, (&ToolPolicy{Default: RuleDeny}).Restricts(), "a non-allow default is restrictive")
	assert.True(t, (&ToolPolicy{Overrides: []ToolPolicyOverride{{Name: "x", Rule: RuleDeny}}}).Restricts(),
		"a deny override is restrictive")
	assert.True(t, (&ToolPolicy{Overrides: []ToolPolicyOverride{{Name: "x", Rule: RuleRequireApproval}}}).Restricts(),
		"a require-approval override is restrictive")
}
