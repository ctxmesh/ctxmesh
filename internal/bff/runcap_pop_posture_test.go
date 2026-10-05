package bff

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

type failingSpender struct{}

func (failingSpender) Spend(context.Context, string, time.Duration) error {
	return errors.New("dial tcp: connection refused")
}

func boundSpendRequest(t *testing.T, signer *runcap.Signer, holder *runcap.ProofSigner) *http.Request {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{
		User: "u", Agent: "a", RunID: "run-p", TTL: time.Minute,
		KeyThumbprint: holder.Thumbprint(),
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://bff/api/internal/spawn", nil)
	req.Header.Set(runcap.HeaderName, tok)
	proof, err := holder.Proof(http.MethodPost, "http://bff/api/internal/spawn")
	require.NoError(t, err)
	req.Header.Set(runcap.PoPHeaderName, proof)
	return req
}

// An unreachable replay set still refuses the spend — but as an outage the caller can retry, not as a
// bad proof.
func TestVerifyRuncapWithProof_ReplaySetDownIsAnOutageNotARejection(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)
	s := &Server{capabilitySigner: signer, proofSpender: failingSpender{}, log: logr.Discard()}

	_, verr := s.verifyRuncapWithProof(boundSpendRequest(t, signer, holder))
	require.Error(t, verr, "an unchecked proof must never be accepted")
	rec := httptest.NewRecorder()
	writeRuncapError(rec, verr)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// A replayed proof is still a rejection (401), not an outage.
func TestVerifyRuncapWithProof_ReplayIsStillARejection(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	holder, err := runcap.NewProofSigner()
	require.NoError(t, err)
	s := &Server{capabilitySigner: signer, log: logr.Discard()}

	req := boundSpendRequest(t, signer, holder)
	_, err = s.verifyRuncapWithProof(req)
	require.NoError(t, err)
	_, verr := s.verifyRuncapWithProof(req)
	require.Error(t, verr, "the same proof twice must be refused")
	rec := httptest.NewRecorder()
	writeRuncapError(rec, verr)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// The async edge copies binding headers onto the bus; the capability and its proof are not among them.
func TestAsyncPublishHeaders_DropCapabilityAndProof(t *testing.T) {
	in := http.Header{}
	in.Set(runcap.HeaderName, "cap")
	in.Set(runcap.PoPHeaderName, "proof")
	in.Set("Ce-Type", "x")
	out := asyncPublishHeaders(in, "ns", "reg")
	_, hasCap := out[http.CanonicalHeaderKey(runcap.HeaderName)]
	_, hasProof := out[http.CanonicalHeaderKey(runcap.PoPHeaderName)]
	assert.False(t, hasCap)
	assert.False(t, hasProof)
	assert.Equal(t, "x", out["Ce-Type"])
}
