package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// popBFF is an HTTP shell over the real runcap signer, the real single-use bind rule and the real proof
// verifier, so these tests exercise the protocol as the BFF enforces it (internal/bff/runcap_bind.go,
// runcap_pop.go). The BFF's own handlers are proven end to end by the delegate acceptance.
type popBFF struct {
	t        *testing.T
	signer   *runcap.Signer
	verifier *runcap.ProofVerifier
	bindCode int // 0 = serve the exchange; otherwise reply with this status

	mu       sync.Mutex
	boundTo  map[string]string // run id -> jkt
	binds    int
	accepted int
}

func newPopBFF(t *testing.T) (*popBFF, *httptest.Server) {
	t.Helper()
	_, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b := &popBFF{
		t: t, signer: runcap.NewSigner(priv, "", nil), verifier: runcap.NewProofVerifier(nil),
		boundTo: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runcapBindPath, b.bind)
	mux.HandleFunc("POST /api/internal/spawn", b.edge)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *popBFF) mint(t *testing.T, run string) string {
	t.Helper()
	tok, err := b.signer.Mint(runcap.MintRequest{User: "u", Agent: "a", RunID: run, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (b *popBFF) bind(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.binds++
	b.mu.Unlock()
	if b.bindCode != 0 {
		w.WriteHeader(b.bindCode)
		return
	}
	capab, err := b.signer.Verifier().Verify(r.Header.Get(runcap.HeaderName))
	if err != nil || capab.KeyThumbprint != "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		JKT string `json:"jkt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	b.mu.Lock()
	existing, taken := b.boundTo[capab.RunID]
	if !taken {
		b.boundTo[capab.RunID] = req.JKT
	}
	b.mu.Unlock()
	if taken && existing != req.JKT {
		w.WriteHeader(http.StatusConflict)
		return
	}
	bound, err := b.signer.Mint(runcap.MintRequest{
		User: capab.User, Agent: capab.Agent, RunID: capab.RunID,
		TTL: time.Until(capab.ExpiresAt), KeyThumbprint: req.JKT,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"capability": bound})
}

// edge requires proof-of-possession, as RUNCAP_REQUIRE_POP=true does.
func (b *popBFF) edge(w http.ResponseWriter, r *http.Request) {
	capab, err := b.signer.Verifier().Verify(r.Header.Get(runcap.HeaderName))
	if err != nil {
		http.Error(w, "invalid capability", http.StatusUnauthorized)
		return
	}
	if err := b.verifier.VerifyProof(capab, r.Header.Get(runcap.PoPHeaderName), r.Method,
		"http://"+r.Host+r.URL.Path); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	b.accepted++
	b.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func newTestBinder(t *testing.T, bffURL string) *runcapBinder {
	t.Helper()
	b, err := newRuncapBinder(bffURL, t.Logf)
	if err != nil || b == nil {
		t.Fatalf("binder: %v", err)
	}
	return b
}

func spend(t *testing.T, c *http.Client, url, tok string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/api/internal/spawn",
		strings.NewReader("{}"))
	req.Header.Set(runcap.HeaderName, tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The launcher exchanges a bearer capability once, then proves possession on every spend.
func TestRuncapProof_BindsOnceAndProvesEverySpend(t *testing.T) {
	bff, srv := newPopBFF(t)
	binder := newTestBinder(t, srv.URL)
	c := withRuncapProof(&http.Client{}, binder)
	bearer := bff.mint(t, "run-1")

	for i := range 3 {
		if code := spend(t, c, srv.URL, bearer); code != http.StatusOK {
			t.Fatalf("spend %d: HTTP %d, want 200", i, code)
		}
	}
	if bff.binds != 1 {
		t.Errorf("bound %d times, want once — the bound capability must be reused", bff.binds)
	}
	if bff.accepted != 3 {
		t.Errorf("accepted %d spends, want 3 — each needs a fresh single-use proof", bff.accepted)
	}
}

// A copy of the bearer capability is worthless once the launcher has bound it: another holder can
// neither bind it nor spend it.
func TestRuncapProof_ACopiedCapabilityCannotBeBoundOrSpent(t *testing.T) {
	bff, srv := newPopBFF(t)
	owner := newTestBinder(t, srv.URL)
	bearer := bff.mint(t, "run-2")
	if _, err := owner.Bind(context.Background(), bearer); err != nil {
		t.Fatal(err)
	}

	thief := newTestBinder(t, srv.URL)
	if _, err := thief.Bind(context.Background(), bearer); !errors.Is(err, errRuncapClaimed) {
		t.Fatalf("a second key binding the same capability: got %v, want errRuncapClaimed", err)
	}
	// The thief sends the bearer copy as is: the edge requires a proof it cannot make.
	if code := spend(t, &http.Client{}, srv.URL, bearer); code != http.StatusUnauthorized {
		t.Errorf("spending the bearer copy: HTTP %d, want 401", code)
	}
	// Nor can it spend the bound capability without the owner's key.
	bound, _ := owner.Bind(context.Background(), bearer)
	if code := spend(t, withRuncapProof(&http.Client{}, thief), srv.URL, bound); code != http.StatusUnauthorized {
		t.Errorf("spending the owner's bound capability from another key: HTTP %d, want 401", code)
	}
}

// A capability and its proofs go only to the BFF.
func TestRuncapProof_OtherHostsSeeTheRequestUntouched(t *testing.T) {
	bff, srv := newPopBFF(t)
	binder := newTestBinder(t, srv.URL)
	var gotCap, gotProof string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCap, gotProof = r.Header.Get(runcap.HeaderName), r.Header.Get(runcap.PoPHeaderName)
	}))
	t.Cleanup(other.Close)
	bearer := bff.mint(t, "run-3")

	spend(t, withRuncapProof(&http.Client{}, binder), other.URL, bearer)
	if gotCap != bearer || gotProof != "" {
		t.Errorf("another host received capability-changed=%v proof=%q; want the request untouched",
			gotCap != bearer, gotProof)
	}
	if bff.binds != 0 {
		t.Errorf("a request to another host triggered %d binds", bff.binds)
	}
}

// A BFF without the exchange leaves capabilities as bearer tokens, and is not asked on every request.
func TestRuncapProof_NoExchangeOfferedSendsBearer(t *testing.T) {
	bff, srv := newPopBFF(t)
	bff.bindCode = http.StatusNotFound
	binder := newTestBinder(t, srv.URL)
	bearer := bff.mint(t, "run-4")

	for range 3 {
		got, err := binder.Bind(context.Background(), bearer)
		if err != nil || got != bearer {
			t.Fatalf("Bind: got changed=%v err=%v; want the bearer back", got != bearer, err)
		}
	}
	if bff.binds != 1 {
		t.Errorf("asked a BFF without the exchange %d times in a minute, want once", bff.binds)
	}
}

// A capability someone else already bound (a peer relayed it) is sent as given, never re-bound.
func TestRuncapProof_AlreadyBoundCapabilityIsLeftAlone(t *testing.T) {
	bff, srv := newPopBFF(t)
	binder := newTestBinder(t, srv.URL)
	foreign, err := bff.signer.Mint(runcap.MintRequest{
		User: "u", Agent: "a", RunID: "run-5", TTL: time.Minute,
		KeyThumbprint: "someone-elses-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := binder.Bind(context.Background(), foreign)
	if err != nil || got != foreign {
		t.Fatalf("Bind on a bound capability: changed=%v err=%v; want it unchanged", got != foreign, err)
	}
	if bff.binds != 0 {
		t.Errorf("tried to re-bind a bound capability %d times", bff.binds)
	}
}

// The front door replaces a bearer capability with this launcher's bound one before the agent's code
// receives the request.
func TestBindInboundCapability_TheAgentNeverSeesTheBearer(t *testing.T) {
	bff, srv := newPopBFF(t)
	binder := newTestBinder(t, srv.URL)
	bearer := bff.mint(t, "run-fd")
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set(runcap.HeaderName, bearer)
	rec := httptest.NewRecorder()

	if !bindInboundCapability(context.Background(), rec, req, binder) {
		t.Fatalf("front door refused: %d %s", rec.Code, rec.Body.String())
	}
	got := req.Header.Get(runcap.HeaderName)
	u, err := runcap.InspectUnverified(got)
	if err != nil || u.KeyThumbprint != binder.signer.Thumbprint() {
		t.Fatalf("the agent would receive changed=%v bound-to-us=%v; want this launcher's bound capability",
			got != bearer, err == nil && u.KeyThumbprint == binder.signer.Thumbprint())
	}
}

// If someone bound the capability before it reached this agent, it has leaked: the invoke is refused
// rather than run with a capability this launcher cannot spend.
func TestBindInboundCapability_AClaimedCapabilityIsRefused(t *testing.T) {
	bff, srv := newPopBFF(t)
	bearer := bff.mint(t, "run-claimed")
	if _, err := newTestBinder(t, srv.URL).Bind(context.Background(), bearer); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set(runcap.HeaderName, bearer)
	rec := httptest.NewRecorder()

	if bindInboundCapability(context.Background(), rec, req, newTestBinder(t, srv.URL)) {
		t.Fatal("a capability bound by another holder must not be passed to the agent")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("HTTP %d, want 401", rec.Code)
	}
}

// An AMP callee receives the caller's capability already bound to the caller's key. It cannot spend it
// at a BFF edge: every edge acts as the capability's run, so allowing it would let the callee act as
// the caller (a confused deputy).
func TestRuncapProof_AnAMPCalleeCannotSpendItsCallersCapability(t *testing.T) {
	bff, srv := newPopBFF(t)
	caller := newTestBinder(t, srv.URL)
	callerCap, err := caller.Bind(context.Background(), bff.mint(t, "run-amp"))
	if err != nil {
		t.Fatal(err)
	}
	callee := newTestBinder(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set(runcap.HeaderName, callerCap)
	if !bindInboundCapability(context.Background(), httptest.NewRecorder(), req, callee) ||
		req.Header.Get(runcap.HeaderName) != callerCap {
		t.Fatal("the callee's front door must pass a relayed bound capability through untouched")
	}
	if code := spend(t, withRuncapProof(&http.Client{}, callee), srv.URL, callerCap); code != http.StatusUnauthorized {
		t.Errorf("the callee spending its caller's capability: HTTP %d, want 401", code)
	}
}

// A BFF that cannot be reached is asked once per back-off, not once per invoke.
func TestRuncapBinder_UnreachableBFFBacksOff(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // drop the connection: a transport error, as a blocking network policy gives
	}))
	t.Cleanup(srv.Close)
	issuer, _ := newPopBFF(t)
	binder := newTestBinder(t, srv.URL)
	bearer := issuer.mint(t, "run-unreachable")

	for i := range 3 {
		if got, err := binder.Bind(context.Background(), bearer); err != nil || got != bearer {
			t.Fatalf("Bind %d: changed=%v err=%v; want the bearer back", i, got != bearer, err)
		}
	}
	if attempts != 1 {
		t.Errorf("tried an unreachable BFF %d times, want once per back-off", attempts)
	}
}
