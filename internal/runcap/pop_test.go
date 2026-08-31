package runcap_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/runcap"
)

const popAud = "credential-plane"

// boundCapability mints a capability bound to the signer's key — the sender-constrained shape.
func boundCapability(t *testing.T, signer *runcap.ProofSigner) runcap.Capability {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := runcap.NewSigner(priv, popAud, nil)

	tok, err := s.Mint(runcap.MintRequest{
		User: "uhash-alice", Agent: "supervisor", RunID: "run-1", TTL: time.Hour,
		KeyThumbprint: signer.Thumbprint(),
	})
	require.NoError(t, err)
	capab, err := s.Verifier().Verify(tok)
	require.NoError(t, err)
	return capab
}

// THE PROPERTY (M142.5): a valid capability plus a matching proof is accepted.
func TestPoP_ValidProofIsAccepted(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)

	proof, err := signer.Proof("POST", "http://bff/api/internal/spawn")
	require.NoError(t, err)

	require.NoError(t, runcap.NewProofVerifier(nil).
		VerifyProof(capab, proof, "POST", "http://bff/api/internal/spawn"))
	assert.NotEmpty(t, capab.KeyThumbprint, "the capability names the key it is bound to")
}

// THE POINT OF THE WHOLE MECHANISM: a leaked capability is useless. An attacker with the token but not
// the private key cannot produce a proof that verifies against its cnf.
func TestPoP_LeakedCapabilityIsUselessWithoutTheKey(t *testing.T) {
	victim, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, victim)

	// The attacker has the capability string (it verifies fine — that was always true) and their own key.
	attacker, err := runcap.NewProofSigner()
	require.NoError(t, err)
	forged, err := attacker.Proof("POST", "http://bff/api/internal/spawn")
	require.NoError(t, err)

	err = runcap.NewProofVerifier(nil).VerifyProof(capab, forged, "POST", "http://bff/api/internal/spawn")
	assert.ErrorIs(t, err, runcap.ErrProofKeyMismatch,
		"possession of the token must not be authority — the key it names is what authorizes")
}

// A proof captured from one request cannot be replayed onto another. Without this, a proof lifted from a
// harmless GET would authorize a dangerous POST inside its freshness window.
func TestPoP_ProofIsBoundToTheRequest(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)

	proof, err := signer.Proof("GET", "http://bff/api/internal/runs/abc")
	require.NoError(t, err)

	v := runcap.NewProofVerifier(nil)
	assert.ErrorIs(t, v.VerifyProof(capab, proof, "POST", "http://bff/api/internal/runs/abc"),
		runcap.ErrProofWrongRequest, "a proof for GET must not authorize POST")
	assert.ErrorIs(t, v.VerifyProof(capab, proof, "GET", "http://bff/api/internal/spawn"),
		runcap.ErrProofWrongRequest, "a proof for one path must not authorize another")
}

// The query string is deliberately NOT bound (RFC 9449 §4.2): it routinely carries values a caller may
// vary, and binding it would fail proofs for reasons unrelated to authenticity.
func TestPoP_QueryStringIsNotPartOfTheBinding(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)

	proof, err := signer.Proof("GET", "http://bff/api/internal/discover?topK=5")
	require.NoError(t, err)
	require.NoError(t, runcap.NewProofVerifier(nil).
		VerifyProof(capab, proof, "GET", "http://bff/api/internal/discover?topK=9"))
}

// A proof is single-use: capturing one in flight must not let it be spent again.
func TestPoP_ProofCannotBeReplayed(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)

	proof, err := signer.Proof("POST", "http://bff/api/internal/spawn")
	require.NoError(t, err)

	v := runcap.NewProofVerifier(nil)
	require.NoError(t, v.VerifyProof(capab, proof, "POST", "http://bff/api/internal/spawn"))
	assert.ErrorIs(t, v.VerifyProof(capab, proof, "POST", "http://bff/api/internal/spawn"),
		runcap.ErrProofReplayed, "the same proof must not be spendable twice")
}

// A capability with no cnf is a legacy BEARER token. The verifier reports that distinctly rather than
// passing or failing silently, because what to do about it is a rollout posture the edge owns.
func TestPoP_LegacyBearerCapabilityIsReportedDistinctly(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := runcap.NewSigner(priv, popAud, nil)
	tok, err := s.Mint(runcap.MintRequest{User: "u", Agent: "a", RunID: "run-1", TTL: time.Hour})
	require.NoError(t, err)
	capab, err := s.Verifier().Verify(tok)
	require.NoError(t, err)

	assert.Empty(t, capab.KeyThumbprint)
	assert.ErrorIs(t, runcap.NewProofVerifier(nil).VerifyProof(capab, "x.y.z", "POST", "http://bff/x"),
		runcap.ErrNotSenderConstrained,
		"an un-bound capability is a posture decision for the edge, not a silent accept or reject")
}

// A stale proof is refused: its whole value is that a captured one expires before it can be used.
func TestPoP_StaleProofIsRefused(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)

	proof, err := signer.Proof("POST", "http://bff/api/internal/spawn")
	require.NoError(t, err)

	// Ten minutes later, the proof is long past its window.
	v := runcap.NewProofVerifier(func() time.Time { return time.Now().Add(10 * time.Minute) })
	assert.ErrorIs(t, v.VerifyProof(capab, proof, "POST", "http://bff/api/internal/spawn"),
		runcap.ErrProofExpired)
}

// A garbled proof, or one typed as something else, is malformed — never accepted on the strength of the
// capability alone.
func TestPoP_MalformedProofIsRefused(t *testing.T) {
	signer, err := runcap.NewProofSigner()
	require.NoError(t, err)
	capab := boundCapability(t, signer)
	v := runcap.NewProofVerifier(nil)

	for _, bad := range []string{"", "not-a-jws", "a.b", "a.b.c.d", "!!!.???.###"} {
		assert.ErrorIs(t, v.VerifyProof(capab, bad, "POST", "http://bff/x"), runcap.ErrProofMalformed,
			"proof %q must be refused as malformed", bad)
	}
}

// The thumbprint is the RFC 8037 canonical form. If it were computed any other way the two sides would
// disagree and binding would silently never match.
func TestPoP_ThumbprintIsStableAndKeySpecific(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	assert.Equal(t, runcap.Thumbprint(pub1), runcap.Thumbprint(pub1), "stable for one key")
	assert.NotEqual(t, runcap.Thumbprint(pub1), runcap.Thumbprint(pub2), "distinct across keys")
	assert.NotEmpty(t, runcap.Thumbprint(pub1))
}

// The proof rides a header; a request without one carries no proof to check.
func TestPoP_HeaderNameIsDistinctFromTheCapabilityHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "http://bff/x", nil)
	assert.Empty(t, r.Header.Get(runcap.PoPHeaderName))
	assert.NotEqual(t, runcap.HeaderName, runcap.PoPHeaderName,
		"the proof and the capability must travel in separate headers, or one could be mistaken for the other")
}
