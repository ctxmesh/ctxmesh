package bff

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// memBindStore is the in-memory twin of the Valkey bind store: single-use, first-writer-wins.
type memBindStore struct {
	mu    sync.Mutex
	bound map[string]string
}

func newMemBindStore() *memBindStore { return &memBindStore{bound: map[string]string{}} }

func (m *memBindStore) Bind(_ context.Context, runID, jkt string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.bound[runID]; ok {
		return existing, false, nil
	}
	m.bound[runID] = jkt
	return jkt, true, nil
}

func newBindServer(t *testing.T) (*Server, *runcap.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	return &Server{
		capabilitySigner: signer,
		runStore:         run.NewMemStore(),
		runcapBind:       newMemBindStore(),
		log:              logr.Discard(),
	}, signer
}

func postBind(t *testing.T, s *Server, capToken, jkt string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(BindRuncapRequest{JKT: jkt})
	req := httptest.NewRequest(http.MethodPost, "/api/internal/runcap/bind", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func bearerCap(t *testing.T, signer *runcap.Signer, runID string) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{
		User: "uhash-alice", Agent: "team-ns/supervisor", RunID: runID, TTL: time.Hour,
	})
	require.NoError(t, err)
	return tok
}

// The exchange: a bearer capability comes back bound to the caller's key, and from then on a proof is
// required — which is what makes a copy of the original worthless.
func TestBindRuncap_ExchangesABearerCapabilityForABoundOne(t *testing.T) {
	s, signer := newBindServer(t)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)

	rec := postBind(t, s, bearerCap(t, signer, "run-b1"), holder.Thumbprint())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp BindRuncapResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	bound, err := signer.Verifier().Verify(resp.Capability)
	require.NoError(t, err)

	assert.Equal(t, holder.Thumbprint(), bound.KeyThumbprint, "the returned capability names the caller's key")
	assert.Equal(t, "run-b1", bound.RunID, "and is otherwise the same capability")
	assert.Equal(t, "team-ns/supervisor", bound.Agent)

	proof, err := holder.Proof("POST", "http://bff/api/internal/spawn")
	require.NoError(t, err)
	require.NoError(t, runcap.NewProofVerifier(nil).
		VerifyProof(bound, proof, "POST", "http://bff/api/internal/spawn"))
}

// THE SECURITY ARGUMENT: binding is SINGLE-USE. Anyone holding the bearer token could bind it to their
// own key, so the first bind must win — an attacker who obtains the token afterwards finds it already
// bound to a key they do not have.
func TestBindRuncap_IsSingleUse(t *testing.T) {
	s, signer := newBindServer(t)
	legit, err := runcap.NewProofSigner()
	require.NoError(t, err)
	attacker, err := runcap.NewProofSigner()
	require.NoError(t, err)

	token := bearerCap(t, signer, "run-b2")
	require.Equal(t, http.StatusOK, postBind(t, s, token, legit.Thumbprint()).Code)

	stolen := postBind(t, s, token, attacker.Thumbprint())
	assert.Equal(t, http.StatusConflict, stolen.Code,
		"a second holder must not be able to bind the same capability to its own key")
	assert.NotContains(t, stolen.Body.String(), attacker.Thumbprint())
}

// The same holder retrying — a restart, a retry after a timeout — is idempotent, not a lockout. A bind
// that failed to reach the caller must be repeatable or a pod restart would strand the run.
func TestBindRuncap_SameKeyIsIdempotent(t *testing.T) {
	s, signer := newBindServer(t)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)

	token := bearerCap(t, signer, "run-b3")
	require.Equal(t, http.StatusOK, postBind(t, s, token, holder.Thumbprint()).Code)
	require.Equal(t, http.StatusOK, postBind(t, s, token, holder.Thumbprint()).Code,
		"re-binding the SAME key must succeed — a lost response must not strand the run")
}

// Binding narrows WHO can spend a capability; it must never extend HOW LONG.
func TestBindRuncap_DoesNotExtendTheCapabilityLifetime(t *testing.T) {
	s, signer := newBindServer(t)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)

	short, err := signer.Mint(runcap.MintRequest{
		User: "u", Agent: "a", RunID: "run-b4", TTL: 2 * time.Minute,
	})
	require.NoError(t, err)
	original, err := signer.Verifier().Verify(short)
	require.NoError(t, err)

	rec := postBind(t, s, short, holder.Thumbprint())
	require.Equal(t, http.StatusOK, rec.Code)
	var resp BindRuncapResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	bound, err := signer.Verifier().Verify(resp.Capability)
	require.NoError(t, err)

	assert.False(t, bound.ExpiresAt.After(original.ExpiresAt.Add(2*time.Second)),
		"the bound capability inherits the original's remaining life, never a fresh full TTL")
}

// An ALREADY-bound capability cannot be re-bound: that would be a downgrade path, letting a holder swap
// in a new key.
func TestBindRuncap_RefusesToRebindABoundCapability(t *testing.T) {
	s, signer := newBindServer(t)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)

	rec := postBind(t, s, bearerCap(t, signer, "run-b5"), holder.Thumbprint())
	require.Equal(t, http.StatusOK, rec.Code)
	var resp BindRuncapResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	again := postBind(t, s, resp.Capability, holder.Thumbprint())
	assert.Equal(t, http.StatusConflict, again.Code, "a bound capability is not re-bindable")
}

// Fail-closed on the inputs the exchange depends on.
func TestBindRuncap_RejectsBadRequests(t *testing.T) {
	s, signer := newBindServer(t)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, postBind(t, s, "", holder.Thumbprint()).Code, "no capability")
	assert.Equal(t, http.StatusUnauthorized,
		postBind(t, s, "forged.token.here", holder.Thumbprint()).Code, "unverifiable capability")
	assert.Equal(t, http.StatusBadRequest, postBind(t, s, bearerCap(t, signer, "run-b6"), "").Code,
		"no jkt ⇒ nothing to bind to")
}

// Without a bind store the exchange is not offered at all — an exchange that could not be made
// single-use would be a downgrade path dressed as a feature.
func TestBindRuncap_RouteAbsentWithoutABindStore(t *testing.T) {
	s, signer := newBindServer(t)
	s.runcapBind = nil

	mux := http.NewServeMux()
	s.registerRuncapBindRoute(mux)
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/api/internal/runcap/bind", nil))
	assert.Empty(t, pattern, "no bind store ⇒ no exchange edge")
	_ = signer
}
